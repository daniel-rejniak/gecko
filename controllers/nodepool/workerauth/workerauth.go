// Package workerauth builds the worker-only registry configuration for the
// credential-helper POC. It replaces all worker auth entries with helper
// references; customer additional-pull-secret delivery is outside this POC.
// HostedCluster and control-plane pull-secret configuration are unchanged.
package workerauth

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GlobalPullSecretLabel controls worker eligibility for the pull-secret syncer.
// Helper-managed workers must not have their static authfiles replaced by it.
const GlobalPullSecretLabel = "hypershift.openshift.io/nodepool-globalps-enabled"

const hostsFile = "/etc/containers/gcp-gar-credential-helper-hosts"

//go:embed assets/docker-credential-gcp
var helperScript string

var (
	dnsLabel   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\z`)
	sourceHost = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}\z`)
	garMirror  = regexp.MustCompile(`^[a-z][a-z0-9-]*-docker\.pkg\.dev/[a-z][a-z0-9-]{4,28}[a-z0-9]/[a-z][a-z0-9_-]{0,62}\z`)
)

// ParseMirrors parses SOURCE_REGISTRY=GAR_HOST/PROJECT/REPOSITORY mappings.
// No values means worker-helper delivery is disabled. A source may appear only
// once, including when repeated with the same destination.
func ParseMirrors(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	mirrors := make(map[string]string, len(values))
	for _, value := range values {
		if strings.Count(value, "=") != 1 {
			return nil, fmt.Errorf("worker registry mirror must be SOURCE_REGISTRY=GAR_HOST/PROJECT/REPOSITORY")
		}
		source, destination, _ := strings.Cut(value, "=")
		if _, exists := mirrors[source]; exists {
			return nil, fmt.Errorf("duplicate worker registry mirror source %q", source)
		}
		mirrors[source] = destination
	}
	if err := validateMirrors(mirrors); err != nil {
		return nil, err
	}
	return mirrors, nil
}

// validateMirrors accepts upstream hostnames mapped to exact GAR repositories,
// without schemes, ports, tags, digests or additional path segments. An empty
// map is valid and represents disabled worker-helper delivery.
func validateMirrors(mirrors map[string]string) error {
	for _, source := range sortedSources(mirrors) {
		if !sourceHost.MatchString(source) || strings.HasSuffix(source, ".pkg.dev") {
			return fmt.Errorf("worker registry mirror source %q must be an upstream registry hostname", source)
		}
		if !garMirror.MatchString(mirrors[source]) {
			return fmt.Errorf("worker registry mirror destination %q must be a GAR hostname/project/repository without a scheme", mirrors[source])
		}
	}
	return nil
}

// Build creates a ConfigMap containing a helper MachineConfig and native
// ImageDigestMirrorSet. Reference it from NodePool.spec.config at creation so
// the helper and authfiles are
// available before machine-config-daemon-pull.service's first Podman pull.
// The ConfigMap is managed through the NodePool's Gecko transport group; no
// cross-cluster owner reference is attached to the private API NodePool.
func Build(namespace, nodePoolName string, mirrors map[string]string) (*corev1.ConfigMap, error) {
	if !dnsLabel.MatchString(namespace) || !dnsLabel.MatchString(nodePoolName) {
		return nil, fmt.Errorf("worker registry namespace and NodePool name must be DNS labels of at most 63 characters")
	}
	if len(mirrors) == 0 {
		return nil, fmt.Errorf("at least one worker registry mirror is required")
	}
	if err := validateMirrors(mirrors); err != nil {
		return nil, err
	}

	helperHosts := map[string]string{}
	for _, destination := range mirrors {
		host, _, _ := strings.Cut(destination, "/")
		helperHosts[host] = "gcp"
	}
	hosts := make([]string, 0, len(helperHosts))
	for host := range helperHosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	auth, err := json.Marshal(map[string]any{
		"auths": map[string]string{}, "credHelpers": helperHosts,
	})
	if err != nil {
		return nil, fmt.Errorf("worker registry config: marshal authfile: %w", err)
	}
	authContent := string(auth) + "\n"

	files := []map[string]any{
		ignitionFile("/usr/local/bin/docker-credential-gcp", helperScript, 0o755),
		ignitionFile(hostsFile, strings.Join(hosts, "\n")+"\n", 0o644),
		ignitionFile("/var/lib/kubelet/config.json", authContent, 0o600),
		ignitionFile("/etc/containers/gar-auth.json", authContent, 0o600),
		ignitionFile("/etc/crio/crio.conf.d/99-gcp-worker-identity.conf",
			"[crio.image]\nglobal_auth_file = \"/etc/containers/gar-auth.json\"\n"+
				"pause_image_auth_file = \"/etc/containers/gar-auth.json\"\n", 0o644),
	}
	services := []string{
		"machine-config-daemon-pull.service", "machine-config-daemon-firstboot.service",
		"rpm-ostreed.service", "crio.service",
	}
	units := make([]map[string]any, 0, len(services))
	for _, service := range services {
		units = append(units, map[string]any{
			"name": service,
			"dropins": []map[string]string{{
				"name":     "99-gcp-worker-identity-path.conf",
				"contents": "[Service]\nEnvironment=\"PATH=/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin\"\n",
			}},
		})
	}
	machineConfig := map[string]any{
		"apiVersion": "machineconfiguration.openshift.io/v1", "kind": "MachineConfig",
		"metadata": map[string]any{
			"name":   "99-zz-gcp-worker-identity-" + nodePoolName,
			"labels": map[string]string{"machineconfiguration.openshift.io/role": "worker"},
		},
		"spec": map[string]any{"config": map[string]any{
			"ignition": map[string]string{"version": "3.2.0"},
			"storage":  map[string]any{"files": files},
			"systemd":  map[string]any{"units": units},
		}},
	}
	var documents []string
	for _, manifest := range []map[string]any{machineConfig, imageDigestMirrorSet(nodePoolName, mirrors)} {
		raw, err := json.Marshal(manifest)
		if err != nil {
			return nil, fmt.Errorf("worker registry config: marshal %s: %w", manifest["kind"], err)
		}
		documents = append(documents, string(raw))
	}

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "gcp-worker-identity-" + nodePoolName, Namespace: namespace,
		},
		Data: map[string]string{"config": strings.Join(documents, "\n---\n")},
	}, nil
}

func ignitionFile(path, content string, mode int) map[string]any {
	return map[string]any{
		"path": path, "mode": mode, "overwrite": true,
		"user":     map[string]string{"name": "root"},
		"group":    map[string]string{"name": "root"},
		"contents": map[string]string{"source": "data:;base64," + base64.StdEncoding.EncodeToString([]byte(content))},
	}
}

func imageDigestMirrorSet(nodePoolName string, mirrors map[string]string) map[string]any {
	entries := make([]map[string]any, 0, len(mirrors))
	for _, source := range sortedSources(mirrors) {
		entries = append(entries, map[string]any{
			"source": source, "mirrors": []string{mirrors[source]},
		})
	}
	return map[string]any{
		"apiVersion": "config.openshift.io/v1", "kind": "ImageDigestMirrorSet",
		"metadata": map[string]string{"name": "gcp-worker-mirrors-" + nodePoolName},
		"spec":     map[string]any{"imageDigestMirrors": entries},
	}
}

func sortedSources(mirrors map[string]string) []string {
	sources := make([]string, 0, len(mirrors))
	for source := range mirrors {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	return sources
}

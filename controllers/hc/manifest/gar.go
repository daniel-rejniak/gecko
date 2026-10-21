package manifest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openshift-online/gecko/controllers/util/gar"
)

// buildGARResources keeps the original ExternalSecret intact and adds a separate,
// continuously refreshed GAR pull secret and its exact namespace identity grant.
func buildGARResources(input Input, namespace string) ([][]byte, error) {
	mc, configured := input.GAR.ForManagementCluster(input.ManagementClusterProjectID)
	if !configured {
		return nil, nil
	}
	objects := []map[string]any{
		{
			"apiVersion": "v1", "kind": "ServiceAccount",
			"metadata": map[string]any{
				"name": gar.ServiceAccountName, "namespace": namespace,
				"annotations": map[string]any{"iam.gke.io/gcp-service-account": mc.ServiceAccountEmail},
			},
		},
		{
			"apiVersion": "iam.cnrm.cloud.google.com/v1beta1", "kind": "IAMPolicyMember",
			"metadata": map[string]any{
				"name": "gar-token-workload-identity", "namespace": namespace,
				"annotations": map[string]any{"cnrm.cloud.google.com/project-id": input.ManagementClusterProjectID},
			},
			"spec": map[string]any{
				"member": fmt.Sprintf("serviceAccount:%s.svc.id.goog[%s/%s]", input.ManagementClusterProjectID, namespace, gar.ServiceAccountName),
				"role":   "roles/iam.workloadIdentityUser",
				"resourceRef": map[string]any{
					"apiVersion": "iam.cnrm.cloud.google.com/v1beta1", "kind": "IAMServiceAccount",
					"external": fmt.Sprintf("projects/%s/serviceAccounts/%s", input.ManagementClusterProjectID, mc.ServiceAccountEmail),
				},
			},
		},
		{
			"apiVersion": "generators.external-secrets.io/v1alpha1", "kind": "GCRAccessToken",
			"metadata": map[string]any{"name": gar.GeneratorName, "namespace": namespace},
			"spec": map[string]any{
				"projectID": strings.Split(input.GAR.RepositoryPrefix, "/")[1],
				"auth": map[string]any{"workloadIdentity": map[string]any{
					"clusterProjectID": input.ManagementClusterProjectID,
					"clusterName":      mc.ClusterName, "clusterLocation": mc.ClusterLocation,
					"serviceAccountRef": map[string]any{"name": gar.ServiceAccountName},
				}},
			},
		},
	}
	data := []any{map[string]any{
		"secretKey": "basePullSecret", "remoteRef": map[string]any{"key": input.PullSecretGCPKey},
	}}
	if input.GAR.RedhatSecretGCPKey != "" {
		data = append(data, map[string]any{
			"secretKey": "redhatPullSecret", "remoteRef": map[string]any{"key": input.GAR.RedhatSecretGCPKey},
		})
	}
	objects = append(objects, map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret",
		"metadata": map[string]any{"name": gar.PullSecretName, "namespace": namespace},
		"spec": map[string]any{
			"refreshPolicy": "Periodic", "refreshInterval": input.GAR.Interval(),
			"secretStoreRef": map[string]any{"kind": "ClusterSecretStore", "name": input.PullSecretStoreName},
			"data":           data,
			"dataFrom": []any{map[string]any{"sourceRef": map[string]any{"generatorRef": map[string]any{
				"apiVersion": "generators.external-secrets.io/v1alpha1", "kind": "GCRAccessToken", "name": gar.GeneratorName,
			}}}},
			"target": map[string]any{
				"name": gar.PullSecretName, "creationPolicy": "Owner", "deletionPolicy": "Retain",
				"template": map[string]any{
					"engineVersion": "v2", "mergePolicy": "Replace", "type": "kubernetes.io/dockerconfigjson",
					"data": map[string]any{".dockerconfigjson": garPullSecretTemplate(input.GAR)},
				},
			},
		},
	})
	manifests := make([][]byte, 0, len(objects))
	for _, obj := range objects {
		raw, err := json.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("marshal GAR resource: %w", err)
		}
		manifests = append(manifests, raw)
	}
	return manifests, nil
}

func garPullSecretTemplate(config *gar.Config) string {
	template := `{{- $base := mustFromJson .basePullSecret -}}
{{- if not (kindIs "map" $base.auths) -}}{{- fail "base pull secret must contain an auths object" -}}{{- end -}}
`
	if config.RedhatSecretGCPKey != "" {
		template += `{{- $redhat := mustFromJson .redhatPullSecret -}}
{{- if not (kindIs "map" $redhat.auths) -}}{{- fail "Red Hat pull secret must contain an auths object" -}}{{- end -}}
{{- $entry := index $redhat.auths "registry.redhat.io" -}}
{{- if not (kindIs "map" $entry) -}}{{- fail "Red Hat registry credentials are missing" -}}{{- end -}}
{{- if not $entry.auth -}}{{- fail "Red Hat registry auth is empty" -}}{{- end -}}
{{- $_ := set $base.auths "registry.redhat.io" $entry -}}
`
	}
	template += fmt.Sprintf(`{{- if or (not .username) (not .password) -}}{{- fail "generated GAR credentials are empty" -}}{{- end -}}
{{- $gar := dict "auth" (printf "%%s:%%s" .username .password | b64enc) -}}
{{- $_ := set $base.auths %q $gar -}}
{{- $base | mustToJson -}}`, config.RegistryHost)
	return template
}

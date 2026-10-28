// Package manifest provides the manifest builder for the nodepool controller.
package manifest

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"

	"github.com/openshift-online/gecko/controllers/nodepool/workerauth"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
)

const (
	// DefaultDiskSizeGB is the default disk size in GB for GCP node pool boot disks.
	DefaultDiskSizeGB = int64(100)
	// DefaultDiskType is the default disk type for GCP node pool boot disks.
	DefaultDiskType = "pd-ssd"
	// DefaultMachineType is the default GCP machine type for node pool instances.
	DefaultMachineType = "n2-standard-4"
)

// Input holds all parameters for building the NodePool manifests.
type Input struct {
	NodePoolID            string
	NodePoolName          string
	NodePoolGeneration    int64
	ClusterID             string
	ClusterName           string
	Replicas              int32
	MachineType           string // e.g. "n2-standard-4"
	GCPRegion             string
	Zone                  string // optional; falls back to GCPRegion+"-a"
	GCPSubnet             string
	DiskSizeGB            int64  // default: 100
	DiskType              string // default: "pd-ssd"
	ReleaseImage          string
	NodeLabels            map[string]string
	Taints                []privatev1.TaintSpec
	WorkerRegistryMirrors map[string]string
}

// Build constructs the NodePool and, when configured, its worker authentication ConfigMap.
func Build(input Input) ([][]byte, error) {
	// Apply defaults
	zone := input.Zone
	if zone == "" {
		zone = input.GCPRegion + "-a"
	}

	diskSizeGB := input.DiskSizeGB
	if diskSizeGB == 0 {
		diskSizeGB = DefaultDiskSizeGB
	}

	diskType := input.DiskType
	if diskType == "" {
		diskType = DefaultDiskType
	}

	machineType := input.MachineType
	if machineType == "" {
		machineType = DefaultMachineType
	}

	genStr := strconv.FormatInt(input.NodePoolGeneration, 10)
	namespace := fmt.Sprintf("clusters-%s", input.ClusterID)

	// Build the NodePool manifest as map[string]any
	nodePoolManifest := map[string]any{
		"apiVersion": "hypershift.openshift.io/v1beta1",
		"kind":       "NodePool",
		"metadata": map[string]any{
			"name":      input.NodePoolName,
			"namespace": namespace,
			"labels": map[string]any{
				"gcp.managed.openshift.io/cluster-id":  input.ClusterID,
				"gcp.managed.openshift.io/nodepool-id": input.NodePoolID,
				"gcp.managed.openshift.io/managed-by":  "nodepool-controller",
			},
			"annotations": map[string]any{
				"gcp.managed.openshift.io/generation": genStr,
			},
		},
		"spec": map[string]any{
			"clusterName": input.ClusterName,
			"replicas":    input.Replicas,
			"release": map[string]any{
				"image": input.ReleaseImage,
			},
			"arch": "amd64",
			"management": map[string]any{
				"autoRepair":  true,
				"upgradeType": "Replace",
				"replace": map[string]any{
					"strategy": "RollingUpdate",
					"rollingUpdate": map[string]any{
						"maxSurge":       1,
						"maxUnavailable": 0,
					},
				},
			},
			"platform": map[string]any{
				"type": "GCP",
				"gcp": map[string]any{
					"machineType": machineType,
					"zone":        zone,
					"subnet":      input.GCPSubnet,
					"bootDisk": map[string]any{
						"diskSizeGB": diskSizeGB,
						"diskType":   diskType,
					},
				},
			},
		},
	}

	spec := nodePoolManifest["spec"].(map[string]any)
	nodeLabels := maps.Clone(input.NodeLabels)
	var workerConfig []byte
	if len(input.WorkerRegistryMirrors) > 0 {
		// Karpenter-managed nodes independently match the Global Pull Secret syncer.
		if _, exists := nodeLabels["karpenter.sh/nodepool"]; exists {
			return nil, fmt.Errorf("nodepool manifest: worker registry authentication does not support karpenter.sh/nodepool labels")
		}
		config, err := workerauth.Build(namespace, input.NodePoolName, input.WorkerRegistryMirrors)
		if err != nil {
			return nil, fmt.Errorf("nodepool manifest: build worker authentication: %w", err)
		}
		config.Labels = map[string]string{
			"gcp.managed.openshift.io/cluster-id":  input.ClusterID,
			"gcp.managed.openshift.io/nodepool-id": input.NodePoolID,
			"gcp.managed.openshift.io/managed-by":  "nodepool-controller",
		}
		if nodeLabels == nil {
			nodeLabels = map[string]string{}
		}
		nodeLabels[workerauth.GlobalPullSecretLabel] = "false"
		spec["config"] = []map[string]string{{"name": config.Name}}
		workerConfig, err = json.Marshal(config)
		if err != nil {
			return nil, fmt.Errorf("nodepool manifest: marshal worker authentication ConfigMap: %w", err)
		}
	}
	if len(nodeLabels) > 0 {
		spec["nodeLabels"] = nodeLabels
	}
	if len(input.Taints) > 0 {
		spec["taints"] = input.Taints
	}

	rawBytes, err := json.Marshal(nodePoolManifest)
	if err != nil {
		return nil, fmt.Errorf("nodepool manifest: marshal NodePool: %w", err)
	}

	manifests := [][]byte{rawBytes}
	if workerConfig != nil {
		manifests = append(manifests, workerConfig)
	}
	return manifests, nil
}

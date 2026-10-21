// Package gar configures optional Artifact Registry credentials for hosted clusters.
package gar

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	PullSecretName     = "gar-pull-secret"
	GeneratorName      = "gar-token"
	ServiceAccountName = "gar-token-generator"
	RefreshAnnotation  = "hypershift.openshift.io/gcp-pull-secret-refresh"
	NodeLabel          = "hypershift.openshift.io/nodepool-globalps-enabled"
)

// Config is deployment configuration, shared by the HC and NodePool controllers.
// Only explicitly configured management clusters receive the additional resources.
type Config struct {
	RegistryHost              string                       `json:"registryHost"`
	RepositoryPrefix          string                       `json:"repositoryPrefix"`
	RefreshInterval           string                       `json:"refreshInterval,omitempty"`
	RedhatSecretGCPKey        string                       `json:"redhatSecretGCPKey,omitempty"`
	ControlPlaneOperatorImage string                       `json:"controlPlaneOperatorImage,omitempty"`
	SelectForHostedClusters   bool                         `json:"selectForHostedClusters"`
	ManagementClusters        map[string]ManagementCluster `json:"managementClusters"`
}

// ManagementCluster identifies the GKE cluster issuing tokens and its keyless
// Google service account. The map key is the selected management project ID.
type ManagementCluster struct {
	ClusterName         string `json:"clusterName"`
	ClusterLocation     string `json:"clusterLocation"`
	ServiceAccountEmail string `json:"serviceAccountEmail"`
}

// Load reads non-secret deployment configuration. An empty path disables GAR.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open GAR configuration: %w", err)
	}
	defer file.Close()
	var config Config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode GAR configuration: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

// Validate rejects incomplete configuration before controllers emit resources.
func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	if c.RegistryHost == "" || strings.ContainsAny(c.RegistryHost, "/:@?# \t\r\n") {
		return fmt.Errorf("GAR registryHost must be a registry hostname")
	}
	parts := strings.Split(c.RepositoryPrefix, "/")
	if len(parts) != 3 || parts[0] != c.RegistryHost || parts[1] == "" || parts[2] == "" || strings.ContainsAny(c.RepositoryPrefix, ":@?# \t\r\n") {
		return fmt.Errorf("GAR repositoryPrefix must be registryHost/project/repository")
	}
	interval, err := time.ParseDuration(c.Interval())
	if err != nil || interval <= 0 || interval >= time.Hour {
		return fmt.Errorf("GAR refreshInterval must be positive and shorter than one hour")
	}
	if len(c.ManagementClusters) == 0 {
		return fmt.Errorf("GAR managementClusters must contain at least one management project")
	}
	for projectID, mc := range c.ManagementClusters {
		if projectID == "" || mc.ClusterName == "" || mc.ClusterLocation == "" || !strings.HasSuffix(mc.ServiceAccountEmail, "@"+projectID+".iam.gserviceaccount.com") {
			return fmt.Errorf("GAR management cluster %q requires clusterName, clusterLocation and a serviceAccountEmail in that project", projectID)
		}
	}
	return nil
}

// Interval returns the generator refresh interval with sufficient time for consumers.
func (c *Config) Interval() string {
	if c.RefreshInterval == "" {
		return "20m"
	}
	return c.RefreshInterval
}

// ForManagementCluster returns settings only for an explicitly configured project.
func (c *Config) ForManagementCluster(projectID string) (ManagementCluster, bool) {
	if c == nil {
		return ManagementCluster{}, false
	}
	mc, ok := c.ManagementClusters[projectID]
	return mc, ok
}

// Selected reports whether consumers should use the new secret in this project.
func (c *Config) Selected(projectID string) bool {
	_, configured := c.ForManagementCluster(projectID)
	return configured && c.SelectForHostedClusters
}

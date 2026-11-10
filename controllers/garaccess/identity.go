package garaccess

import (
	"context"
	"fmt"
	"regexp"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/iam/v1"
)

var defaultComputeAccount = regexp.MustCompile(`^[1-9][0-9]*-compute@developer\.gserviceaccount\.com$`)

type identityLookup struct {
	compute *compute.Service
	iam     *iam.Service
}

func newIdentityLookup(ctx context.Context) (*identityLookup, error) {
	computeClient, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("create Compute client: %w", err)
	}
	iamClient, err := iam.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("create IAM client: %w", err)
	}
	return &identityLookup{compute: computeClient, iam: iamClient}, nil
}

func (l *identityLookup) customerMember(ctx context.Context, projectID string) (string, error) {
	project, err := l.compute.Projects.Get(projectID).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("get Compute project %s: %w", projectID, err)
	}
	// Use Google's authoritative default, rather than a user-supplied WIF project number.
	if project.Name != projectID || !defaultComputeAccount.MatchString(project.DefaultServiceAccount) {
		return "", fmt.Errorf("project %s has no valid default Compute service account", projectID)
	}
	name := fmt.Sprintf("projects/%s/serviceAccounts/%s", projectID, project.DefaultServiceAccount)
	account, err := l.iam.Projects.ServiceAccounts.Get(name).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("get default Compute account for %s: %w", projectID, err)
	}
	if account.ProjectId != projectID || account.Email != project.DefaultServiceAccount || account.Disabled {
		return "", fmt.Errorf("default Compute account for %s must belong to the project and be enabled", projectID)
	}
	return "serviceAccount:" + account.Email, nil
}

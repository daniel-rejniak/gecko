package garaccess

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/util/validation"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const retryPending = 5 * time.Second

// Config identifies the shared GAR repository and persistent regional IAM namespace.
type Config struct {
	Repository   string
	IAMNamespace string
}

func (c Config) Validate() error {
	if !regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/repositories/[^/]+$`).MatchString(c.Repository) {
		return fmt.Errorf("gar-repository must be projects/PROJECT/locations/LOCATION/repositories/REPOSITORY")
	}
	if c.IAMNamespace == "" || len(validation.IsDNS1123Label(c.IAMNamespace)) != 0 {
		return fmt.Errorf("iam-namespace must be an existing Kubernetes namespace name")
	}
	return nil
}

// Reconciler maintains one shared Reader grant per customer account and repository.
type Reconciler struct {
	reader   client.Reader
	kcc      client.Client
	identity *identityLookup
	config   Config
	log      logger.Logger
}

func New(ctx context.Context, log logger.Logger, reader client.Reader, kcc client.Client, config Config) (*Reconciler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	identity, err := newIdentityLookup(ctx)
	if err != nil {
		return nil, err
	}
	return &Reconciler{reader: reader, kcc: kcc, identity: identity, config: config, log: log}, nil
}

// Reconcile handles a project namespace, rather than an individual NodePool.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if req.Namespace == "" {
		return reconcile.Result{}, fmt.Errorf("customer project namespace is empty")
	}
	var nodePools privatev1.NodePoolList
	if err := r.reader.List(ctx, &nodePools, client.InNamespace(req.Namespace)); err != nil {
		return reconcile.Result{}, fmt.Errorf("list NodePools in project namespace %s: %w", req.Namespace, err)
	}

	// Count every object, including zero-replica pools and pools still deleting.
	r.log.Infof(ctx, "gar-access: project namespace %s has %d NodePools", req.Namespace, len(nodePools.Items))
	if len(nodePools.Items) == 0 {
		return r.removeGrants(ctx, req.Namespace)
	}

	// The POC requires every consumer in this logical namespace to target its GCP project.
	checked := map[string]bool{}
	for _, np := range nodePools.Items {
		if checked[np.Spec.ClusterID] {
			continue
		}
		var cluster privatev1.Cluster
		if err := r.reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: np.Spec.ClusterID}, &cluster); err != nil {
			return reconcile.Result{}, fmt.Errorf("get parent cluster for NodePool %s: %w", np.Name, err)
		}
		if cluster.Spec.Platform.GCP == nil || cluster.Spec.Platform.GCP.ProjectID != req.Namespace {
			return reconcile.Result{}, fmt.Errorf("cluster %s must target customer project %s", cluster.Name, req.Namespace)
		}
		checked[np.Spec.ClusterID] = true
	}
	member, err := r.identity.customerMember(ctx, req.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	return r.ensureGrant(ctx, req.Namespace, member)
}

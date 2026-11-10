package garaccess

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	readerRole   = "roles/artifactregistry.reader"
	managedLabel = "app.kubernetes.io/managed-by"
	managedBy    = "gecko-gar-access"
	projectLabel = "gcp.managed.openshift.io/gar-customer-project"
	stateKey     = "gcp.managed.openshift.io/gar-access-state"
)

// NewPolicyMember supplies the GVK for the regional Config Connector client/cache.
func NewPolicyMember() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "iam.cnrm.cloud.google.com", Version: "v1beta1", Kind: "IAMPolicyMember"})
	return obj
}

func desiredGrant(config Config, project, member string) *unstructured.Unstructured {
	obj := NewPolicyMember()
	// Stable per repository/account/role; no dependency on a particular Cluster or NodePool.
	hash := sha256.Sum256([]byte(config.Repository + "\n" + member + "\n" + readerRole))
	obj.SetName(fmt.Sprintf("gar-reader-%x", hash[:12]))
	obj.SetNamespace(config.IAMNamespace)
	obj.SetLabels(map[string]string{managedLabel: managedBy, projectLabel: project})
	obj.SetAnnotations(map[string]string{"cnrm.cloud.google.com/state-into-spec": "absent", stateKey: "active"})
	obj.Object["spec"] = map[string]interface{}{
		"member": member,
		"role":   readerRole,
		"resourceRef": map[string]interface{}{
			"apiVersion": "artifactregistry.cnrm.cloud.google.com/v1beta1",
			"kind":       "ArtifactRegistryRepository",
			"external":   config.Repository,
		},
	}
	return obj
}

func matchesGrant(existing, desired *unstructured.Unstructured) bool {
	return existing.GetName() == desired.GetName() && existing.GetNamespace() == desired.GetNamespace() &&
		existing.GetLabels()[managedLabel] == managedBy && existing.GetLabels()[projectLabel] == desired.GetLabels()[projectLabel] &&
		reflect.DeepEqual(existing.Object["spec"], desired.Object["spec"]) && len(existing.GetOwnerReferences()) == 0
}

// PolicyMemberToProject recovers project reconciliation on restart and KCC updates.
func (r *Reconciler) PolicyMemberToProject(_ context.Context, obj client.Object) []reconcile.Request {
	policy, ok := obj.(*unstructured.Unstructured)
	if !ok || policy.GetLabels()[managedLabel] != managedBy {
		return nil
	}
	repository, _, _ := unstructured.NestedString(policy.Object, "spec", "resourceRef", "external")
	project := policy.GetLabels()[projectLabel]
	if repository != r.config.Repository || project == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: project}}}
}

func (r *Reconciler) ensureGrant(ctx context.Context, project, member string) (reconcile.Result, error) {
	desired := desiredGrant(r.config, project, member)
	existing := NewPolicyMember()
	err := r.kcc.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := r.kcc.Create(ctx, desired); err != nil {
			return reconcile.Result{}, fmt.Errorf("create GAR Reader grant for %s: %w", project, err)
		}
		r.log.Infof(ctx, "gar-access: created %s/%s for %s", desired.GetNamespace(), desired.GetName(), project)
		return reconcile.Result{RequeueAfter: retryPending}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("get GAR Reader grant for %s: %w", project, err)
	}
	if !matchesGrant(existing, desired) {
		return reconcile.Result{}, fmt.Errorf("IAMPolicyMember %s does not match the Gecko-owned grant for %s", existing.GetName(), project)
	}
	if !existing.GetDeletionTimestamp().IsZero() {
		// KCC must finish revocation before the same grant can be recreated.
		return reconcile.Result{RequeueAfter: retryPending}, nil
	}
	if existing.GetAnnotations()[stateKey] != "active" {
		annotations := existing.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[stateKey] = "active"
		existing.SetAnnotations(annotations)
		if err := r.kcc.Update(ctx, existing); err != nil {
			return reconcile.Result{}, fmt.Errorf("reactivate GAR Reader grant for %s: %w", project, err)
		}
	}
	return reconcile.Result{}, nil
}

func (r *Reconciler) removeGrants(ctx context.Context, project string) (reconcile.Result, error) {
	policies := &unstructured.UnstructuredList{}
	policies.SetGroupVersionKind(schema.GroupVersionKind{Group: "iam.cnrm.cloud.google.com", Version: "v1beta1", Kind: "IAMPolicyMemberList"})
	if err := r.kcc.List(ctx, policies, client.InNamespace(r.config.IAMNamespace),
		client.MatchingLabels{managedLabel: managedBy, projectLabel: project}); err != nil {
		return reconcile.Result{}, fmt.Errorf("list GAR Reader grants for %s: %w", project, err)
	}
	for i := range policies.Items {
		policy := &policies.Items[i]
		if len(r.PolicyMemberToProject(ctx, policy)) == 0 {
			continue
		}
		member, _, _ := unstructured.NestedString(policy.Object, "spec", "member")
		desired := desiredGrant(r.config, project, member)
		if !matchesGrant(policy, desired) {
			return reconcile.Result{}, fmt.Errorf("refuse to delete unexpected IAMPolicyMember %s", policy.GetName())
		}
		if !policy.GetDeletionTimestamp().IsZero() {
			return reconcile.Result{RequeueAfter: retryPending}, nil
		}
		// Block new provisioning before the final inventory read. The NodePool gate
		// must use a direct API read and require this state to be active.
		if policy.GetAnnotations()[stateKey] != "draining" {
			annotations := policy.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[stateKey] = "draining"
			policy.SetAnnotations(annotations)
			if err := r.kcc.Update(ctx, policy); err != nil {
				return reconcile.Result{}, fmt.Errorf("mark GAR Reader grant draining for %s: %w", project, err)
			}
		}
		// Recheck the API immediately before deletion; inventory errors never mean zero.
		var nodePools privatev1.NodePoolList
		if err := r.reader.List(ctx, &nodePools, client.InNamespace(project)); err != nil {
			return reconcile.Result{}, fmt.Errorf("recheck NodePools for %s: %w", project, err)
		}
		if len(nodePools.Items) != 0 {
			return reconcile.Result{RequeueAfter: retryPending}, nil
		}
		uid, version := policy.GetUID(), policy.GetResourceVersion()
		if err := r.kcc.Delete(ctx, policy, client.Preconditions{UID: &uid, ResourceVersion: &version}); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("delete GAR Reader grant for %s: %w", project, err)
		}
		r.log.Infof(ctx, "gar-access: deleting %s/%s for %s", policy.GetNamespace(), policy.GetName(), project)
		return reconcile.Result{RequeueAfter: retryPending}, nil
	}
	return reconcile.Result{}, nil
}

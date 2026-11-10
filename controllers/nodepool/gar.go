package nodepool

import (
	"context"
	"fmt"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r *Reconciler) garAccessReady(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster) (bool, error) {
	if r.GARAccess == nil {
		return true, nil
	}
	var checkErr error
	if cluster.Spec.Platform.GCP == nil || cluster.Spec.Platform.GCP.ProjectID != np.Namespace {
		checkErr = fmt.Errorf("cluster must target customer project %s", np.Namespace)
	} else {
		checkErr = r.GARAccess.Check(ctx, np.Namespace)
	}
	condition := metav1.Condition{
		Type:               "NodePoolGARAccessReady",
		Status:             metav1.ConditionTrue,
		Reason:             "GARAccessReady",
		Message:            "Config Connector reconciled the customer GAR Reader grant",
		ObservedGeneration: np.Generation,
	}
	changed := false
	if checkErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "GARAccessNotReady"
		condition.Message = checkErr.Error()
		changed = setWaitingNPConditions(np, condition.Reason, condition.Message)
		r.log.Warnf(ctx, "nodepool %s: waiting for GAR access: %v", np.Name, checkErr)
	}
	if meta.SetStatusCondition(&np.Status.Conditions, condition) {
		changed = true
	}
	if changed {
		if err := r.client.Status().Update(ctx, np); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, fmt.Errorf("update NodePool GAR access status: %w", err)
		}
	}
	return checkErr == nil, nil
}

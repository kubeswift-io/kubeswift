package swiftmigration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

// volumeDetachWait bounds how long live Validating waits for the guest's
// volumes to detach from a node other than the source's. A failed live
// attempt can leave its target attached for minutes: the attach completes
// after the destination pod is gone, and Kubernetes detaches it later (about
// 4 minutes in the lab, #692). An attachment still there after this is stuck,
// and the migration fails, naming it.
const volumeDetachWait = 5 * time.Minute

// volumeDetachPollInterval is the requeue cadence during that wait. A
// VolumeAttachment going away does not enqueue the migration.
const volumeDetachPollInterval = 5 * time.Second

// phaseDetailAwaitingVolumeDetach is the Validating phaseDetail during that
// wait. Stable per the phaseDetail vocabulary discipline (see
// api/migration/v1alpha1): operators may match on it.
const phaseDetailAwaitingVolumeDetach = "waiting for the guest's volumes to detach from another node"

// awaitVolumeDetach runs the attachment gate of live Validating. nil means no
// volume of the guest is attached anywhere but the source pod's node.
//
// While one is, the migration stays in Validating and requeues, with
// Compatible=Unknown reason AwaitingVolumeDetach. The wait is timed from that
// condition, so a controller restart does not restart it, and it does not run
// on into another wait's time: the condition goes as soon as the volumes are
// clear. Once volumeDetachWait has run out, the migration fails.
func (r *SwiftMigrationReconciler) awaitVolumeDetach(
	ctx context.Context,
	status *migrationv1alpha1.SwiftMigrationStatus,
	src *corev1.Pod,
) *phaseResult {
	stray, err := r.strayAttachments(ctx, src)
	if err != nil {
		return phaseTransient(err)
	}
	if len(stray) == 0 {
		if c := apimeta.FindStatusCondition(status.Conditions, migrationv1alpha1.SwiftMigrationConditionCompatible); c != nil &&
			c.Status == metav1.ConditionUnknown && c.Reason == ReasonAwaitingVolumeDetach {
			apimeta.RemoveStatusCondition(&status.Conditions, migrationv1alpha1.SwiftMigrationConditionCompatible)
		}
		return nil
	}
	detail := strings.Join(stray, "; ")
	setCondition(status, migrationv1alpha1.SwiftMigrationConditionCompatible, metav1.ConditionUnknown,
		ReasonAwaitingVolumeDetach, "the guest's volumes are still attached to another node: "+detail)
	c := apimeta.FindStatusCondition(status.Conditions, migrationv1alpha1.SwiftMigrationConditionCompatible)
	if time.Since(c.LastTransitionTime.Time) < volumeDetachWait {
		setPhaseDetail(status, phaseDetailAwaitingVolumeDetach)
		return phaseRequeue(volumeDetachPollInterval)
	}
	msg := fmt.Sprintf("the guest's volumes are still attached to another node after %s: %s. "+
		"An earlier migration may have left them; migrate again once `kubectl get volumeattachments` no longer lists them",
		volumeDetachWait, detail)
	setCondition(status, migrationv1alpha1.SwiftMigrationConditionCompatible, metav1.ConditionFalse, ReasonValidationFailed, msg)
	return phaseFailure(msg, migrationv1alpha1.FailureReasonOther)
}

// strayAttachments names the VolumeAttachments of src's persistent volumes on
// a node other than src's own, as `PVC "<claim>" on node <node>
// (VolumeAttachment <name>)`, sorted. A claim not yet bound has no attachment
// to wait for.
func (r *SwiftMigrationReconciler) strayAttachments(ctx context.Context, src *corev1.Pod) ([]string, error) {
	claimOfPV := map[string]string{}
	for _, v := range src.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		var pvc corev1.PersistentVolumeClaim
		if err := r.Get(ctx, client.ObjectKey{Namespace: src.Namespace, Name: v.PersistentVolumeClaim.ClaimName}, &pvc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if pvc.Spec.VolumeName != "" {
			claimOfPV[pvc.Spec.VolumeName] = pvc.Name
		}
	}
	if len(claimOfPV) == 0 {
		return nil, nil
	}
	var vas storagev1.VolumeAttachmentList
	if err := r.List(ctx, &vas); err != nil {
		return nil, fmt.Errorf("list VolumeAttachments: %w", err)
	}
	var out []string
	for i := range vas.Items {
		va := &vas.Items[i]
		pv := va.Spec.Source.PersistentVolumeName
		if pv == nil || va.Spec.NodeName == src.Spec.NodeName {
			continue
		}
		if claim, ok := claimOfPV[*pv]; ok {
			out = append(out, fmt.Sprintf("PVC %q on node %s (VolumeAttachment %s)", claim, va.Spec.NodeName, va.Name))
		}
	}
	sort.Strings(out)
	return out, nil
}

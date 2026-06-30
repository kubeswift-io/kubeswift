package swiftmigration

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

// PodTerminationGracePeriod is the grace period passed to Delete(pod)
// in the Preparing phase. Mirrors swiftctl stop's default. Long enough
// for swiftletd to issue a clean CH shutdown and for the network-init
// teardown to run.
const PodTerminationGracePeriod = 30

// preparingPollInterval is the requeue cadence used by handlePreparing
// while waiting for pod termination + VolumeAttachment GC. Short
// enough that operators see progress; the SwiftGuest watch in
// SetupWithManager (commit 5) wakes the controller faster on actual
// state changes.
const preparingPollInterval = 5 * time.Second

// handlePreparing implements the Preparing phase per architect Q3
// (Option A: patch runPolicy=Stopped first, then Delete(pod)).
//
// The phase has three sub-states tracked via phaseDetail:
//
//  1. "claiming SwiftGuest (annotation + runPolicy=Stopped)" — first
//     entry. Patch the AnnotationMigrationInProgress annotation as the
//     idempotency marker (architect Risk 3) AND patch
//     runPolicy=Stopped, in a single combined client.MergeFrom so the
//     SwiftGuest controller's reconciler can't observe a partial
//     state. Then issue Delete(pod) with grace=30s (idempotent —
//     IsNotFound is fine).
//  2. "waiting for source pod termination" — polling for Pod NotFound.
//     The kubelet runs swiftletd's SIGTERM handler (CH shutdown),
//     then the container exits and the pod terminates.
//  3. "waiting for volume detach" — polling for VolumeAttachment GC.
//     The cluster-controller-manager deletes the VolumeAttachment
//     after the kubelet's NodeUnpublishVolume completes; the CSI
//     driver then performs ControllerUnpublishVolume which makes the
//     PV available for cross-node attach. Without this gate, the
//     destination launcher pod would Multi-Attach error on the same
//     RWO PVC.
//
// Idempotency: the annotation is the source-of-truth. Re-entry with
// AnnotationMigrationInProgress already matching our name skips the
// claim step. Re-entry with the annotation matching a *different*
// SwiftMigration fails this migration with a clear "another migration
// in progress" error — Phase 1's mitigation for two operators
// concurrently submitting migrations of the same guest.
//
// Failure mode (architect Risk 2): if the controller crashes after
// Delete(pod) but before transitioning to StopAndCopy, restart picks
// up the same SwiftGuest annotation marker and resumes from the
// poll loop. The cutover hasn't happened yet (no spec.nodeName patch),
// so source resumes via runPolicy: Running being patched back —
// implemented in commit 10's failure path.
func (r *SwiftMigrationReconciler) handlePreparing(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	// Phase 3a per-mode dispatch. By Preparing, status.Mode has been
	// stamped by Validating; isLiveMode reads from status.
	if isLiveMode(mig, status) {
		return r.handlePreparingLive(ctx, mig, status)
	}

	// Re-resolve source guest. The Validating phase already touched
	// it; we re-read because runPolicy/annotation may have been
	// modified between phase transitions.
	var guest swiftv1alpha1.SwiftGuest
	if getErr := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return phaseFailure(fmt.Sprintf("source SwiftGuest %q deleted during Preparing", mig.Spec.GuestRef.Name), "")
		}
		return phaseTransient(fmt.Errorf("get source guest: %w", getErr))
	}

	// Idempotency check: AnnotationMigrationInProgress is the source
	// of truth. Three cases:
	//   - missing → write our name + patch runPolicy=Stopped (combined)
	//   - present + matches our name → re-entry, proceed to poll
	//   - present + matches different name → conflict, fail
	current := guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress]
	if current != "" && current != mig.Name {
		return phaseFailure(fmt.Sprintf("another SwiftMigration %q is already in progress for SwiftGuest %q", current, guest.Name), "")
	}

	// GPU release-and-reallocate: reserve the target GPUs BEFORE stopping the
	// source (the reserve-before-stop atomicity — a failed reserve never
	// strands a stopped, GPU-less guest). Idempotent across re-entries; runs
	// before the claim+stop below. Non-GPU guests skip this entirely.
	if guest.HasVFIODevices() {
		if res := r.reserveTargetGPUs(ctx, mig, &guest, status); res != nil {
			return res
		}
	}

	if current == "" {
		// First entry: claim the guest. Single combined MergeFrom
		// patch sets both the annotation and runPolicy=Stopped so the
		// SwiftGuest controller cannot observe a half-claimed state
		// (annotation set but runPolicy still Running, or vice versa).
		patch := client.MergeFrom(guest.DeepCopy())
		if guest.Annotations == nil {
			guest.Annotations = map[string]string{}
		}
		guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] = mig.Name
		guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
		if patchErr := r.Patch(ctx, &guest, patch); patchErr != nil {
			return phaseTransient(fmt.Errorf("claim SwiftGuest with annotation + runPolicy=Stopped: %w", patchErr))
		}
		setPhaseDetail(status, "claimed SwiftGuest; deleting source launcher pod")
		if r.Recorder != nil {
			r.Recorder.Event(mig, corev1.EventTypeNormal, "GuestClaimed",
				fmt.Sprintf("claimed SwiftGuest %q with runPolicy=Stopped", guest.Name))
		}
	}

	// Delete the source launcher pod. Resolve the pod by the canonical
	// name (status.podRef.name when set), NOT literal guest.Name: after
	// a prior LIVE migration the guest's launcher pod was renamed to
	// `<guest>-mig-<uid>` and status.podRef.name points there. Looking
	// up guest.Name would return NotFound, the controller would assume
	// the pod is already gone, and advance to the volume-detach wait
	// while the real (renamed) pod keeps the PVC attached — hanging
	// Preparing indefinitely (TFU #18). For a fresh guest that was never
	// live-migrated, canonicalPodNameForGuest returns guest.Name, so the
	// Phase 1 offline path is unchanged. Same W26/LBA-2 canonical-pod-
	// name invariant the live path uses; the offline path predates it.
	// Idempotent: NotFound is the success case here.
	//
	// Note on Terminating-with-finalizer: a pod with DeletionTimestamp
	// set but still existing (e.g., custom admission finalizer) is
	// NOT NotFound — apierrors.IsNotFound returns false. The controller
	// stays in this poll branch, doesn't re-issue Delete (the
	// DeletionTimestamp guard below), and the kubelet's normal pod-
	// deletion path runs to completion. Don't try to "optimize" by
	// treating Terminating as Gone.
	var pod corev1.Pod
	srcPodName := canonicalPodNameForGuest(&guest)
	getErr := r.Get(ctx, client.ObjectKey{Name: srcPodName, Namespace: guest.Namespace}, &pod)
	podGone := apierrors.IsNotFound(getErr)
	if getErr != nil && !podGone {
		return phaseTransient(fmt.Errorf("get source pod: %w", getErr))
	}
	if !podGone {
		// Pod still exists — issue Delete (idempotent on re-entry).
		// Skip the Delete when DeletionTimestamp is already set; the
		// API-server has already observed our prior delete and the
		// pod is on its way out.
		if pod.DeletionTimestamp == nil {
			grace := int64(PodTerminationGracePeriod)
			if delErr := r.Delete(ctx, &pod, &client.DeleteOptions{GracePeriodSeconds: &grace}); delErr != nil && !apierrors.IsNotFound(delErr) {
				return phaseTransient(fmt.Errorf("delete source pod: %w", delErr))
			}
			if r.Recorder != nil {
				r.Recorder.Event(mig, corev1.EventTypeNormal, "PodTerminating",
					fmt.Sprintf("deleted source launcher pod %q (grace=%ds)", pod.Name, PodTerminationGracePeriod))
			}
		}
		setPhaseDetail(status, "waiting for source pod termination")
		setReadyCondition(status, metav1.ConditionFalse, ReasonPreparing, "waiting for source pod termination")
		return phaseRequeue(preparingPollInterval)
	}

	// Pod is gone. Now wait for the per-guest root PVC's
	// VolumeAttachment to be GC'd. Without this gate, the destination
	// pod can hit a Multi-Attach error on RWO storage.
	pvcName := swiftguest.RootDiskCloneName(guest.Name)
	attached, err := r.isPVCStillAttached(ctx, guest.Namespace, pvcName)
	if err != nil {
		return phaseTransient(err)
	}
	if attached {
		setPhaseDetail(status, fmt.Sprintf("waiting for volume detach (PVC %q)", pvcName))
		setReadyCondition(status, metav1.ConditionFalse, ReasonPreparing, "waiting for volume detach")
		if r.Recorder != nil {
			// Kubernetes EventRecorder aggregates by
			// (reason, message, source, involvedObject) and increments
			// the existing event's count rather than creating
			// duplicates. Operators see one PVCDetaching event with
			// count=N, not N events.
			r.Recorder.Event(mig, corev1.EventTypeNormal, "PVCDetaching",
				fmt.Sprintf("waiting for VolumeAttachment GC of PVC %q", pvcName))
		}
		return phaseRequeue(preparingPollInterval)
	}

	// Pod gone + no VolumeAttachment → safe to advance. The Cluster
	// Controller Manager has finished ControllerUnpublishVolume; the
	// PV is available for cross-node attach.
	setPhase(status, migrationv1alpha1.SwiftMigrationPhaseStopAndCopy)
	setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy, "stopping and copying state")
	setPhaseDetail(status, "patching SwiftGuest to start on destination node")
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, "VolumeDetached",
			fmt.Sprintf("PVC %q detached; advancing to StopAndCopy", pvcName))
	}
	return phaseAdvance()
}

// isPVCStillAttached returns true when a VolumeAttachment exists that
// references the PV backing the named PVC. The cluster controller
// manager creates the VolumeAttachment when a pod first uses the PVC
// and deletes it after the kubelet completes NodeUnpublishVolume on
// the consuming pod's termination. Once the VA is gone, the CSI
// driver completes ControllerUnpublishVolume and the PV is available
// for cross-node attach.
//
// Returns (false, nil) if the PVC has no bound PV (still creating, or
// already deleted); the caller treats this as "not attached, proceed."
// Returns (true, nil) for the normal "still attached, requeue" case.
// Returns err for transient API failures.
func (r *SwiftMigrationReconciler) isPVCStillAttached(
	ctx context.Context,
	namespace, pvcName string,
) (bool, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: namespace}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get PVC %q: %w", pvcName, err)
	}
	if pvc.Spec.VolumeName == "" {
		// PVC isn't bound to a PV yet; nothing to detach.
		return false, nil
	}

	// TODO(phase-3+): match VolumeAttachment to NodeName as well.
	// Currently any VA referencing the PV blocks advance, even one
	// from a different node (e.g., a stale VA from a prior failed
	// migration). The cluster controller manager GCs orphan VAs in
	// practice, so this is a non-issue today, but on a hypothetical
	// failure path the wait would be longer than necessary.
	// Comparing va.Spec.NodeName to guest.Status.NodeName would
	// narrow the match — at the cost of relying on a status field
	// that may be stale.
	var vaList storagev1.VolumeAttachmentList
	if err := r.List(ctx, &vaList); err != nil {
		return false, fmt.Errorf("list VolumeAttachments: %w", err)
	}
	for i := range vaList.Items {
		va := &vaList.Items[i]
		if va.Spec.Source.PersistentVolumeName != nil && *va.Spec.Source.PersistentVolumeName == pvc.Spec.VolumeName {
			return true, nil
		}
	}
	return false, nil
}

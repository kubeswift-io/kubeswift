package swiftrestore

// In-place csi-volume-snapshot restore (#711): roll a guest's root disk back to
// a CSI snapshot of it, keeping the guest.
//
// A PVC cannot be renamed, so the restore seeds a new root-disk PVC from the
// VolumeSnapshot (the guest's next generation, see internal/rootdisk) and swaps
// the guest onto it:
//
//  1. Pending: refuse what cannot work, wait while a migration or another
//     restore holds the guest.
//  2. Restoring: record the swap (status.rootDisk) before acting, seed the new
//     PVC (owned by the restore, not yet a root disk), wait until it is Bound,
//     hold the guest (AnnotationDiskRestore), delete its launcher, wait until no
//     launcher mounts a root disk, then make the new PVC the guest's root disk
//     (labels, generation, owner) and release the hold.
//  3. Resuming: wait until a launcher started on the new disk has the guest
//     running, then delete the replaced PVC.
//
// The replaced PVC is kept until the restore is Ready. A restore that fails
// after the swap deletes the new PVC and the guest's launcher, so the guest
// boots from its previous disk again (rollbackInPlaceDisk).

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
	"github.com/kubeswift-io/kubeswift/internal/rootdisk"
)

const (
	// rolePendingRootDisk marks a PVC an in-place restore seeded but has not
	// swapped in yet. It is not a root disk to anything until the swap.
	rolePendingRootDisk = "root-disk-pending"
	// launcherRootVolume is the launcher pod's root-disk volume name.
	launcherRootVolume = "root-disk"
	// inPlaceBusyRetry is how often a Pending in-place restore waiting for a
	// migration or another restore looks again.
	inPlaceBusyRetry = 10 * time.Second
)

var swiftGuestGVK = schema.GroupVersionKind{Group: "swift.kubeswift.io", Version: "v1alpha1", Kind: "SwiftGuest"}

// isInPlaceCSI reports whether a csi-volume-snapshot restore targets the
// snapshot's own guest.
func isInPlaceCSI(snap *snapshotv1alpha1.SwiftSnapshot, restore *snapshotv1alpha1.SwiftRestore) bool {
	return restore.Spec.TargetGuest.Name == snap.Spec.GuestRef.Name
}

// handlePendingCSIInPlace validates an in-place csi-volume-snapshot restore of
// guest and advances it to Restoring.
func (r *SwiftRestoreReconciler) handlePendingCSIInPlace(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	guest *swiftv1alpha1.SwiftGuest,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, error) {
	fail := func(reason, msg string) (bool, time.Duration, error) {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, reason, msg)
		return true, 0, nil
	}
	if !restore.Spec.TargetGuest.OverwriteExisting {
		return fail(ReasonTargetConflict, "an in-place restore replaces the root disk of SwiftGuest "+guest.Name+
			"; set targetGuest.overwriteExisting=true to confirm")
	}
	if !restore.Spec.ResumeAfterRestore {
		return fail(ReasonInPlaceUnsupported, "an in-place csi-volume-snapshot restore boots SwiftGuest "+guest.Name+
			" from the restored disk; resumeAfterRestore=false is not supported for it (set the guest's runPolicy to Stopped to keep it stopped)")
	}
	if guest.Spec.KernelRef != nil {
		return fail(ReasonInPlaceUnsupported, "SwiftGuest "+guest.Name+" boots from a kernel and has no root-disk PVC to restore")
	}
	if guest.Status.SharedBaseDisk != nil {
		return fail(ReasonInPlaceUnsupported, "SwiftGuest "+guest.Name+" has a shared-base root disk, which is built on its node, not held in a PVC")
	}
	if other := guest.Annotations[snapshotv1alpha1.AnnotationDiskRestore]; other != "" && other != restore.Name {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonGuestBusy,
			"waiting: SwiftRestore "+other+" is restoring SwiftGuest "+guest.Name+"'s disk")
		return false, inPlaceBusyRetry, nil
	}
	if mig, err := r.activeMigration(ctx, guest); err != nil {
		return false, 0, err
	} else if mig != "" {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonGuestBusy,
			"waiting: SwiftMigration "+mig+" is migrating SwiftGuest "+guest.Name)
		return false, inPlaceBusyRetry, nil
	}
	status.GuestRef = &snapshotv1alpha1.SwiftRestoreGuestRef{Name: guest.Name}
	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseRestoring)
	setReadyCondition(status, metav1.ConditionFalse, ReasonRestoring,
		"restoring the root disk of SwiftGuest "+guest.Name+" in place")
	return true, 0, nil
}

// activeMigration returns the SwiftMigration that has claimed guest and is not
// finished, or "".
func (r *SwiftRestoreReconciler) activeMigration(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (string, error) {
	name := guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress]
	if name == "" {
		return "", nil
	}
	var mig migrationv1alpha1.SwiftMigration
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: name}, &mig); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	switch mig.Status.Phase {
	case migrationv1alpha1.SwiftMigrationPhaseCompleted,
		migrationv1alpha1.SwiftMigrationPhaseFailed,
		migrationv1alpha1.SwiftMigrationPhaseCancelled:
		return "", nil
	}
	return name, nil
}

// handleRestoringCSIInPlace seeds the new root disk and swaps the guest onto it.
func (r *SwiftRestoreReconciler) handleRestoringCSIInPlace(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
	vsName string,
	sizeBytes int64,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, string, error) {
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: snap.Spec.GuestRef.Name, Namespace: restore.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return false, 0, "SwiftGuest " + snap.Spec.GuestRef.Name + " was deleted during the restore", nil
		}
		return false, 0, "", err
	}

	// Record the swap before creating anything, so every later pass (and a
	// failure) knows which PVC is new and which one it replaces.
	if status.RootDisk == nil {
		prev, gen, err := rootdisk.Claim(ctx, r.Client, &guest)
		if err != nil {
			return false, 0, "", err
		}
		status.RootDisk = &snapshotv1alpha1.SwiftRestoreRootDisk{
			ClaimName:         rootdisk.GenerationName(guest.Name, gen+1),
			PreviousClaimName: prev,
		}
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoring,
			"seeding root disk "+status.RootDisk.ClaimName+" from the snapshot")
		return false, time.Second, "", nil
	}
	rd := status.RootDisk
	gen := 1
	if n, err := generationOf(rd.ClaimName, guest.Name); err == nil {
		gen = n
	}

	// Whether the swap happened is read from the PVC, never only from
	// status: a status write lost after the swap must not make the next pass
	// stop the guest that already runs from the restored disk.
	var pvc corev1.PersistentVolumeClaim
	pvcErr := r.Get(ctx, client.ObjectKey{Name: rd.ClaimName, Namespace: restore.Namespace}, &pvc)
	if pvcErr != nil && !apierrors.IsNotFound(pvcErr) {
		return false, 0, "", pvcErr
	}
	if pvcErr == nil && rootdisk.Generation(&guest, &pvc) == gen {
		rd.Switched = true
	}

	if !rd.Switched {
		shape, err := r.diskShapeOf(ctx, restore.Namespace, rd.PreviousClaimName)
		if err != nil {
			return false, 0, "", err
		}
		if err := r.ensureInPlaceRestorePVC(ctx, restore, &guest, rd.ClaimName, vsName, shape, sizeBytes); err != nil {
			return false, 0, "", err
		}
		if err := r.Get(ctx, client.ObjectKey{Name: rd.ClaimName, Namespace: restore.Namespace}, &pvc); err != nil {
			return false, 0, "", err
		}
		if pvc.Status.Phase != corev1.ClaimBound {
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoring,
				"restored root disk "+rd.ClaimName+" not yet Bound (phase="+string(pvc.Status.Phase)+")")
			return false, 5 * time.Second, "", nil
		}

		// Stop the guest: hold it, so the guest controller starts no launcher
		// from either disk, then delete its launcher. The hold is set on every
		// pass until the swap: a merge patch of the same value is a no-op.
		if err := r.setDiskRestoreAnnotation(ctx, &guest, &restore.Name); err != nil {
			return false, 0, "", fmt.Errorf("hold SwiftGuest %s: %w", guest.Name, err)
		}
		running, err := r.rootDiskLaunchers(ctx, &guest)
		if err != nil {
			return false, 0, "", err
		}
		if len(running) > 0 {
			for i := range running {
				if running[i].DeletionTimestamp == nil {
					zero := int64(0)
					if err := r.Delete(ctx, &running[i], &client.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !apierrors.IsNotFound(err) {
						return false, 0, "", fmt.Errorf("delete launcher %s: %w", running[i].Name, err)
					}
				}
			}
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoring,
				"stopping SwiftGuest "+guest.Name+": waiting for launcher "+running[0].Name+" to go")
			return false, 2 * time.Second, "", nil
		}

		// Swap: the new PVC becomes the guest's root disk.
		if err := r.makeRootDisk(ctx, &pvc, &guest, gen); err != nil {
			return false, 0, "", err
		}
		rd.Switched = true
	}

	if err := r.releaseDiskRestoreHold(ctx, &guest, restore.Name); err != nil {
		return false, 0, "", err
	}
	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseResuming)
	setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
		"SwiftGuest "+guest.Name+" now uses root disk "+rd.ClaimName+"; waiting for it to run")
	return true, 0, "", nil
}

// handleResumingCSIInPlace waits until the guest runs from the restored disk,
// then deletes the replaced PVC.
func (r *SwiftRestoreReconciler) handleResumingCSIInPlace(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, error) {
	rd := status.RootDisk
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.TargetGuest.Name, Namespace: restore.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
				"SwiftGuest "+restore.Spec.TargetGuest.Name+" was deleted during the restore")
			return true, 0, nil
		}
		return false, 0, err
	}

	// The swap released the hold; release it again in case that patch was
	// lost with the status write that followed it.
	if err := r.releaseDiskRestoreHold(ctx, &guest, restore.Name); err != nil {
		return false, 0, err
	}

	ready := false
	if guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyStopped {
		// Nothing boots a stopped guest; it starts from the restored disk
		// whenever it is started.
		ready = true
	} else {
		pod, err := r.findLauncherPod(ctx, &guest)
		if err != nil {
			return false, 0, err
		}
		switch {
		case pod == nil:
			setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
				"waiting for SwiftGuest "+guest.Name+"'s launcher on root disk "+rd.ClaimName)
			return false, 5 * time.Second, nil
		case rootClaimOf(pod) != rd.ClaimName:
			setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
				"launcher "+pod.Name+" mounts "+rootClaimOf(pod)+", not the restored disk "+rd.ClaimName+"; waiting")
			return false, 5 * time.Second, nil
		case pod.Status.Phase == corev1.PodFailed:
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
				"SwiftGuest "+guest.Name+" failed to start from the restored disk (launcher "+pod.Name+" Failed); it goes back to "+rd.PreviousClaimName)
			return true, 0, nil
		}
		ready = runningSince(&guest, pod.CreationTimestamp)
	}
	if !ready {
		setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
			"waiting for SwiftGuest "+guest.Name+" to run from root disk "+rd.ClaimName)
		return false, 5 * time.Second, nil
	}

	if rd.PreviousClaimName != "" && rd.PreviousClaimName != rd.ClaimName {
		var prev corev1.PersistentVolumeClaim
		err := r.Get(ctx, client.ObjectKey{Name: rd.PreviousClaimName, Namespace: restore.Namespace}, &prev)
		if err == nil && metav1.IsControlledBy(&prev, &guest) {
			if err := r.Delete(ctx, &prev); err != nil && !apierrors.IsNotFound(err) {
				return false, 0, fmt.Errorf("delete replaced root disk %s: %w", prev.Name, err)
			}
			// A finished pod still pins a PVC it mounts (pvc-protection), so
			// the guest's own Jobs that filled the replaced disk (the image
			// clone Job) go with it.
			if err := r.deleteFinishedDiskJobs(ctx, &guest, prev.Name); err != nil {
				return false, 0, err
			}
		} else if err != nil && !apierrors.IsNotFound(err) {
			return false, 0, err
		}
	}
	now := metav1.Now()
	status.CompletedAt = &now
	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseReady)
	setReadyCondition(status, metav1.ConditionTrue, ReasonRestoreReady,
		"SwiftGuest "+guest.Name+" runs from the restored root disk "+rd.ClaimName+"; "+rd.PreviousClaimName+" deleted")
	return true, 0, nil
}

// rollbackInPlaceDisk undoes an in-place disk restore that failed: the guest
// goes back to its previous root disk and is released. Before the swap the new
// PVC is only deleted; after it, the guest's launcher is deleted too, so the
// guest controller boots it from the previous disk.
func (r *SwiftRestoreReconciler) rollbackInPlaceDisk(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) error {
	rd := status.RootDisk
	if rd == nil || rd.ClaimName == "" || rd.ClaimName == rd.PreviousClaimName {
		return nil
	}
	var guest swiftv1alpha1.SwiftGuest
	guestErr := r.Get(ctx, client.ObjectKey{Name: restore.Spec.TargetGuest.Name, Namespace: restore.Namespace}, &guest)
	if guestErr != nil && !apierrors.IsNotFound(guestErr) {
		return guestErr
	}
	// The new PVC first: once it is deleting, it is nobody's root disk, so a
	// launcher the guest controller starts from here on gets the previous one.
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: rd.ClaimName, Namespace: restore.Namespace}, &pvc); err == nil {
		if err := r.Delete(ctx, &pvc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete restored root disk %s: %w", pvc.Name, err)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if guestErr != nil {
		return nil
	}
	if rd.Switched {
		pods, err := r.rootDiskLaunchers(ctx, &guest)
		if err != nil {
			return err
		}
		for i := range pods {
			if rootClaimOf(&pods[i]) != rd.ClaimName || pods[i].DeletionTimestamp != nil {
				continue
			}
			zero := int64(0)
			if err := r.Delete(ctx, &pods[i], &client.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete launcher %s: %w", pods[i].Name, err)
			}
		}
	}
	return r.releaseDiskRestoreHold(ctx, &guest, restore.Name)
}

// ensureInPlaceRestorePVC creates (idempotent) the PVC an in-place restore
// seeds from the VolumeSnapshot. Owned by the restore and labelled pending, it
// is no root disk until makeRootDisk swaps it in, so deleting the restore before
// the swap deletes it.
func (r *SwiftRestoreReconciler) ensureInPlaceRestorePVC(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	guest *swiftv1alpha1.SwiftGuest,
	pvcName, vsName string,
	shape sourceDiskShape,
	sizeBytes int64,
) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: restore.Namespace}, &existing)
	if err == nil {
		if existing.Labels[swiftRestoreOwnerLabel] != names.LabelValue(restore.Name) &&
			!metav1.IsControlledBy(&existing, guest) {
			return fmt.Errorf("PVC %s already exists and is not this restore's", pvcName)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get restore PVC: %w", err)
	}
	apiGroup := "snapshot.storage.k8s.io"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: restore.Namespace,
			Labels: map[string]string{
				swiftRestoreOwnerLabel: names.LabelValue(restore.Name),
				rootdisk.LabelGuest:    guest.Name,
				rootdisk.LabelRole:     rolePendingRootDisk,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(restore, swiftRestoreGVK)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      shape.AccessModes,
			VolumeMode:       shape.VolumeMode,
			StorageClassName: ptrString(shape.StorageClassName),
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     vsName,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(sizeBytes, resource.BinarySI)},
			},
		},
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create restore PVC: %w", err)
	}
	return nil
}

// makeRootDisk swaps pvc in as guest's generation-gen root disk: root-disk
// labels, the generation annotation, and the guest as its only controller (the
// restore's ownership ends, so deleting the restore keeps the guest's disk).
func (r *SwiftRestoreReconciler) makeRootDisk(ctx context.Context, pvc *corev1.PersistentVolumeClaim, guest *swiftv1alpha1.SwiftGuest, gen int) error {
	if rootdisk.Generation(guest, pvc) == gen {
		return nil
	}
	if pvc.Labels == nil {
		pvc.Labels = map[string]string{}
	}
	pvc.Labels[rootdisk.LabelGuest] = guest.Name
	pvc.Labels[rootdisk.LabelRole] = rootdisk.RoleRootDisk
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[rootdisk.AnnotationGeneration] = strconv.Itoa(gen)
	pvc.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)}
	if err := r.Update(ctx, pvc); err != nil {
		return fmt.Errorf("swap in root disk %s: %w", pvc.Name, err)
	}
	return nil
}

// releaseDiskRestoreHold removes AnnotationDiskRestore from guest if, read
// uncached, it names restore: a cached guest may not show a hold set a moment
// ago, and a hold another restore set later is not this one's to remove. The
// patch carries the resourceVersion it read, so a change in between conflicts
// and is retried.
func (r *SwiftRestoreReconciler) releaseDiskRestoreHold(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, restore string) error {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var live swiftv1alpha1.SwiftGuest
	if err := reader.Get(ctx, client.ObjectKeyFromObject(guest), &live); err != nil {
		return client.IgnoreNotFound(err)
	}
	if live.Annotations[snapshotv1alpha1.AnnotationDiskRestore] != restore {
		return nil
	}
	base := live.DeepCopy()
	delete(live.Annotations, snapshotv1alpha1.AnnotationDiskRestore)
	if err := r.Patch(ctx, &live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("release SwiftGuest %s: %w", guest.Name, err)
	}
	return nil
}

// setDiskRestoreAnnotation sets AnnotationDiskRestore on guest to *name, or
// removes it when name is nil.
func (r *SwiftRestoreReconciler) setDiskRestoreAnnotation(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, name *string) error {
	var value any
	if name != nil {
		value = *name
	}
	data, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{snapshotv1alpha1.AnnotationDiskRestore: value},
	}})
	if err != nil {
		return err
	}
	return r.Patch(ctx, guest, client.RawPatch(types.MergePatchType, data))
}

// deleteFinishedDiskJobs deletes the guest's Jobs whose finished pods mount
// claim: those pods would keep the deleted PVC Terminating. Only Jobs the guest
// controls are touched; anything else that mounts the PVC keeps it until it
// goes.
func (r *SwiftRestoreReconciler) deleteFinishedDiskJobs(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, claim string) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(guest.Namespace)); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		if !mountsClaim(pod, claim) {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != "Job" {
			continue
		}
		var job batchv1.Job
		if err := r.Get(ctx, client.ObjectKey{Name: owner.Name, Namespace: guest.Namespace}, &job); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(&job, guest) {
			continue
		}
		if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete Job %s: %w", job.Name, err)
		}
	}
	return nil
}

// mountsClaim reports whether pod mounts PVC claim.
func mountsClaim(pod *corev1.Pod, claim string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// rootDiskLaunchers lists the guest's pods that mount a root disk: its
// launchers, whatever their name (a live migration renames them).
func (r *SwiftRestoreReconciler) rootDiskLaunchers(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := r.List(ctx, &list, client.InNamespace(guest.Namespace)); err != nil {
		return nil, err
	}
	var out []corev1.Pod
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], guest) && rootClaimOf(&list.Items[i]) != "" {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// rootClaimOf returns the PVC a launcher pod mounts as its root disk, or "".
func rootClaimOf(pod *corev1.Pod) string {
	for _, v := range pod.Spec.Volumes {
		if v.Name == launcherRootVolume && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// runningSince reports whether guest has GuestRunning=True set at or after
// since: a GuestRunning left from the launcher the restore deleted does not
// count.
func runningSince(guest *swiftv1alpha1.SwiftGuest, since metav1.Time) bool {
	for _, c := range guest.Status.Conditions {
		if c.Type == conditionGuestRunning {
			return c.Status == metav1.ConditionTrue && !c.LastTransitionTime.Before(&since)
		}
	}
	return false
}

// generationOf parses the generation out of a name GenerationName produced.
func generationOf(claim, guestName string) (int, error) {
	prefix := rootdisk.GenerationName(guestName, 1)
	prefix = prefix[:len(prefix)-1]
	if len(claim) <= len(prefix) || claim[:len(prefix)] != prefix {
		return 0, fmt.Errorf("%s is not a generation name of %s", claim, guestName)
	}
	return strconv.Atoi(claim[len(prefix):])
}

// diskShapeOf returns the storage shape of PVC name, which the restored disk
// must reproduce (see sourceDiskShape).
func (r *SwiftRestoreReconciler) diskShapeOf(ctx context.Context, namespace, name string) (sourceDiskShape, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &pvc); err != nil {
		return sourceDiskShape{}, fmt.Errorf("get root disk PVC %s: %w", name, err)
	}
	return shapeOf(&pvc)
}

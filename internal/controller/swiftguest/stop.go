package swiftguest

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

const (
	// stopPollInterval is how soon a guest whose launcher is shutting down is
	// looked at again. The launcher's deletion wakes the controller as well;
	// this is the backstop.
	stopPollInterval = 5 * time.Second
	// stopDeferredPollInterval is how soon a stop that waits for another
	// operation is retried. Most of them change the guest or its launcher when
	// they finish, which wakes the controller sooner.
	stopDeferredPollInterval = 10 * time.Second
)

// reconcileStop converges a guest whose runPolicy is Stopped.
//
// With no launcher, or one that has exited, the guest is stopped: that is
// recorded and the pass ends here (done). A launcher that is still up is shut
// down the way actions.Stop does it: deleted with its default grace period, so
// swiftletd turns the SIGTERM into an ACPI power-off. The controller used to
// only keep the launcher from being recreated, so a guest set Stopped with
// kubectl or from Git kept running until someone deleted its pod.
//
// The launcher is not deleted while another operation owns it (stopBlocker);
// the stop waits for it. Either way the pass then goes on as for any live
// launcher, so the guest's status is still reported while it shuts down or
// waits, and requeue says when to look again.
func (r *SwiftGuestReconciler) reconcileStop(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, status *swiftv1alpha1.SwiftGuestStatus) (requeue time.Duration, done bool, err error) {
	var pod corev1.Pod
	podErr := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: canonicalPodName(guest)}, &pod)
	if podErr != nil && !apierrors.IsNotFound(podErr) {
		return 0, false, podErr
	}
	podGone := apierrors.IsNotFound(podErr)
	// A launcher that handed its VM to a live migration has exited, but the
	// guest has not stopped: the VM runs in the destination pod, which cutover
	// has yet to name in status.podRef. Reporting the guest Stopped would clear
	// the GuestRunning the destination wrote. The migration owns it, below.
	podDone := !podGone && !launcherHandedOff(&pod) &&
		(pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed)
	if podGone || podDone {
		r.stopDeferred.Delete(client.ObjectKeyFromObject(guest))
		status.Phase = swiftv1alpha1.SwiftGuestPhaseStopped
		// Nothing of the last run is true of a stopped guest.
		ClearRunState(status, "Stopped", "the guest is stopped; it has no launcher")
		var podForStopped *corev1.Pod
		if !podGone {
			podForStopped = &pod
		}
		recordGuestMetrics(guest, &guest.Status, status, podForStopped)
		return 0, true, r.patchStatus(ctx, guest, status)
	}

	// Already shutting down: deleted by an earlier pass, by actions.Stop, or by
	// the operation that owned it. Deleting it again changes nothing.
	if pod.DeletionTimestamp != nil {
		return stopPollInterval, false, nil
	}

	owner, err := r.stopBlocker(ctx, guest)
	if err != nil {
		return 0, false, err
	}
	if owner.name != "" {
		// The operation set runPolicy Stopped itself and deletes the launcher
		// on its own schedule: no stop was deferred, so none is reported.
		if owner.stopsGuest {
			return stopPollInterval, false, nil
		}
		// Said once per wait, not on every pass: a live migration wakes this
		// controller many times, and the recorder's per-object spam filter
		// would then drop the Stopping event that follows the wait. The
		// generation is part of the wait, so a guest set Stopped again is told
		// again.
		key := client.ObjectKeyFromObject(guest)
		wait := fmt.Sprintf("%s/%d/%s", guest.UID, guest.Generation, owner.name)
		if prev, ok := r.stopDeferred.Load(key); !ok || prev != wait {
			r.stopDeferred.Store(key, wait)
			log.FromContext(ctx).Info("runPolicy is Stopped; waiting for another operation before stopping the guest",
				"waitingFor", owner.name)
			r.event(guest, corev1.EventTypeNormal, "StopDeferred",
				"runPolicy is Stopped; waiting for %s to finish before stopping the guest", owner.name)
		}
		return stopDeferredPollInterval, false, nil
	}
	r.stopDeferred.Delete(client.ObjectKeyFromObject(guest))

	if err := r.Delete(ctx, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			// Gone since the lookup; the next pass records the guest Stopped.
			return stopPollInterval, false, nil
		}
		return 0, false, err
	}
	log.FromContext(ctx).Info("runPolicy is Stopped; deleted the launcher pod to stop the guest", "pod", pod.Name)
	r.event(guest, corev1.EventTypeNormal, "Stopping",
		"runPolicy is Stopped; deleted launcher pod %s, the guest shuts down within its termination grace period", pod.Name)
	return stopPollInterval, false, nil
}

// launcherOwner is an operation that owns a guest's launcher (stopBlocker).
type launcherOwner struct {
	// name is the operation's kind and name, e.g. "SwiftMigration m"; empty
	// when nothing owns the launcher.
	name string
	// stopsGuest reports that the operation set runPolicy Stopped itself and
	// deletes the launcher on its own: an offline migration, which also
	// restores runPolicy Running when it is done, or a full-state capture.
	stopsGuest bool
}

// stopBlocker finds the operation that owns the guest's launcher, which a
// runPolicy Stopped must wait for. Deleting the launcher under one of them
// breaks it: a migration loses the VM it is moving, a snapshot the VM it is
// capturing, and a restore the launcher it is loading the snapshot into.
//
//   - A SwiftMigration of the guest that has not finished. The
//     migration-in-progress marker on the guest is stamped only by an offline
//     migration, so the migrations are listed; spec.timeout bounds the wait.
//     The marker naming the migration says the migration set runPolicy
//     Stopped: an offline migration claims the guest with both in one patch.
//   - A SwiftRestore onto the guest that is Restoring or Resuming: from
//     Restoring on, the guest's launcher is the restore's. Before that the
//     restore waits on the snapshot, not on the guest.
//   - A SwiftSnapshot of the guest that is Capturing, or Uploading a full-state
//     (includeDisk) capture, which stops and terminates the source itself. A
//     Pending snapshot has not touched the launcher and may be waiting for this
//     very guest to run; other uploads read only what the capture wrote.
func (r *SwiftGuestReconciler) stopBlocker(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (launcherOwner, error) {
	var migs migrationv1alpha1.SwiftMigrationList
	if err := r.List(ctx, &migs, client.InNamespace(guest.Namespace)); err != nil {
		return launcherOwner{}, fmt.Errorf("list SwiftMigrations: %w", err)
	}
	for i := range migs.Items {
		m := &migs.Items[i]
		if m.Spec.GuestRef.Name != guest.Name {
			continue
		}
		switch m.Status.Phase {
		case migrationv1alpha1.SwiftMigrationPhaseCompleted,
			migrationv1alpha1.SwiftMigrationPhaseFailed,
			migrationv1alpha1.SwiftMigrationPhaseCancelled:
			continue
		}
		return launcherOwner{
			name:       "SwiftMigration " + m.Name,
			stopsGuest: guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] == m.Name,
		}, nil
	}

	var restores snapshotv1alpha1.SwiftRestoreList
	if err := r.List(ctx, &restores, client.InNamespace(guest.Namespace)); err != nil {
		return launcherOwner{}, fmt.Errorf("list SwiftRestores: %w", err)
	}
	for i := range restores.Items {
		rst := &restores.Items[i]
		if rst.Spec.TargetGuest.Name != guest.Name {
			continue
		}
		switch rst.Status.Phase {
		case snapshotv1alpha1.SwiftRestorePhaseRestoring, snapshotv1alpha1.SwiftRestorePhaseResuming:
			return launcherOwner{name: "SwiftRestore " + rst.Name}, nil
		}
	}

	var snaps snapshotv1alpha1.SwiftSnapshotList
	if err := r.List(ctx, &snaps, client.InNamespace(guest.Namespace)); err != nil {
		return launcherOwner{}, fmt.Errorf("list SwiftSnapshots: %w", err)
	}
	for i := range snaps.Items {
		s := &snaps.Items[i]
		if s.Spec.GuestRef.Name != guest.Name {
			continue
		}
		switch s.Status.Phase {
		case snapshotv1alpha1.SwiftSnapshotPhaseCapturing:
			return launcherOwner{name: "SwiftSnapshot " + s.Name}, nil
		case snapshotv1alpha1.SwiftSnapshotPhaseUploading:
			if s.Spec.IncludeDisk {
				return launcherOwner{name: "SwiftSnapshot " + s.Name, stopsGuest: true}, nil
			}
		}
	}
	return launcherOwner{}, nil
}

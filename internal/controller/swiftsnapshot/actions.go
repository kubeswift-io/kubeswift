package swiftsnapshot

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// launcherGuestLabel names the guest a launcher pod runs, on every launcher
// (the live-migration destination included).
const launcherGuestLabel = "swift.kubeswift.io/guest"

// The migration action a SwiftMigration writes on a launcher. Mirrors
// rust/swiftletd/src/action.rs MIGRATION_ACTION_*_KEY.
const (
	annoMigrationAction     = "kubeswift.io/migration-action"
	annoMigrationActionID   = "kubeswift.io/migration-action-id"
	annoMigrationActionArgs = "kubeswift.io/migration-action-args"
)

// swiftletd counts a namespace as busy while its action annotation is set,
// whatever became of the action, and rejects both when a snapshot action and
// a migration action are set at once and neither is running. An action left
// on the launcher after its operation ended therefore refused every later
// operation of the other kind (#823): after a memory snapshot the guest could
// not be live-migrated, and the rejection overwrote the snapshot's status.

// clearFinishedActions removes this snapshot's actions from the guest's
// launcher once the snapshot is terminal. Idempotent; it runs on every pass
// over a terminal snapshot, so a controller upgrade clears what earlier
// versions left. The capture is cleared at once: nothing waits for it any
// more, and one swiftletd had not taken up would otherwise still run later.
// The resume queued behind an abandoned capture is cleared only once
// swiftletd has finished it, so the guest is not left paused.
func (r *SwiftSnapshotReconciler) clearFinishedActions(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) error {
	if !capturesMemory(snap) {
		return nil
	}
	pod, err := r.findLauncherPod(ctx, snap.Namespace, snap.Spec.GuestRef.Name)
	if err != nil || pod == nil {
		return err
	}
	a := pod.Annotations
	id := a[annoActionID]
	switch {
	case id == "":
		return nil
	case id == capturingActionID(snap):
	case id == capturingActionID(snap)+"-resume" && a[annoStatusID] == id && actionFinished(a[annoStatus]):
	default:
		return nil
	}
	return removeAnnotations(ctx, r.Client, pod, annoAction, annoActionID, annoActionArgs)
}

// actionFinished reports whether a snapshot-status value is final.
func actionFinished(status string) bool {
	switch status {
	case "ready", "failed", "rejected":
		return true
	}
	return false
}

// migrationSlot is the capture's check of the launcher's migration action.
// A migration that is still under way holds the launcher: the capture waits
// for it, and waitFor names it. An action no running migration owns is a
// leftover (a migration that completed, or was deleted, and did not clear
// it) and is removed so the capture is not rejected for it.
func (r *SwiftSnapshotReconciler) migrationSlot(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, pod *corev1.Pod) (waitFor string, err error) {
	if pod.Annotations[annoMigrationAction] == "" {
		return "", nil
	}
	var migs migrationv1alpha1.SwiftMigrationList
	if err := r.List(ctx, &migs, client.InNamespace(snap.Namespace)); err != nil {
		return "", err
	}
	for i := range migs.Items {
		m := &migs.Items[i]
		if m.Spec.GuestRef.Name != snap.Spec.GuestRef.Name || migrationEnded(m.Status.Phase) {
			continue
		}
		return fmt.Sprintf("waiting for SwiftMigration %s of SwiftGuest %s to finish: the launcher runs a capture or a migration, not both",
			m.Name, snap.Spec.GuestRef.Name), nil
	}
	return "", removeAnnotations(ctx, r.Client, pod, annoMigrationAction, annoMigrationActionID, annoMigrationActionArgs)
}

func migrationEnded(phase migrationv1alpha1.SwiftMigrationPhase) bool {
	switch phase {
	case migrationv1alpha1.SwiftMigrationPhaseCompleted,
		migrationv1alpha1.SwiftMigrationPhaseFailed,
		migrationv1alpha1.SwiftMigrationPhaseCancelled:
		return true
	}
	return false
}

// removeAnnotations deletes keys from the pod with a merge patch.
func removeAnnotations(ctx context.Context, c client.Client, pod *corev1.Pod, keys ...string) error {
	patch := client.MergeFrom(pod.DeepCopy())
	changed := false
	for _, k := range keys {
		if _, ok := pod.Annotations[k]; ok {
			delete(pod.Annotations, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return client.IgnoreNotFound(c.Patch(ctx, pod, patch))
}

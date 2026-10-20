package swiftmigration

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func controlledBy(guest *swiftv1alpha1.SwiftGuest) []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(guest,
		schema.GroupVersionKind{Group: swiftv1alpha1.GroupName, Version: swiftv1alpha1.Version, Kind: "SwiftGuest"})}
}

// #823: after cutover the destination pod is the guest's launcher, and the
// receive action left on it made swiftletd reject every later snapshot. A
// completed migration clears it, and only its own.
func TestCompletedMigrationClearsItsReceiveFromTheLauncher(t *testing.T) {
	for _, tc := range []struct {
		name      string
		actionID  string
		wantClear bool
	}{
		{"its own receive", "m1:recv:1", true},
		{"another migration's", "m0:recv:1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mig, guest, _, dst := stopAndCopyFixture(t, "uid-1")
			mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseCompleted
			guest.Status.PodRef = &corev1.ObjectReference{Kind: "Pod", Name: dst.Name, Namespace: dst.Namespace}
			dst.OwnerReferences = controlledBy(guest)
			stamp(dst, migrationActionVerbReceive, tc.actionID, "running", tc.actionID, "")
			dst.Annotations[AnnotationMigrationActionArgs] = "{}"
			r := newStopAndCopyReconciler(t, mig, guest, dst)
			ctx := context.Background()

			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
				t.Fatal(err)
			}
			var got corev1.Pod
			if err := r.Get(ctx, client.ObjectKeyFromObject(dst), &got); err != nil {
				t.Fatal(err)
			}
			_, set := got.Annotations[AnnotationMigrationAction]
			if set == tc.wantClear {
				t.Errorf("migration-action present = %v, want %v: %v", set, !tc.wantClear, got.Annotations)
			}
			if got.Annotations[AnnotationMigrationStatus] != "running" {
				t.Errorf("swiftletd's status was touched: %v", got.Annotations)
			}
		})
	}
}

// #823: a migration writing its action drops a snapshot action swiftletd has
// finished, which would otherwise get both rejected; an unfinished one stays.
func TestWriteMigrationActionDropsAFinishedSnapshotAction(t *testing.T) {
	for _, tc := range []struct {
		status   string
		wantKept bool
	}{
		{"ready", false},
		{"failed", false},
		{"running", true},
	} {
		t.Run(tc.status, func(t *testing.T) {
			mig, guest, src, _ := stopAndCopyFixture(t, "uid-1")
			src.Annotations[annoSnapshotAction] = "capture"
			src.Annotations[annoSnapshotActionID] = "snap-1"
			src.Annotations[annoSnapshotActionArgs] = "{}"
			src.Annotations[annoSnapshotStatus] = tc.status
			src.Annotations[annoSnapshotStatusID] = "snap-1"
			r := newStopAndCopyReconciler(t, mig, guest, src)
			ctx := context.Background()

			if err := r.writeMigrationAction(ctx, src, migrationActionVerbSend, sendActionID(mig), "{}"); err != nil {
				t.Fatal(err)
			}
			var got corev1.Pod
			if err := r.Get(ctx, client.ObjectKeyFromObject(src), &got); err != nil {
				t.Fatal(err)
			}
			if _, kept := got.Annotations[annoSnapshotAction]; kept != tc.wantKept {
				t.Errorf("snapshot action kept = %v, want %v: %v", kept, tc.wantKept, got.Annotations)
			}
			if got.Annotations[AnnotationMigrationActionID] != sendActionID(mig) {
				t.Errorf("send not written: %v", got.Annotations)
			}
			if got.Annotations[annoSnapshotStatus] != tc.status {
				t.Errorf("snapshot status was touched: %v", got.Annotations)
			}
		})
	}
}

package swiftsnapshot

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// readySnapTTL builds a Ready, finalizer-bearing s3 snapshot captured
// capturedAgo in the past with the given ttl.
func readySnapTTL(name, ns string, ttl, capturedAgo time.Duration) *snapshotv1alpha1.SwiftSnapshot {
	s := s3Snap(name, ns, nil)
	s.Finalizers = []string{S3ObjectFinalizer}
	s.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseReady
	captured := metav1.NewTime(time.Now().Add(-capturedAgo))
	s.Status.CapturedAt = &captured
	d := metav1.Duration{Duration: ttl}
	s.Spec.TTL = &d
	return s
}

func deletionStamped(t *testing.T, c client.Client, snap *snapshotv1alpha1.SwiftSnapshot) bool {
	t.Helper()
	var got snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(snap), &got); err != nil {
		if apierrors.IsNotFound(err) {
			return true // GC'd (no finalizer) also counts as "deleted"
		}
		t.Fatal(err)
	}
	return got.DeletionTimestamp != nil
}

func TestHandleRetention_NoTTL_NoOp(t *testing.T) {
	snap := s3Snap("snap1", "ns", nil)
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseReady
	r, c := newReconciler(t, snap)
	d, err := r.handleRetention(context.Background(), snap)
	if err != nil || d != 0 {
		t.Fatalf("no TTL should be a no-op; requeue=%v err=%v", d, err)
	}
	if deletionStamped(t, c, snap) {
		t.Error("no-TTL snapshot must not be deleted")
	}
}

func TestHandleRetention_NotExpired_RequeuesCapped(t *testing.T) {
	// 48h ttl, captured now -> remaining ~48h, capped to 1h.
	snap := readySnapTTL("snap1", "ns", 48*time.Hour, 0)
	r, c := newReconciler(t, snap)
	d, err := r.handleRetention(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if d <= 0 || d > retentionMaxRequeue {
		t.Errorf("requeue = %v, want in (0, %v]", d, retentionMaxRequeue)
	}
	if deletionStamped(t, c, snap) {
		t.Error("not-yet-expired snapshot must not be deleted")
	}
}

func TestHandleRetention_Expired_Unreferenced_Deletes(t *testing.T) {
	snap := readySnapTTL("snap1", "ns", time.Minute, 2*time.Minute) // expired
	r, c := newReconciler(t, snap)
	d, err := r.handleRetention(context.Background(), snap)
	if err != nil || d != 0 {
		t.Fatalf("expired+unreferenced should delete; requeue=%v err=%v", d, err)
	}
	if !deletionStamped(t, c, snap) {
		t.Error("expired unreferenced snapshot should be deleted (DeletionTimestamp set)")
	}
}

func TestHandleRetention_Expired_BlockedByCloneGuest(t *testing.T) {
	snap := readySnapTTL("snap1", "ns", time.Minute, 2*time.Minute)
	cloneGuest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-x", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			CloneFromSnapshot: &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "snap1"}},
		},
	}
	r, c := newReconciler(t, snap, cloneGuest)
	d, err := r.handleRetention(context.Background(), snap)
	if err != nil || d != retentionBlockedRequeue {
		t.Fatalf("referenced snapshot should defer; requeue=%v err=%v", d, err)
	}
	if deletionStamped(t, c, snap) {
		t.Error("a referenced snapshot must NOT be deleted")
	}
	var got snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(snap), &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.SwiftSnapshotConditionRetentionBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("RetentionBlocked condition should be True; got %+v", cond)
	}
}

func TestHandleRetention_Expired_InFlightRestoreBlocks_TerminalDoesNot(t *testing.T) {
	mkRestore := func(phase snapshotv1alpha1.SwiftRestorePhase) *snapshotv1alpha1.SwiftRestore {
		return &snapshotv1alpha1.SwiftRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "rst-x", Namespace: "ns"},
			Spec:       snapshotv1alpha1.SwiftRestoreSpec{SnapshotRef: snapshotv1alpha1.SwiftRestoreSnapshotRef{Name: "snap1"}},
			Status:     snapshotv1alpha1.SwiftRestoreStatus{Phase: phase},
		}
	}
	// In-flight restore blocks.
	snap := readySnapTTL("snap1", "ns", time.Minute, 2*time.Minute)
	r, c := newReconciler(t, snap, mkRestore(snapshotv1alpha1.SwiftRestorePhaseRestoring))
	d, err := r.handleRetention(context.Background(), snap)
	if err != nil || d != retentionBlockedRequeue {
		t.Fatalf("in-flight restore should defer; requeue=%v err=%v", d, err)
	}
	if deletionStamped(t, c, snap) {
		t.Error("in-flight restore must block deletion")
	}

	// Terminal (Ready) restore does NOT block.
	snap2 := readySnapTTL("snap2", "ns", time.Minute, 2*time.Minute)
	rst2 := mkRestore(snapshotv1alpha1.SwiftRestorePhaseReady)
	rst2.Name = "rst-done"
	rst2.Spec.SnapshotRef.Name = "snap2"
	r2, c2 := newReconciler(t, snap2, rst2)
	d2, err := r2.handleRetention(context.Background(), snap2)
	if err != nil || d2 != 0 {
		t.Fatalf("terminal restore should not block; requeue=%v err=%v", d2, err)
	}
	if !deletionStamped(t, c2, snap2) {
		t.Error("a terminal restore must not block TTL deletion")
	}
}

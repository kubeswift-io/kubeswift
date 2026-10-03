package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// An s3 or oci snapshot that does not say where it is stored is admitted when
// the webhook is off (the default). The capture ran first and the block was
// read afterwards, so the reconcile dereferenced nil on every pass with the
// guest already captured (#706). It now fails at once, before the phase
// machine runs.
func TestReconcile_BackendWithoutDestinationFailsFirst(t *testing.T) {
	cases := map[string]struct {
		mut  func(*snapshotv1alpha1.SwiftSnapshot)
		want string
	}{
		"oci without its block":       {func(s *snapshotv1alpha1.SwiftSnapshot) { s.Spec.Backend.OCI = nil }, "spec.backend.oci is required"},
		"oci without a repository":    {func(s *snapshotv1alpha1.SwiftSnapshot) { s.Spec.Backend.OCI.Repository = "" }, "spec.backend.oci.repository is required"},
		"oci tag that is a reference": {func(s *snapshotv1alpha1.SwiftSnapshot) { s.Spec.Backend.OCI.Tag = "repo:v1" }, "must be a bare tag"},
		"s3 without its block": {func(s *snapshotv1alpha1.SwiftSnapshot) {
			s.Spec.Backend = snapshotv1alpha1.SwiftSnapshotBackend{Type: snapshotv1alpha1.SnapshotBackendS3}
		}, "spec.backend.s3 is required"},
		"s3 without a bucket": {func(s *snapshotv1alpha1.SwiftSnapshot) {
			s.Spec.Backend = snapshotv1alpha1.SwiftSnapshotBackend{Type: snapshotv1alpha1.SnapshotBackendS3, S3: &snapshotv1alpha1.S3Backend{}}
		}, "spec.backend.s3.bucket is required"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			snap := ociSnap(nil)
			c.mut(snap)
			r, cl := newReconciler(t, snap)
			key := types.NamespacedName{Namespace: snap.Namespace, Name: snap.Name}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			var got snapshotv1alpha1.SwiftSnapshot
			if err := cl.Get(context.Background(), key, &got); err != nil {
				t.Fatal(err)
			}
			if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseFailed {
				t.Fatalf("phase = %q, want Failed", got.Status.Phase)
			}
			var msg string
			for _, cond := range got.Status.Conditions {
				if cond.Type == "Ready" {
					msg = cond.Message
				}
			}
			if !strings.Contains(msg, c.want) {
				t.Errorf("Ready message = %q, want it to contain %q", msg, c.want)
			}
		})
	}
}

// A well-formed oci snapshot passes the guard and goes on to the phase machine.
func TestReconcile_BackendWithDestinationProceeds(t *testing.T) {
	snap := ociSnap(nil)
	r, cl := newReconciler(t, snap)
	key := types.NamespacedName{Namespace: snap.Namespace, Name: snap.Name}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got snapshotv1alpha1.SwiftSnapshot
	if err := cl.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		t.Errorf("a well-formed oci snapshot failed: %+v", got.Status.Conditions)
	}
}

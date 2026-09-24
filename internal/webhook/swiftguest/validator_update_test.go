package swiftguest

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// A guest admitted under an older, wider hostPath allowlist no longer
// validates once the allowlist narrows. Updates that do not touch its spec --
// the controllers removing their finalizers, annotation changes -- must still
// go through, or a deleted guest (and its namespace) stays Terminating.
func TestValidateUpdate_UnchangedSpecOrDeletionIsNotRevalidated(t *testing.T) {
	hp := "/srv/old-share"
	old := guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Finalizers = []string{"kubeswift.io/storage"}
		g.Spec.Filesystems = []swiftv1alpha1.Filesystem{{
			Name: "share", Source: swiftv1alpha1.FilesystemSource{HostPath: &hp},
		}}
	})
	v := &Validator{AllowedHostPathPrefixes: []string{"/srv/vm"}} // narrowed since admission
	ctx := context.Background()

	if _, err := v.ValidateCreate(ctx, old); err == nil {
		t.Fatal("fixture should fail validation under the narrowed allowlist")
	}

	now := metav1.Now()
	deleting := old.DeepCopy()
	deleting.DeletionTimestamp = &now
	unfinalized := deleting.DeepCopy()
	unfinalized.Finalizers = nil
	if _, err := v.ValidateUpdate(ctx, deleting, unfinalized); err != nil {
		t.Errorf("finalizer removal on a deleting guest was rejected: %v", err)
	}

	annotated := old.DeepCopy()
	annotated.Annotations = map[string]string{"example.com/x": "y"}
	if _, err := v.ValidateUpdate(ctx, old, annotated); err != nil {
		t.Errorf("a metadata-only update was rejected: %v", err)
	}

	// A spec change is still validated in full.
	changed := old.DeepCopy()
	changed.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	if _, err := v.ValidateUpdate(ctx, old, changed); err == nil {
		t.Error("a spec change must be validated against the current rules")
	}
}

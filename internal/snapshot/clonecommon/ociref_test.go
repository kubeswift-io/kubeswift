package clonecommon

import (
	"context"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func ociRefSnap(ns, name, repo, tag string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendOCI,
				OCI:  &snapshotv1alpha1.OCIBackend{Repository: repo, Tag: tag},
			},
		},
	}
}

func TestOCIReference(t *testing.T) {
	if got := OCIReference(ociRefSnap("team-a", "s1", "reg/vm", "")); got != "reg/vm:team-a-s1" {
		t.Errorf("default tag: %q", got)
	}
	if got := OCIReference(ociRefSnap("team-a", "s1", "reg/vm", "nightly")); got != "reg/vm:nightly" {
		t.Errorf("explicit tag: %q", got)
	}
	pushed := ociRefSnap("team-a", "s1", "reg/edited", "nightly")
	pushed.Status.OCI = &snapshotv1alpha1.OCISnapshotStatus{Reference: "reg/vm:nightly"}
	if got := OCIReference(pushed); got != "reg/vm:nightly" {
		t.Errorf("a pushed snapshot is where it pushed, not what its spec says now: %q", got)
	}
	if got := OCIReference(ociRefSnap("team-a", "s1", "", "")); got != "" {
		t.Errorf("no repository: %q", got)
	}
	local := ociRefSnap("team-a", "s1", "reg/vm", "")
	local.Spec.Backend.Type = snapshotv1alpha1.SnapshotBackendLocal
	if got := OCIReference(local); got != "" {
		t.Errorf("not oci: %q", got)
	}
}

// Only live snapshots in the same namespace that push to the same
// repository:tag hold it (#705).
func TestOCIReferenceHolders(t *testing.T) {
	s := runtime.NewScheme()
	gv := schema.GroupVersion{Group: "snapshot.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gv, &snapshotv1alpha1.SwiftSnapshot{}, &snapshotv1alpha1.SwiftSnapshotList{})
	metav1.AddToGroupVersion(s, gv)

	same := ociRefSnap("team-a", "same", "reg/vm", "nightly")
	otherTag := ociRefSnap("team-a", "other-tag", "reg/vm", "weekly")
	otherRepo := ociRefSnap("team-a", "other-repo", "reg/other", "nightly")
	otherNS := ociRefSnap("team-b", "other-ns", "reg/vm", "nightly")
	deleting := ociRefSnap("team-a", "deleting", "reg/vm", "nightly")
	deleting.Finalizers = []string{"kubeswift.io/test"}
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	failed := ociRefSnap("team-a", "failed", "reg/vm", "nightly")
	failed.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseFailed
	failedPushed := ociRefSnap("team-a", "failed-pushed", "reg/vm", "nightly")
	failedPushed.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseFailed
	failedPushed.Status.OCI = &snapshotv1alpha1.OCISnapshotStatus{Reference: "reg/vm:nightly"}
	self := ociRefSnap("team-a", "self", "reg/vm", "nightly")

	objs := []client.Object{same, otherTag, otherRepo, otherNS, deleting, failed, failedPushed, self}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&snapshotv1alpha1.SwiftSnapshot{}).Build()

	holders, err := OCIReferenceHolders(context.Background(), c, self)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range holders {
		names = append(names, h.Name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "failed-pushed,same" {
		t.Errorf("holders = %s, want failed-pushed,same", got)
	}
}

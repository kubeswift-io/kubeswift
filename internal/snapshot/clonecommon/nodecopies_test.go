package clonecommon

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func TestRecordNodeCopy(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(schema.GroupVersion{Group: "snapshot.kubeswift.io", Version: "v1alpha1"},
		&snapshotv1alpha1.SwiftSnapshot{}, &snapshotv1alpha1.SwiftSnapshotList{})
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Group: "snapshot.kubeswift.io", Version: "v1alpha1"})
	snap := &snapshotv1alpha1.SwiftSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}}
	snap.Status.NodeName = "capture"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).WithStatusSubresource(snap).Build()
	ctx := context.Background()
	get := func() *snapshotv1alpha1.SwiftSnapshot {
		var s snapshotv1alpha1.SwiftSnapshot
		if err := c.Get(ctx, client.ObjectKeyFromObject(snap), &s); err != nil {
			t.Fatal(err)
		}
		return &s
	}

	for _, node := range []string{"capture", "worker-2", "worker-2", "worker-3", ""} {
		if err := RecordNodeCopy(ctx, c, get(), node); err != nil {
			t.Fatalf("record %q: %v", node, err)
		}
	}
	if got := get().Status.NodeCopies; !slices.Equal(got, []string{"worker-2", "worker-3"}) {
		t.Errorf("nodeCopies = %v; the capture node is not a copy, and each node is listed once", got)
	}

	// A stale read must not drop an entry another writer added.
	stale := get()
	if err := RecordNodeCopy(ctx, c, get(), "worker-4"); err != nil {
		t.Fatal(err)
	}
	if err := RecordNodeCopy(ctx, c, stale, "worker-5"); err == nil {
		t.Error("a write from a stale read must conflict, not overwrite")
	}
}

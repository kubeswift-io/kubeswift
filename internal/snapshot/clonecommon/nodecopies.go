package clonecommon

import (
	"context"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// RecordNodeCopy records on the snapshot that node is about to hold a
// downloaded copy of its artifacts (status.nodeCopies), so deleting the
// snapshot removes that copy too. Call it before creating the download Job:
// a copy is guest memory, and one the snapshot does not know about outlives
// it on the node's disk. The capture node is not recorded; it is
// status.nodeName, which the deletion already cleans.
//
// The write is optimistic-locked: two clones recording different nodes at
// once must not drop each other's entry. A conflict is returned for the
// caller's requeue.
func RecordNodeCopy(ctx context.Context, c client.Client, snap *snapshotv1alpha1.SwiftSnapshot, node string) error {
	if node == "" || node == snap.Status.NodeName || slices.Contains(snap.Status.NodeCopies, node) {
		return nil
	}
	patch := client.MergeFromWithOptions(snap.DeepCopy(), client.MergeFromWithOptimisticLock{})
	snap.Status.NodeCopies = append(snap.Status.NodeCopies, node)
	return c.Status().Patch(ctx, snap, patch)
}

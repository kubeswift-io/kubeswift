package swiftguestpool

import (
	"context"
	"errors"
	"sort"

	corev1 "k8s.io/api/core/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// errNoSchedulableNodes is returned when a cloneFromSnapshot pool replica needs
// a target node but no Ready, schedulable worker node exists.
var errNoSchedulableNodes = errors.New("no schedulable worker nodes for cloneFromSnapshot pool replica")

// assignCloneTargetNode pins a cloneFromSnapshot pool replica's targetNode for
// the Tier C (s3) path: the replica's download Job + restore-receive launcher
// must be co-located on a chosen node, decided BEFORE the guest is created
// (Snapshot Phase 4 design OQ1 — pre-assign, not float-then-pin). Replicas are
// round-robined across the schedulable worker nodes by ordinal, spreading them
// (replicas ≤ nodes → one per node). Tier B clones ignore targetNode (the
// SwiftGuest controller pins them to the capture node), so setting it for any
// cloneFromSnapshot template is harmless.
//
// NOTE (Phase 4 follow-up): a pool with replicas > nodes places multiple
// replicas on one node, whose per-guest Tier C download Jobs would write the
// same snapshot-keyed node cache concurrently. The download dedup (a shared
// per-(node,snapshot) Job) is a tracked follow-up; until then keep
// cloneFromSnapshot pools at replicas ≤ schedulable nodes.
func (r *SwiftGuestPoolReconciler) assignCloneTargetNode(
	ctx context.Context, spec *swiftv1alpha1.SwiftGuestSpec, index int,
) error {
	if spec.CloneFromSnapshot == nil {
		return nil
	}
	nodes, err := r.schedulableWorkerNodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errNoSchedulableNodes
	}
	spec.CloneFromSnapshot.TargetNode = nodes[index%len(nodes)]
	return nil
}

// schedulableWorkerNodes returns the names of Ready, schedulable, non-control-
// plane nodes, sorted for deterministic round-robin assignment.
func (r *SwiftGuestPoolReconciler) schedulableWorkerNodes(ctx context.Context) ([]string, error) {
	var list corev1.NodeList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	var names []string
	for i := range list.Items {
		n := &list.Items[i]
		if n.Spec.Unschedulable || isControlPlaneNode(n) || !nodeIsReady(n) {
			continue
		}
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return names, nil
}

func isControlPlaneNode(n *corev1.Node) bool {
	if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}
	if _, ok := n.Labels["node-role.kubernetes.io/master"]; ok {
		return true
	}
	for _, t := range n.Spec.Taints {
		if t.Key == "node-role.kubernetes.io/control-plane" || t.Key == "node-role.kubernetes.io/master" {
			return true
		}
	}
	return false
}

func nodeIsReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

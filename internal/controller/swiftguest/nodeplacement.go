package swiftguest

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// pinnedNode returns the node a guest's launcher must run on, and what pinned
// it there, for messages.
//
// For a guest without a shared-base disk this is exactly spec.nodeName, as it
// always was — no behaviour changes for anyone else.
//
// A shared-base disk is node-local, so for a guest that has one the disk's node
// WINS, and every other pin must agree with it. A disagreement is an error, not
// a choice. Obeying spec.nodeName would start the guest where its disk is not:
// that node has no record of it, so the launcher's reactivate step refuses to
// start rather than build a pristine disk, and the guest fails there. Obeying
// the disk would silently ignore an explicit request. So the guest is held, with
// the reason, until the pins agree.
func pinnedNode(guest *swiftv1alpha1.SwiftGuest) (node, source string, err error) {
	disk := ""
	if guest.Status.SharedBaseDisk != nil {
		disk = guest.Status.SharedBaseDisk.Node
	}
	if disk == "" {
		if guest.Spec.NodeName != "" {
			return guest.Spec.NodeName, "spec.nodeName", nil
		}
		return "", "", nil
	}
	if s := guest.Spec.NodeName; s != "" && s != disk {
		return "", "", fmt.Errorf(
			"spec.nodeName=%q, but this guest's shared-base root disk lives on node %q "+
				"(status.sharedBaseDisk.node) and cannot move; node %q has no copy of it, so the "+
				"guest cannot start there. Set spec.nodeName to %q or clear it",
			s, disk, s, disk)
	}
	if g := guest.Status.GPU; g != nil && g.NodeName != "" && g.NodeName != disk {
		return "", "", fmt.Errorf(
			"the GPU allocated to this guest is on node %q, but its shared-base root disk lives on "+
				"node %q (status.sharedBaseDisk.node) and cannot move; the guest can only run where "+
				"both are",
			g.NodeName, disk)
	}
	return disk, "status.sharedBaseDisk.node", nil
}

// checkNodePlacement enforces the taint/toleration check that the scheduler
// would have run, for guests that pin a node.
//
// The launcher pod is bound by setting pod.Spec.NodeName directly rather than
// via a kubernetes.io/hostname nodeSelector. That is deliberate and documented
// on applyNodeName: direct binding gives fast kubelet-time rejection on a bad
// fit, which the SwiftMigration controller relies on for clean failure
// detection, and NodeName is immutable post-binding, which the StopAndCopy
// delete-and-recreate contract depends on.
//
// The security cost of that choice is that direct binding SKIPS THE SCHEDULER
// ENTIRELY, and kubelet admission has no taint predicate. So
// node-role.kubernetes.io/control-plane:NoSchedule does not stop a pinned pod
// from landing on a control plane node -- and the launcher is privileged, so
// that is node-root on a control plane. A namespaced tenant able to create a
// SwiftGuest could pick the node.
//
// Rather than change the binding mechanism (which would regress migration
// failure detection), this reproduces the one scheduler predicate that
// mattered. Guests that legitimately target a tainted node still can: they
// carry a matching toleration, exactly as a normal pod would.
//
// Only NoSchedule and NoExecute are considered. PreferNoSchedule is a soft
// preference the scheduler weighs; it never blocks placement, so enforcing it
// here would be stricter than Kubernetes itself.
func checkNodePlacement(ctx context.Context, c client.Reader, guest *swiftv1alpha1.SwiftGuest, pod *corev1.Pod) error {
	return checkNodePlacementFor(ctx, c, guest, pod.Spec.Tolerations)
}

// checkNodePlacementFor is the same check against a toleration set directly,
// so it can run BEFORE the pod is built.
//
// That ordering is the point (issue #444): a guest pinned to an unschedulable
// node used to stall with no diagnosis, because the root-disk clone Job is
// created first and pins to the SAME node, its pod sits Pending forever, and
// the reconcile never reaches buildPod where this check lived. The operator saw
// a guest stuck in Scheduling with nothing saying why.
//
// It decides whether a NEW launcher may be created there. It says nothing about
// a launcher already running: a cordon or taint added later does not evict it,
// and Reconcile does not consult this for one.
func checkNodePlacementFor(ctx context.Context, c client.Reader, guest *swiftv1alpha1.SwiftGuest, tolerations []corev1.Toleration) error {
	name, source, err := pinnedNode(guest)
	if err != nil {
		return err
	}
	if name == "" {
		return nil // not pinned; the scheduler runs normally and applies taints itself
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			if source == "status.sharedBaseDisk.node" {
				// Not a typo to correct: the disk is on that node and nowhere
				// else. Waiting is the only safe answer — starting elsewhere
				// would boot the guest on an empty disk.
				return fmt.Errorf("this guest's shared-base root disk lives on node %q, which does not "+
					"exist; the disk is node-local and cannot move, so the guest waits for the node "+
					"to return. If the node is gone for good, so is the disk: delete and recreate "+
					"the guest", name)
			}
			return fmt.Errorf("%s=%q does not exist", source, name)
		}
		return fmt.Errorf("resolve %s=%q: %w", source, name, err)
	}
	// A cordon is spec.unschedulable, which the node lifecycle controller then
	// mirrors as a taint. Either one alone counts: the taint lags behind both the
	// cordon and the uncordon. It is routine and it passes, so it reads as a
	// cordon, not as a taint the guest cannot tolerate -- and only once no other
	// taint blocks, so an uncordon is not promised to fix a node it will not.
	cordon := corev1.Taint{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}
	cordoned := node.Spec.Unschedulable && !tolerated(cordon, tolerations)
	var others []corev1.Taint
	for _, taint := range node.Spec.Taints {
		if taint.Key != corev1.TaintNodeUnschedulable {
			others = append(others, taint)
		} else if _, blocks := untoleratedTaint([]corev1.Taint{taint}, tolerations); blocks {
			cordoned = true
		}
	}
	if t, ok := untoleratedTaint(others, tolerations); ok {
		// NB: SwiftGuest has no spec.tolerations field — the launcher pod is
		// built with none — so in practice a pinned guest cannot target a
		// NoSchedule/NoExecute node at all. The message says that, rather than
		// pointing at a field that does not exist (which the first version of
		// this check did).
		if source == "status.sharedBaseDisk.node" {
			// Re-pinning is not an option: the disk cannot follow the guest.
			return fmt.Errorf(
				"this guest's shared-base root disk lives on node %q, which has taint %s=%s:%s, and a "+
					"guest cannot tolerate taints; the disk cannot move, so remove the taint",
				name, t.Key, t.Value, t.Effect)
		}
		return fmt.Errorf(
			"%s=%q has taint %s=%s:%s and a guest cannot tolerate taints; "+
				"pin the guest to an untainted node, or remove the taint",
			source, name, t.Key, t.Value, t.Effect)
	}
	if cordoned {
		return fmt.Errorf("%s=%q is cordoned; the guest starts once the node is uncordoned", source, name)
	}
	return nil
}

// nodePlacementRetry is how often a guest held by its pinned node looks again.
// Nothing watches Nodes, so this is what starts the guest once the node is
// uncordoned or the taint is removed.
const nodePlacementRetry = 30 * time.Second

// holdForNodePlacement parks a pinned guest whose node cannot take a new
// launcher: Pending, with the reason on PodScheduled, retried. It waits rather
// than fails, like a pod whose only eligible node is cordoned. Cordons, pressure
// taints and NotReady come and go, and Failed would count a VM failure and make
// a SwiftGuestPool delete the guest. One that never gets placed still alerts, as
// KubeSwiftGuestStuckPending.
func (r *SwiftGuestReconciler) holdForNodePlacement(ctx context.Context, guest *swiftv1alpha1.SwiftGuest,
	status *swiftv1alpha1.SwiftGuestStatus, cause error) (ctrl.Result, error) {
	status.Phase = swiftv1alpha1.SwiftGuestPhasePending
	SetPodScheduledCondition(status, nil, false, cause.Error())
	recordGuestMetrics(guest, &guest.Status, status, nil)
	if err := r.patchStatus(ctx, guest, status); err != nil {
		return ctrl.Result{}, err
	}
	node, _, _ := pinnedNode(guest)
	log.FromContext(ctx).Info("waiting for the pinned node to take a launcher",
		"node", node, "reason", cause.Error())
	return ctrl.Result{RequeueAfter: nodePlacementRetry}, nil
}

// untoleratedTaint returns the first NoSchedule/NoExecute taint not tolerated.
func untoleratedTaint(taints []corev1.Taint, tolerations []corev1.Toleration) (corev1.Taint, bool) {
	for _, taint := range taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if !tolerated(taint, tolerations) {
			return taint, true
		}
	}
	return corev1.Taint{}, false
}

// tolerated implements the Kubernetes toleration match: an empty Effect matches
// every effect, operator Exists matches any value, and an empty Key with
// operator Exists is the wildcard that tolerates everything.
func tolerated(taint corev1.Taint, tolerations []corev1.Toleration) bool {
	for _, tol := range tolerations {
		if tol.Effect != "" && tol.Effect != taint.Effect {
			continue
		}
		if tol.Key == "" {
			if tol.Operator == corev1.TolerationOpExists {
				return true // wildcard: tolerates every taint
			}
			continue
		}
		if tol.Key != taint.Key {
			continue
		}
		switch tol.Operator {
		case corev1.TolerationOpExists:
			return true
		case corev1.TolerationOpEqual, "":
			if tol.Value == taint.Value {
				return true
			}
		}
	}
	return false
}

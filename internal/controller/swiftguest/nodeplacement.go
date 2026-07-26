package swiftguest

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

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
	if guest.Spec.NodeName == "" {
		return nil // not pinned; the scheduler runs normally and applies taints itself
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: guest.Spec.NodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("spec.nodeName=%q does not exist", guest.Spec.NodeName)
		}
		return fmt.Errorf("resolve spec.nodeName=%q: %w", guest.Spec.NodeName, err)
	}
	if t, ok := untoleratedTaint(node.Spec.Taints, pod.Spec.Tolerations); ok {
		return fmt.Errorf(
			"spec.nodeName=%q has taint %s=%s:%s which this guest does not tolerate; "+
				"add a matching toleration to spec.tolerations to place a guest there",
			guest.Spec.NodeName, t.Key, t.Value, t.Effect)
	}
	return nil
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

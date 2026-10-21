package swiftsandbox

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// Resolved=False reasons while a pod by the cold launcher's name belongs to
// someone else (#772).
const (
	// reasonPreviousLauncherStopping: the pod is a previous launcher by this
	// name that is stopping or has stopped; this sandbox's launcher is
	// created once it is gone.
	reasonPreviousLauncherStopping = "PreviousLauncherStopping"
	// reasonLauncherPodConflict: a live pod by this name is not this
	// sandbox's; nothing is created until it is removed.
	reasonLauncherPodConflict = "LauncherPodConflict"
	// reasonIntentConfigMapConflict: a ConfigMap by the launcher's intent name
	// is not this sandbox's; the launcher is not created until it is removed.
	reasonIntentConfigMapConflict = "IntentConfigMapConflict"
	// reasonIntentMismatch: swiftletd found an intent other than the one its
	// pod was built for, and did not start the VM.
	reasonIntentMismatch = "IntentMismatch"
)

// foreignColdLauncher reports why the pod named sb.Name, if any, cannot be
// sb's cold launcher. A cold launcher is found by name, and a sandbox
// re-created under the same name used to take over the previous sandbox's
// pod: report its state, then fail when it exited, never starting its own
// workload. Only a pod this sandbox controls is its launcher. reason is ""
// when there is no pod by that name or it is sb's. Callers wait with
// waitForReference; the pod's own events requeue sb (its owner had sb's name).
func (r *SwiftSandboxReconciler) foreignColdLauncher(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (reason, msg string, err error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	return foreignPodReason(sb, &pod)
}

// foreignPodReason is foreignColdLauncher for a pod already read.
func foreignPodReason(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) (reason, msg string, err error) {
	if metav1.IsControlledBy(pod, sb) {
		return "", "", nil
	}
	if pod.DeletionTimestamp != nil || podFinished(pod) {
		return reasonPreviousLauncherStopping, fmt.Sprintf(
			"pod %s belongs to a previous launcher by this name; this sandbox's launcher is created once it is gone", pod.Name), nil
	}
	return reasonLauncherPodConflict, fmt.Sprintf(
		"pod %s exists and is not this sandbox's launcher; delete it, or re-create the sandbox under another name", pod.Name), nil
}

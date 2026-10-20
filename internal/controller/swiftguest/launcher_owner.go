package swiftguest

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// PodScheduled=False reasons while the pod by the guest's launcher name
// belongs to someone else (#819).
const (
	// reasonPreviousLauncherStopping: the pod is the launcher of a previous
	// guest by this name, stopping or stopped; this guest's launcher is
	// created once it is gone.
	reasonPreviousLauncherStopping = "PreviousLauncherStopping"
	// reasonLauncherPodConflict: a live pod by this name is not this guest's;
	// nothing is created until it is removed.
	reasonLauncherPodConflict = "LauncherPodConflict"
)

// foreignLauncherRetry backs up the pod watch while waiting: the foreign
// pod's events already requeue the guest, since its owner had the guest's name.
const foreignLauncherRetry = 5 * time.Second

// foreignLauncher reports why the pod at the guest's launcher name, if any,
// cannot be its launcher. The launcher is found by name, and a guest
// re-created under the name of one just deleted took over the old guest's
// stopping pod: it reported that VM's phase and IP as its own, then Failed
// when the pod exited, and only then started its own launcher. Every launcher
// of a guest, the migration destination included, is controlled by it, so a
// pod it does not control is never its launcher. reason is "" when there is
// no pod by that name or the guest controls it.
func (r *SwiftGuestReconciler) foreignLauncher(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (reason, msg string, err error) {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: canonicalPodName(guest)}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	reason, msg = foreignPodReason(guest, &pod)
	return reason, msg, nil
}

// foreignPodReason is foreignLauncher for a pod already read.
func foreignPodReason(guest *swiftv1alpha1.SwiftGuest, pod *corev1.Pod) (reason, msg string) {
	if metav1.IsControlledBy(pod, guest) {
		return "", ""
	}
	if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return reasonPreviousLauncherStopping, fmt.Sprintf(
			"pod %s belongs to a previous guest by this name; this guest's launcher is created once it is gone", pod.Name)
	}
	return reasonLauncherPodConflict, fmt.Sprintf(
		"pod %s exists and is not this guest's launcher; delete it, or re-create the guest under another name", pod.Name)
}

// holdForForeignLauncher keeps the guest Pending, with the reason on
// PodScheduled, and reports nothing of the foreign pod.
func (r *SwiftGuestReconciler) holdForForeignLauncher(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, reason, msg string) (ctrl.Result, error) {
	status := guest.Status.DeepCopy()
	status.Phase = swiftv1alpha1.SwiftGuestPhasePending
	setCondition(status, metav1.Condition{
		Type:    ConditionPodScheduled,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: msg,
	})
	recordGuestMetrics(guest, &guest.Status, status, nil)
	if err := r.patchStatus(ctx, guest, status); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("launcher name is held by a pod this guest does not control", "reason", reason, "message", msg)
	return ctrl.Result{RequeueAfter: foreignLauncherRetry}, nil
}

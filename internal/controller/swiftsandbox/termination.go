package swiftsandbox

import (
	"context"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

// sandboxTerminationFinalizer holds a deleted sandbox while its launcher pod
// stops. Without it the sandbox went at once and garbage collection deleted
// the launcher pod with the pod's own grace period, which a warm slot fixes
// when it is created, before any workload (and its drain) is known.
//
// It does not protect the NetworkPolicy: that has its own finalizer
// (launcherIsolationFinalizer), held until no pod it selects can run.
const sandboxTerminationFinalizer = "kubeswift.io/sandbox-termination"

// terminationSlack is how long past the launcher pod's grace period the
// sandbox waits for the pod to finish before it lets go anyway (the node may
// be unreachable, and then the pod stays until someone removes it).
const terminationSlack = 30 * time.Second

// Terminating condition reasons.
const (
	terminatingDraining     = "Draining"
	terminatingStopping     = "Stopping"
	terminatingLauncherGone = "LauncherGone"
)

// terminationIntent is spec.termination for swiftletd; nil without a drain.
func terminationIntent(sb *sandboxv1alpha1.SwiftSandbox) *runtimeintent.SandboxTerminationIntent {
	d := sb.Spec.DrainPeriod()
	if d <= 0 {
		return nil
	}
	return &runtimeintent.SandboxTerminationIntent{DrainMs: d.Milliseconds()}
}

// launcherGracePeriodSeconds is the termination grace period of the sandbox's
// launcher pod: its drain, then the time swiftletd has to stop the microVM.
func launcherGracePeriodSeconds(sb *sandboxv1alpha1.SwiftSandbox) int64 {
	return int64(math.Ceil(sb.Spec.TerminationGracePeriod().Seconds()))
}

// ensureTerminationFinalizer adds sandboxTerminationFinalizer before the
// sandbox gets a launcher. Sandboxes from before it existed get it on their
// next reconcile.
func (r *SwiftSandboxReconciler) ensureTerminationFinalizer(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) error {
	if controllerutil.ContainsFinalizer(sb, sandboxTerminationFinalizer) {
		return nil
	}
	return patchFinalizers(ctx, r.Client, sb, func() { controllerutil.AddFinalizer(sb, sandboxTerminationFinalizer) })
}

// podFinished is whether every container of pod has exited for good (a
// launcher pod never restarts).
func podFinished(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// terminateLauncher ends a deleted sandbox's launcher. The first pass marks the
// sandbox Terminating, sets the workload readiness gate False, and deletes the
// pod with the sandbox's own grace period (drain plus stop); swiftletd drains
// and stops the microVM on the SIGTERM that follows. Later passes only watch.
// Every decision is read from the pod, so a controller restart picks up where
// it was. done is true once the launcher has finished or is gone, or its grace
// period plus terminationSlack has passed.
func (r *SwiftSandboxReconciler) terminateLauncher(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (bool, ctrl.Result, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: launcherPodName(sb)}, &pod)
	switch {
	case apierrors.IsNotFound(err):
		return true, ctrl.Result{}, r.setTerminating(ctx, sb, terminatingLauncherGone, "the launcher pod is gone")
	case err != nil:
		return false, ctrl.Result{}, err
	}
	// Not this sandbox's launcher (a slot it never finished claiming, or a
	// pod by its name from something else): nothing of ours to stop.
	if !metav1.IsControlledBy(&pod, sb) || podFinished(&pod) {
		return true, ctrl.Result{}, nil
	}
	if pod.DeletionTimestamp == nil {
		grace := launcherGracePeriodSeconds(sb)
		drain := sb.Spec.DrainPeriod()
		if err := r.setTerminating(ctx, sb, terminatingDraining, fmt.Sprintf(
			"out of Service endpoints; the workload runs for %s, then the microVM stops", drain)); err != nil {
			return false, ctrl.Result{}, err
		}
		if err := r.withdrawReadiness(ctx, sb, &pod); err != nil {
			return false, ctrl.Result{}, err
		}
		uid := pod.UID
		if err := r.Delete(ctx, &pod, client.GracePeriodSeconds(grace),
			client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return false, ctrl.Result{}, err
		}
		klog.FromContext(ctx).Info("sandbox terminating: launcher pod deleted",
			"pod", pod.Name, "drain", drain, "gracePeriodSeconds", grace)
		return false, ctrl.Result{RequeueAfter: drain + time.Second}, nil
	}

	// The pod is terminating, deleted by this controller or by anyone else.
	// It started at deletionTimestamp - deletionGracePeriodSeconds.
	gracePeriod := time.Duration(0)
	if pod.DeletionGracePeriodSeconds != nil {
		gracePeriod = time.Duration(*pod.DeletionGracePeriodSeconds) * time.Second
	}
	started := pod.DeletionTimestamp.Add(-gracePeriod)
	drainEnd := started.Add(sb.Spec.DrainPeriod())
	deadline := pod.DeletionTimestamp.Add(terminationSlack)
	now := time.Now()
	if !now.Before(deadline) {
		// The NetworkPolicy stays until the pod is gone; only the sandbox
		// object is let go.
		klog.FromContext(ctx).Info("sandbox terminating: launcher pod still present after its grace period; releasing the sandbox",
			"pod", pod.Name, "deletionTimestamp", pod.DeletionTimestamp.Time)
		return true, ctrl.Result{}, nil
	}
	if now.Before(drainEnd) {
		return false, ctrl.Result{RequeueAfter: drainEnd.Sub(now)}, nil
	}
	if err := r.setTerminating(ctx, sb, terminatingStopping, "the drain is over; stopping the microVM"); err != nil {
		return false, ctrl.Result{}, err
	}
	return false, ctrl.Result{RequeueAfter: deadline.Sub(now)}, nil
}

// setTerminating records the termination phase on the sandbox and, once,
// reports its workload not ready.
func (r *SwiftSandboxReconciler) setTerminating(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, reason, msg string) error {
	if c := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionTerminating); c != nil && c.Reason == reason {
		return nil
	}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionTerminating, Status: metav1.ConditionTrue,
		Reason: reason, Message: msg, ObservedGeneration: sb.Generation,
	})
	if apimeta.IsStatusConditionTrue(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady) {
		apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
			Type: sandboxv1alpha1.SwiftSandboxConditionWorkloadReady, Status: metav1.ConditionFalse,
			Reason: "Terminating", Message: "the sandbox is being deleted", ObservedGeneration: sb.Generation,
		})
	}
	return r.Status().Update(ctx, sb)
}

// withdrawReadiness sets the launcher pod's workload readiness gate False
// before the pod is deleted, so the pod is not Ready and its endpoint is not
// serving, whoever wrote the gate; kube-proxy otherwise keeps sending new
// connections to a serving terminating endpoint when no other is ready. It
// must come first: the kubelet does not recompute Ready for a pod that is
// already terminating. The price is that the kubelet's SIGTERM, and so the
// drain, can wait for its next relist (up to 1 s) after this write.
func (r *SwiftSandboxReconciler) withdrawReadiness(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) error {
	if !hasWorkloadGate(pod) {
		return nil
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == WorkloadReadyGate && c.Status == corev1.ConditionFalse {
			return nil
		}
	}
	msg := "the sandbox is being deleted"
	if id, warm := pod.Annotations[annSandboxExecActionID]; warm {
		msg = workloadPrefix(id) + msg
	}
	orig := pod.DeepCopy()
	cond := corev1.PodCondition{Type: WorkloadReadyGate, Status: corev1.ConditionFalse,
		LastTransitionTime: metav1.Now(), Reason: "Terminating", Message: msg}
	replaced := false
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == WorkloadReadyGate {
			pod.Status.Conditions[i], replaced = cond, true
		}
	}
	if !replaced {
		pod.Status.Conditions = append(pod.Status.Conditions, cond)
	}
	if err := r.Status().Patch(ctx, pod, client.StrategicMergeFrom(orig)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

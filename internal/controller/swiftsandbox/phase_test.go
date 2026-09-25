package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// A one-shot sandbox's launcher pod through one launch, as the kubelet reports
// it. The kubelet publishes the launcher container's exit before it moves the
// pod to Succeeded/Failed: it keeps the pod Running until it has stopped it, a
// few seconds later. The sandbox fell through to Materializing in that window,
// and a finishing sandbox read Running -> Materializing -> Completed (lab,
// round 4). Each step must leave the phase where it was or move it forward,
// and the outcome still comes from the terminal pod.
func TestSandboxReconcile_PhaseOnlyMovesForward(t *testing.T) {
	materialized := corev1.ContainerStatus{Name: materializeInitName, State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"},
	}}
	type step struct {
		name     string
		phase    corev1.PodPhase
		init     corev1.ContainerStatus
		launcher corev1.ContainerStatus
		want     sandboxv1alpha1.SwiftSandboxPhase
	}
	for _, tc := range []struct {
		exitCode int32
		podPhase corev1.PodPhase
		want     sandboxv1alpha1.SwiftSandboxPhase
	}{
		{0, corev1.PodSucceeded, sandboxv1alpha1.SwiftSandboxCompleted},
		{3, corev1.PodFailed, sandboxv1alpha1.SwiftSandboxFailed},
	} {
		exited := corev1.ContainerStatus{Name: launcherName, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: tc.exitCode},
		}}
		steps := []step{
			{"materializing", corev1.PodPending,
				corev1.ContainerStatus{Name: materializeInitName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				corev1.ContainerStatus{Name: launcherName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
				sandboxv1alpha1.SwiftSandboxMaterializing},
			{"launcher running", corev1.PodRunning, materialized,
				corev1.ContainerStatus{Name: launcherName, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				sandboxv1alpha1.SwiftSandboxRunning},
			{"launcher exited, pod still Running", corev1.PodRunning, materialized, exited,
				sandboxv1alpha1.SwiftSandboxRunning},
			{"pod terminal", tc.podPhase, materialized, exited, tc.want},
		}

		r, c := sandboxReconciler(plainSandbox(testImage(t)), readyKernel("default", defaultKernelProfile))
		reconcileSB(t, r, "sb") // creates the launcher pod
		for _, s := range steps {
			var pod corev1.Pod
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
				t.Fatal(err)
			}
			pod.Status = corev1.PodStatus{
				Phase:                 s.phase,
				InitContainerStatuses: []corev1.ContainerStatus{s.init},
				ContainerStatuses:     []corev1.ContainerStatus{s.launcher},
			}
			if err := c.Status().Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
			reconcileSB(t, r, "sb")
			if got := getSandbox(t, c, "sb").Status.Phase; got != s.want {
				t.Errorf("exit %d, %s: phase = %q, want %q", tc.exitCode, s.name, got, s.want)
			}
		}
	}
}

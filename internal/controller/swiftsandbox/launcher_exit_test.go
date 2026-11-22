package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// exitedLauncher is a launcher pod still Running (its console container
// lives on) whose launcher container has exited with code and message.
func exitedLauncher(sb *sandboxv1alpha1.SwiftSandbox, code int32, msg string) *corev1.Pod {
	pod := runningLauncher(sb, nil)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: launcherName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: "Error", Message: msg}}},
		{Name: ConsoleContainerName, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}
	return pod
}

// A launcher that exited non-zero fails the sandbox at once, with the
// launcher's termination message, even while the pod is still Running.
func TestReconcile_LauncherFailedWhilePodRunning(t *testing.T) {
	sb := probedSandbox()
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxRunning
	msg := "the primary NIC's dnsmasq exited at start, so the guest would get no address: dnsmasq: failed to create inotify: Too many open files"
	r, c := sandboxReconciler(sb, exitedLauncher(sb, 1, msg))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || !strings.Contains(got.Status.Message, "dnsmasq exited at start") {
		t.Errorf("want Failed with the launcher's message, got %s: %q", got.Status.Phase, got.Status.Message)
	}
	if got.Status.ExitCode == nil || *got.Status.ExitCode != 1 {
		t.Errorf("exit code %v, want 1", got.Status.ExitCode)
	}
}

// A launcher that exited 0 while the pod is still Running is a sandbox
// finishing: it waits for the pod's terminal phase, as before.
func TestReconcile_LauncherExitedZeroWaitsForPod(t *testing.T) {
	sb := probedSandbox()
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxRunning
	r, c := sandboxReconciler(sb, exitedLauncher(sb, 0, ""))
	reconcileSB(t, r, "sb")
	if got := getSandbox(t, c, "sb"); got.Status.Phase != sandboxv1alpha1.SwiftSandboxRunning {
		t.Errorf("got %s: %q, want Running until the pod ends", got.Status.Phase, got.Status.Message)
	}
}

// A warm slot whose launcher failed has ended even while its pod is Running.
func TestSlotEnded(t *testing.T) {
	sb := probedSandbox()
	running := runningLauncher(sb, nil)
	failed := exitedLauncher(sb, 1, "dnsmasq exited at start")
	finishing := exitedLauncher(sb, 0, "")
	succeeded := runningLauncher(sb, nil)
	succeeded.Status.Phase = corev1.PodSucceeded
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want bool
	}{
		"running":                        {running, false},
		"launcher failed, pod Running":   {failed, true},
		"launcher exited 0, pod Running": {finishing, false},
		"pod succeeded":                  {succeeded, true},
	} {
		if got := slotEnded(tc.pod); got != tc.want {
			t.Errorf("%s: slotEnded = %v, want %v", name, got, tc.want)
		}
	}
	if msg := slotEndMessage(failed); !strings.Contains(msg, "dnsmasq exited at start") {
		t.Errorf("slotEndMessage = %q, want the launcher's message", msg)
	}
}

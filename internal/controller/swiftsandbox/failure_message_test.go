package swiftsandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Lab validation of v0.15.0, round 5: a warm slot whose launcher had failed
// was deleted with a SlotEnded event reading only "launcher pod failed". The
// launcher writes no termination message, so the kubelet's exit code and
// reason are what is left to say why.
func TestPodFailureMessage(t *testing.T) {
	terminated := func(name string, code int32, reason, msg string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason, Message: msg},
		}}
	}
	for _, tc := range []struct {
		name   string
		status corev1.PodStatus
		want   string
	}{
		{"launcher termination message", corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{terminated(launcherName, 1, "Error", "Cannot open initramfs file")},
		}, "Cannot open initramfs file"},
		{"pod message (eviction)", corev1.PodStatus{
			Reason: "Evicted", Message: "The node was low on resource: memory.",
			ContainerStatuses: []corev1.ContainerStatus{terminated(launcherName, 137, "Error", "")},
		}, "The node was low on resource: memory."},
		{"failed init container", corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				terminated("network-init", 0, "Completed", ""),
				terminated("kernel-init", 2, "Error", ""),
			},
		}, "init container kernel-init exited 2 (Error)"},
		{"launcher exit without a message", corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{terminated(launcherName, 1, "Error", "")},
		}, "launcher exited 1 (Error)"},
		{"launcher OOMKilled", corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{terminated(launcherName, 137, "OOMKilled", "")},
		}, "launcher exited 137 (OOMKilled)"},
		{"only a pod reason", corev1.PodStatus{Reason: "NodeShutdown"}, "launcher pod failed: NodeShutdown"},
		{"nothing recorded", corev1.PodStatus{}, "launcher pod failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.status.Phase = corev1.PodFailed
			if got := podFailureMessage(&corev1.Pod{Status: tc.status}); got != tc.want {
				t.Errorf("podFailureMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

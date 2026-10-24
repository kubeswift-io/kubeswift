package swiftsandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func consoleContainer(t *testing.T, pod *corev1.Pod) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == ConsoleContainerName {
			return &pod.Spec.Containers[i]
		}
	}
	t.Fatalf("pod %s has no console container", pod.Name)
	return nil
}

// The console container reads the console volume and nothing else, as an
// unprivileged native sidecar without the pod's service account token, so
// `get pods/log` on it grants no launcher or node access.
func TestConsoleContainer_IsUnprivilegedAndSeesOnlyTheConsole(t *testing.T) {
	pod := buildPod(plainSandbox("busybox:1"), "sandbox")
	c := consoleContainer(t, pod)

	// Started with the launcher (no init step on the cold path), after it,
	// and ending with it (the launcher's FIFO).
	if pod.Spec.Containers[0].Name != launcherName || c.RestartPolicy != nil {
		t.Errorf("console container must follow the launcher as an ordinary container: %v", pod.Spec.Containers)
	}
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == ConsoleContainerName {
			t.Error("console container is init-ordered: a kubelet step on every cold start")
		}
	}
	if want := []string{"follow-console", "--file", consoleSerialLog, "--until-closed", consoleAliveFIFO}; !equalStrings(c.Args, want) {
		t.Errorf("cold console follows %v, want %v", c.Args, want)
	}
	sc := c.SecurityContext
	if sc == nil || sc.Privileged == nil || *sc.Privileged || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
		sc.RunAsUser == nil || *sc.RunAsUser == 0 || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.Capabilities == nil ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 {
		t.Errorf("console container is not locked down: %+v", sc)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m
	}
	if len(mounts) != 2 || !mounts[consoleVolume].ReadOnly || mounts[consoleVolume].MountPath != consoleDir {
		t.Errorf("console container mounts %v, want only the console volume read-only", c.VolumeMounts)
	}
	if m, ok := mounts[consoleNoToken]; !ok || m.MountPath != serviceAccountPath {
		t.Error("the service account token is not masked")
	}
	if len(c.VolumeDevices) != 0 || len(c.EnvFrom) != 0 || len(c.Env) != 0 || len(c.Ports) != 0 {
		t.Errorf("console container has devices, env or ports: %+v", c)
	}

	launcher := pod.Spec.Containers[0]
	if c.Resources.Limits.Memory().IsZero() {
		t.Error("console container memory is not bounded")
	}
	var env, mount bool
	for _, e := range launcher.Env {
		env = env || (e.Name == "KUBESWIFT_CONSOLE_DIR" && e.Value == consoleDir)
	}
	for _, m := range launcher.VolumeMounts {
		mount = mount || (m.Name == consoleVolume && m.MountPath == consoleDir && !m.ReadOnly)
	}
	if !env || !mount {
		t.Errorf("launcher does not write the console volume: env=%t mount=%t", env, mount)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == consoleVolume && (v.EmptyDir == nil || v.HostPath != nil) {
			t.Errorf("console volume is not an emptyDir: %+v", v)
		}
	}
}

// A warm slot's console is its checkout's workload, never the slot's boot.
func TestConsoleContainer_WarmSlotFollowsTheWorkload(t *testing.T) {
	pod := buildPod(plainSandbox("busybox:1"), "sandbox")
	followWorkloadLog(pod)
	if c := consoleContainer(t, pod); !equalStrings(c.Args, consoleArgs(consoleWorkloadLog)) {
		t.Errorf("slot console follows %v", c.Args)
	}
}

func TestConsoleContainer_Off(t *testing.T) {
	ConsoleEnabled = false
	defer func() { ConsoleEnabled = true }()
	pod := buildPod(plainSandbox("busybox:1"), "sandbox")
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		if c.Name == ConsoleContainerName {
			t.Fatal("console container with --sandbox-console=false")
		}
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "KUBESWIFT_CONSOLE_DIR" {
			t.Fatal("launcher told to write a console volume it does not have")
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

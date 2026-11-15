package swiftsandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func launcherOf(t *testing.T, pod *corev1.Pod) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == launcherName {
			return c
		}
	}
	t.Fatal("no launcher container")
	return corev1.Container{}
}

// The launcher requests the guest's memory plus overhead and a CPU per vCPU,
// and sets no limits.
func TestBuildPod_LauncherRequests(t *testing.T) {
	sb := egSandbox("")
	sb.Spec.CPU = 2
	sb.Spec.Memory = resource.MustParse("1Gi")
	c := launcherOf(t, buildPod(sb, "sandbox"))
	if got, want := c.Resources.Requests[corev1.ResourceMemory], resource.MustParse("1088Mi"); got.Cmp(want) != 0 {
		t.Errorf("memory request = %s, want %s", got.String(), want.String())
	}
	if got := c.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("cpu request = %s, want 2", got.String())
	}
	if len(c.Resources.Limits) != 0 {
		t.Errorf("limits = %v, want none", c.Resources.Limits)
	}

	// Unset size: the launcher's floors (1 vCPU, 128 MiB).
	c = launcherOf(t, buildPod(egSandbox(""), "sandbox"))
	if got := c.Resources.Requests[corev1.ResourceMemory]; got.Cmp(resource.MustParse("192Mi")) != 0 {
		t.Errorf("floor memory request = %s, want 192Mi", got.String())
	}
	if got := c.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("floor cpu request = %s, want 1", got.String())
	}
}

func TestBuildPod_LauncherCPUPerVCPU(t *testing.T) {
	old := LauncherCPUPerVCPU
	t.Cleanup(func() { LauncherCPUPerVCPU = old })
	sb := egSandbox("")
	sb.Spec.CPU = 4

	LauncherCPUPerVCPU = resource.MustParse("250m")
	if got := launcherOf(t, buildPod(sb, "sandbox")).Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("250m x 4 = %s, want 1", got.String())
	}
	LauncherCPUPerVCPU = resource.MustParse("0")
	if _, ok := launcherOf(t, buildPod(sb, "sandbox")).Resources.Requests[corev1.ResourceCPU]; ok {
		t.Error("zero per vCPU still requests CPU")
	}
}

func TestParseLauncherCPUPerVCPU(t *testing.T) {
	for in, want := range map[string]string{"": "1", "1": "1", "250m": "250m", "0": "0", "1.5": "1500m"} {
		q, err := ParseLauncherCPUPerVCPU(in)
		if err != nil || q.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("%q -> %s, %v; want %s", in, q.String(), err, want)
		}
	}
	for _, bad := range []string{"-1", "lots", "1Gi?"} {
		if _, err := ParseLauncherCPUPerVCPU(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

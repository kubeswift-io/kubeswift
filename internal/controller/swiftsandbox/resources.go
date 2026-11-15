package swiftsandbox

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// LauncherMemoryOverheadMiB is what the launcher container uses beyond guest
// memory: the Cloud Hypervisor process itself, swiftletd, dnsmasq, virtiofsd
// and the page tables for guest RAM. Measured at 10 to 25 MiB for 256 MiB to
// 1 GiB guests, block and virtio-fs rootfs, with the guest's memory filled.
// Page cache the pod is charged for (the rootfs) is reclaimable and is not
// counted.
const LauncherMemoryOverheadMiB = 64

// LauncherCPUPerVCPU is the launcher's CPU request per guest vCPU (chart
// sandboxMaterialize.launcherCPUPerVCPU). One full CPU by default: a vCPU is a
// host thread that can use a whole core. Lower it to overcommit CPU on nodes
// that run mostly idle sandboxes; zero requests no CPU.
var LauncherCPUPerVCPU = resource.MustParse("1")

// ParseLauncherCPUPerVCPU parses the chart value. Empty keeps the default.
func ParseLauncherCPUPerVCPU(s string) (resource.Quantity, error) {
	if s == "" {
		return resource.MustParse("1"), nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return q, fmt.Errorf("launcher CPU per vCPU %q: %w", s, err)
	}
	if q.Sign() < 0 {
		return q, fmt.Errorf("launcher CPU per vCPU %q is negative", s)
	}
	return q, nil
}

// guestSize is the guest's vCPUs and memory in MiB, with the floors the
// launcher applies.
func guestSize(sb *sandboxv1alpha1.SwiftSandbox) (cpu, memMiB int) {
	cpu = int(sb.Spec.CPU)
	if cpu < 1 {
		cpu = 1
	}
	memMiB = int(sb.Spec.Memory.Value() >> 20)
	if memMiB < 128 {
		memMiB = 128
	}
	return cpu, memMiB
}

// launcherResources is the launcher container's requests: guest memory plus
// LauncherMemoryOverheadMiB, and LauncherCPUPerVCPU per vCPU. Without them the
// scheduler counts a sandbox as the console container's 8 MiB and places
// sandboxes on a node whose memory they will not fit in.
//
// No limits. Guest memory is bounded by the hypervisor already, and a CPU
// limit would throttle the vCPUs.
func launcherResources(sb *sandboxv1alpha1.SwiftSandbox) corev1.ResourceRequirements {
	cpu, memMiB := guestSize(sb)
	req := corev1.ResourceList{
		corev1.ResourceMemory: *resource.NewQuantity(int64(memMiB+LauncherMemoryOverheadMiB)<<20, resource.BinarySI),
	}
	if LauncherCPUPerVCPU.Sign() > 0 {
		req[corev1.ResourceCPU] = *resource.NewMilliQuantity(LauncherCPUPerVCPU.MilliValue()*int64(cpu), resource.DecimalSI)
	}
	return corev1.ResourceRequirements{Requests: req}
}

package swiftguest

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// HugepagesMountPath is where the launcher gets its hugetlbfs mount. Cloud
// Hypervisor's `hugepages=on` allocates guest RAM from the default hugetlbfs
// mount, which is this path.
const HugepagesMountPath = "/dev/hugepages"

// applyHugepages rewrites a launcher pod's memory accounting when the guest
// class asks for hugepage-backed RAM, and returns the volume/mount that give
// the container a hugetlbfs to allocate from.
//
// The load-bearing part is that guest RAM MOVES from `memory` to
// `hugepages-<size>` rather than being requested twice. The kubelet subtracts
// reserved hugepages from the node's allocatable `memory` (observed directly:
// reserving 8Gi of 2Mi pages dropped allocatable memory by exactly that much),
// so a pod that asked for both would consume 2x its guest RAM worth of the
// node and stop scheduling long before the hugepages ran out.
//
// The launcher overhead stays on ordinary `memory`: it is the VMM's own
// working set (page tables, device emulation, virtiofsd), which is not
// allocated from hugetlbfs.
//
// Requests and limits are set to the same value because Kubernetes requires it
// for hugepages — they are a non-overcommittable resource.
func applyHugepages(res *corev1.ResourceRequirements, rg *resolved.ResolvedGuest, guestMemMiB int) (*corev1.Volume, *corev1.VolumeMount) {
	name := rg.GetHugepagesResourceName()
	if name == "" {
		return nil, nil
	}

	qty := *resource.NewQuantity(int64(guestMemMiB)*1024*1024, resource.BinarySI)
	rn := corev1.ResourceName(name)
	res.Requests[rn] = qty
	res.Limits[rn] = qty

	// Guest RAM now comes from hugetlbfs, so leave only the launcher overhead
	// on ordinary memory. Without this the pod double-books the node.
	overhead := *resource.NewQuantity(int64(LauncherMemoryOverheadMiB)*1024*1024, resource.BinarySI)
	res.Requests[corev1.ResourceMemory] = overhead
	res.Limits[corev1.ResourceMemory] = overhead

	// medium is size-qualified ("HugePages-2Mi"), not bare "HugePages": a bare
	// medium is only unambiguous on a node offering a single hugepage size, and
	// these nodes expose both pools.
	medium := corev1.StorageMedium("HugePages-" + rg.Hugepages)
	vol := &corev1.Volume{
		Name:         "hugepages",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: medium}},
	}
	mount := &corev1.VolumeMount{Name: "hugepages", MountPath: HugepagesMountPath}
	return vol, mount
}

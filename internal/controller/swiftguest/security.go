package swiftguest

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// CloneGrowInitImage is the image used by clone-grow-init.
// ubuntu:22.04 matches CloneJobImage and ships qemu-utils + gdisk via apt.
const CloneGrowInitImage = "ubuntu:22.04"

// privilegedContext returns a privileged security context.
// Used by network-init, gpu-init, and launcher containers which need deep
// host access (KVM, VFIO, tap devices, iptables, sysctl).
func privilegedContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		Privileged: ptr.To(true),
	}
}

// networkInitContainer returns the network-init init container.
func networkInitContainer() corev1.Container {
	return corev1.Container{
		Name:            "network-init",
		Image:           LauncherImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "/usr/local/bin/network-init.sh"},
		SecurityContext: privilegedContext(),
	}
}

// cloneGrowInitContainer returns the clone-grow-init init container that
// expands a snapshot-based clone PVC's image.raw to the target size.
//
// Used only on the snapshot clone strategy. The Copy Job path performs the
// equivalent qemu-img resize + sgdisk -e itself before the launcher pod is
// created (see createCloneJob in rootdisk.go).
//
// The PVC's underlying block device must already be expanded to targetBytes
// before this init container runs; the SwiftGuest controller's
// expand-and-wait gate (ensureRootDiskCloneFromSnapshot) enforces that.
func cloneGrowInitContainer(targetBytes int64) corev1.Container {
	script := fmt.Sprintf(`set -e
echo "clone-grow-init: target=%d bytes"
apt-get update -qq
apt-get install -y -qq qemu-utils gdisk >/dev/null 2>&1
qemu-img resize -f raw %s/image.raw %d
sgdisk -e %s/image.raw
sync
echo "clone-grow-init: complete ($(stat -c %%s %s/image.raw) bytes)"`,
		targetBytes, DisksRootPath, targetBytes, DisksRootPath, DisksRootPath)

	return corev1.Container{
		Name:            "clone-grow-init",
		Image:           CloneGrowInitImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c", script},
		SecurityContext: privilegedContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "root-disk", MountPath: DisksRootPath},
		},
	}
}

// gpuInitSecurityContext returns the security context for the gpu-init
// init container.
func gpuInitSecurityContext() *corev1.SecurityContext {
	return privilegedContext()
}

// launcherSecurityContext returns the security context for the launcher
// (swiftletd) container.
func launcherSecurityContext(hasGPU bool) *corev1.SecurityContext {
	return privilegedContext()
}

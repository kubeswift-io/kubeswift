package swiftguest

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/kubeswift-io/kubeswift/internal/resolved"
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
//
// It mounts the runtime-intent ConfigMap and the shared run emptyDir at the
// SAME paths the launcher uses. Without the intent mount, network-init.sh's
// has_nics() check fails and the script silently falls back to the legacy
// single-NIC br0/tap0 path — meaning its multi-NIC / Multus secondary-bridging
// path was unreachable and secondary NADs were never bridged into the guest.
// The run mount lets network setup persist state the launcher reads (e.g. a
// NAD-assigned primary IP for the multi-node-L2 datapath).
//
// All pod builders that add this init container (disk-boot, kernel-boot, GPU,
// restore) define both the "runtime-intent" and "run" volumes, so referencing
// them here is safe.
func networkInitContainer() corev1.Container {
	return corev1.Container{
		Name:            "network-init",
		Image:           LauncherImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "/usr/local/bin/network-init.sh"},
		SecurityContext: privilegedContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "runtime-intent", MountPath: IntentPath},
			{Name: "run", MountPath: RunDirPath},
		},
	}
}

// cloneGrowInitContainer returns the clone-grow-init init container that
// finalises a snapshot-based clone PVC before the launcher pod boots.
//
// Used only on the snapshot clone strategy. The Copy Job path performs the
// equivalent operations itself before the launcher pod is created (see
// createCloneJob in rootdisk.go).
//
// The PVC's underlying block device (or filesystem) must already be at
// targetBytes before this init container runs; the SwiftGuest controller's
// expand-and-wait gate (ensureRootDiskCloneFromSnapshot) enforces that.
//
// W9 — branches on rg.Storage.VolumeMode (PR #32 surface):
//
//   - Filesystem (default): byte-identical to pre-W9.
//     `qemu-img resize -f raw .../image.raw <bytes> && sgdisk -e .../image.raw`.
//   - Block: VolumeDevices for the root-disk volume; runs
//     `sgdisk -e /dev/kubeswift-root` only. No qemu-img resize —
//     block devices are pre-sized at the PVC's requested capacity by
//     the storage layer; resize would be a no-op and including it
//     would mask future regressions in the expand-and-wait gate.
func cloneGrowInitContainer(rg *resolved.ResolvedGuest, targetBytes int64) corev1.Container {
	isBlock := rg != nil && rg.Storage.VolumeMode == "Block"

	var script string
	var volumeMounts []corev1.VolumeMount
	var volumeDevices []corev1.VolumeDevice

	if isBlock {
		// Block path. sgdisk -e operates byte-level through the block
		// device's standard read/write interface; works natively on
		// raw devices. No qemu-img resize (no-op on Block) and no cp
		// (the PVC was populated by the Copy Job which already wrote
		// the raw image to the block device).
		script = fmt.Sprintf(`set -e
echo "clone-grow-init: Block mode, target=%d bytes, device=%s"
apt-get update -qq
apt-get install -y -qq gdisk >/dev/null 2>&1
sgdisk -e %s
sync
echo "clone-grow-init: complete (Block)"`,
			targetBytes, DiskRootDevicePath, DiskRootDevicePath)
		volumeDevices = []corev1.VolumeDevice{
			{Name: "root-disk", DevicePath: DiskRootDevicePath},
		}
	} else {
		// Filesystem path — byte-identical to pre-W9 behaviour. Reviewers:
		// any change that alters this branch is a regression risk for
		// every existing SwiftGuest (the default volumeMode).
		script = fmt.Sprintf(`set -e
echo "clone-grow-init: target=%d bytes"
apt-get update -qq
apt-get install -y -qq qemu-utils gdisk >/dev/null 2>&1
qemu-img resize -f raw %s/image.raw %d
sgdisk -e %s/image.raw
sync
echo "clone-grow-init: complete ($(stat -c %%s %s/image.raw) bytes)"`,
			targetBytes, DisksRootPath, targetBytes, DisksRootPath, DisksRootPath)
		volumeMounts = []corev1.VolumeMount{
			{Name: "root-disk", MountPath: DisksRootPath},
		}
	}

	return corev1.Container{
		Name:            "clone-grow-init",
		Image:           CloneGrowInitImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c", script},
		SecurityContext: privilegedContext(),
		VolumeMounts:    volumeMounts,
		VolumeDevices:   volumeDevices,
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

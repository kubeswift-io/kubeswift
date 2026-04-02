package swiftguest

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// networkInitContainer returns the hardened network-init init container.
// Capabilities: NET_ADMIN (ip link, ip addr, brctl, iptables, sysctl) +
// NET_RAW (raw sockets for dnsmasq, iptables conntrack).
func networkInitContainer() corev1.Container {
	return corev1.Container{
		Name:            "network-init",
		Image:           LauncherImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/usr/local/bin/network-init.sh"},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               ptr.To(false),
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW"},
			},
		},
	}
}

// gpuInitSecurityContext returns the hardened security context for the gpu-init
// init container. SYS_ADMIN is required to write to sysfs paths for VFIO driver
// binding (/sys/bus/pci/devices/*/driver_override, /sys/bus/pci/drivers_probe)
// and to execute fmpm for Fabric Manager partition activation.
func gpuInitSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		Privileged:               ptr.To(false),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"SYS_ADMIN"},
		},
	}
}

// launcherSecurityContext returns the hardened security context for the launcher
// (swiftletd) container.
//
// Non-GPU path capabilities:
//   - NET_ADMIN: tap device manipulation for virtio-net, dnsmasq operation
//   - SYS_ADMIN: KVM ioctls (/dev/kvm), namespace operations
//
// GPU path adds:
//   - SYS_RESOURCE: hugepage allocation (mlock limits for hugepage-backed memory)
//   - DAC_OVERRIDE: access VFIO device files in /dev/vfio/ owned by root
func launcherSecurityContext(hasGPU bool) *corev1.SecurityContext {
	caps := []corev1.Capability{"NET_ADMIN", "SYS_ADMIN"}
	if hasGPU {
		caps = append(caps, "SYS_RESOURCE", "DAC_OVERRIDE")
	}
	return &corev1.SecurityContext{
		Privileged:               ptr.To(false),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  caps,
		},
	}
}

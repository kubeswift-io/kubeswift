package swiftguest

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

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

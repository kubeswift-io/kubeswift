package swiftsandbox

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

const (
	intentConfigMapSuffix = "-runtime-intent"
	kernelNodeLabel       = "kubeswift.io/kernel-node"
	defaultKernelProfile  = "sandbox"
	materializeInitName   = "sandbox-materialize"
	launcherName          = "launcher"
	// SandboxLabelKey ties the launcher pod (and its intent ConfigMap) to the
	// owning SwiftSandbox.
	SandboxLabelKey = "sandbox.kubeswift.io/sandbox"
)

func intentConfigMapName(sb *sandboxv1alpha1.SwiftSandbox) string {
	return sb.Name + intentConfigMapSuffix
}

func privileged() *corev1.SecurityContext {
	return &corev1.SecurityContext{Privileged: ptr.To(true)}
}

// networked reports whether the sandbox gets a network (every mode except "none").
func networked(sb *sandboxv1alpha1.SwiftSandbox) bool {
	return sb.Spec.Network.Mode != sandboxv1alpha1.SandboxNetworkNone
}

// buildIntent constructs the mode-3 sandbox RuntimeIntent: kernel boot + the RO
// OCI rootfs disk + the bridge cmdline (rootfs selector + entrypoint).
func buildIntent(sb *sandboxv1alpha1.SwiftSandbox, kernelName, rootfsPath, entrypoint string) *runtimeintent.RuntimeIntent {
	kernelDir := kernelv1alpha1.KernelLocalPath(sb.Namespace, kernelName)
	cmdline := "console=ttyS0 kubeswift.rootfs=block"
	if networked(sb) {
		// Kernel IP autoconfig (the sandbox kernel has CONFIG_IP_PNP_DHCP=y). A bare
		// OCI workload (e.g. /bin/sh) runs no DHCP client and the bridge-init does no
		// network setup, so the guest would otherwise never acquire the dnsmasq lease
		// / an IP (and status.network.primaryIP would stay empty). ip=dhcp makes the
		// kernel DHCP eth0 at boot. Omitted for network:none — there is no dnsmasq,
		// so kernel DHCP would only stall the boot.
		cmdline += " ip=dhcp"
	}
	if entrypoint != "" {
		cmdline += " kubeswift.entrypoint=" + entrypoint
	}
	cpu := int(sb.Spec.CPU)
	if cpu < 1 {
		cpu = 1
	}
	memMiB := int(sb.Spec.Memory.Value() >> 20)
	if memMiB < 128 {
		memMiB = 128
	}
	return &runtimeintent.RuntimeIntent{
		KernelBoot: &runtimeintent.KernelBootSpec{
			KernelPath:    kernelDir + "/bzImage",
			InitramfsPath: kernelDir + "/rootfs.cpio.gz",
			Cmdline:       cmdline,
		},
		SandboxRootfs: &runtimeintent.SandboxRootfsSpec{Path: rootfsPath},
		CPU:           cpu,
		Memory:        memMiB,
		Lifecycle:     "start",
		GuestID:       sb.Namespace + "/" + sb.Name,
		// Networked unless mode=none: network-init sets up br0/tap0 and the
		// launcher-entrypoint starts dnsmasq; a deny-ingress NetworkPolicy enforces
		// the "restricted" posture (built by the controller).
		Network:    networked(sb),
		Hypervisor: "cloud-hypervisor",
	}
}

// buildIntentConfigMap wraps the serialized intent in the ConfigMap the launcher
// mounts (swiftletd reads swiftguest.IntentPath/IntentFile).
func buildIntentConfigMap(sb *sandboxv1alpha1.SwiftSandbox, intentJSON []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: intentConfigMapName(sb), Namespace: sb.Namespace},
		Data:       map[string]string{swiftguest.IntentFile: string(intentJSON)},
	}
}

// buildPod builds the sandbox launcher pod: a sandbox-materialize init container
// (pulls the image + produces the RO ext4 in the node cache) followed by the
// swiftletd launcher (mode-3 direct-kernel boot of that rootfs). RestartPolicy
// Never; pinned to a kernel node.
func buildPod(sb *sandboxv1alpha1.SwiftSandbox, kernelName string) *corev1.Pod {
	kernelDir := kernelv1alpha1.KernelLocalPath(sb.Namespace, kernelName)

	nodeSelector := map[string]string{kernelNodeLabel: "true"}
	for k, v := range sb.Spec.NodeSelector {
		nodeSelector[k] = v
	}

	matArgs := []string{
		"--image", sb.Spec.Image,
		"--cache-dir", rootfsCacheDir,
		"--mode", "block",
		"--result-file", "/dev/termination-log",
	}
	// (--pull-secret mount when spec.imagePullSecret is set — follow-up.)

	initContainers := []corev1.Container{{
		Name:            materializeInitName,
		Image:           SandboxMaterializeImage(),
		Args:            matArgs,
		SecurityContext: privileged(),
		VolumeMounts:    []corev1.VolumeMount{{Name: "rootfs-cache", MountPath: rootfsCacheDir}},
	}}
	if networked(sb) {
		// network-init (br0/tap0/dnsmasq) runs first; it mounts the same
		// runtime-intent + run volumes the launcher uses.
		initContainers = append([]corev1.Container{swiftguest.NetworkInitContainer()}, initContainers...)
	}

	dirCreate := corev1.HostPathDirectoryOrCreate
	charDev := corev1.HostPathCharDev

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.Name,
			Namespace: sb.Namespace,
			Labels:    map[string]string{SandboxLabelKey: sb.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			NodeSelector:   nodeSelector,
			InitContainers: initContainers,
			Containers: []corev1.Container{{
				Name:            launcherName,
				Image:           swiftguest.LauncherImage(),
				SecurityContext: privileged(),
				// swiftletd reads POD_NAME/POD_NAMESPACE (downward API) to know which
				// pod to report onto; without them it skips the report + lease paths
				// entirely (guest pid/hypervisor/IP never surface). KUBESWIFT_REPORT_GUEST_CR
				// tells swiftletd NOT to patch a SwiftGuest CR status (there is none for a
				// sandbox — the SwiftSandbox controller owns status from the pod annotations).
				Env: []corev1.EnvVar{
					{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
					{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
					{Name: "KUBESWIFT_REPORT_GUEST_CR", Value: "false"},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "kernel-artifacts", MountPath: kernelDir},
					{Name: "rootfs-cache", MountPath: rootfsCacheDir},
					{Name: "runtime-intent", MountPath: swiftguest.IntentPath},
					{Name: "run", MountPath: swiftguest.RunDirPath},
					{Name: "dev-kvm", MountPath: "/dev/kvm"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "kernel-artifacts", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: kernelDir, Type: &dirCreate}}},
				{Name: "rootfs-cache", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: rootfsCacheDir, Type: &dirCreate}}},
				{Name: "runtime-intent", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: intentConfigMapName(sb)}}}},
				{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "dev-kvm", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/kvm", Type: &charDev}}},
			},
		},
	}
}

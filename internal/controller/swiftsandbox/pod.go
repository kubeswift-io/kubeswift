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

// buildIntent constructs the mode-3 sandbox RuntimeIntent: kernel boot + the RO
// OCI rootfs disk + the bridge cmdline (rootfs selector + entrypoint).
func buildIntent(sb *sandboxv1alpha1.SwiftSandbox, kernelName, rootfsPath, entrypoint string) *runtimeintent.RuntimeIntent {
	kernelDir := kernelv1alpha1.KernelLocalPath(sb.Namespace, kernelName)
	cmdline := "console=ttyS0 kubeswift.rootfs=block"
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
		// P4c: restricted-mode networking (network-init + NetworkPolicy) is not yet
		// wired; v1 sandboxes boot network-isolated regardless of spec.network.mode.
		Network:    false,
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
	// (P4c: --pull-secret mount when spec.imagePullSecret is set.)

	dirCreate := corev1.HostPathDirectoryOrCreate
	charDev := corev1.HostPathCharDev

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.Name,
			Namespace: sb.Namespace,
			Labels:    map[string]string{SandboxLabelKey: sb.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector:  nodeSelector,
			InitContainers: []corev1.Container{{
				Name:            materializeInitName,
				Image:           SandboxMaterializeImage(),
				Args:            matArgs,
				SecurityContext: privileged(),
				VolumeMounts: []corev1.VolumeMount{
					{Name: "rootfs-cache", MountPath: rootfsCacheDir},
				},
			}},
			Containers: []corev1.Container{{
				Name:            launcherName,
				Image:           swiftguest.LauncherImage(),
				SecurityContext: privileged(),
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

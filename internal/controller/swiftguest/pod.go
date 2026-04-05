package swiftguest

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

// IntentConfigMapSuffix is the suffix for the runtime intent ConfigMap name.
const IntentConfigMapSuffix = "-runtime-intent"

// LauncherMemoryOverheadMiB is the extra memory added to the container limit
// beyond the guest memory. Accounts for the hypervisor process (CH or QEMU),
// swiftletd, dnsmasq, and kernel cgroup overhead. Without this, the container
// is OOMKilled because the guest RAM is allocated inside the container's cgroup.
const LauncherMemoryOverheadMiB = 512

// BuildPod creates a pod spec for the SwiftGuest.
func BuildPod(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest, seedConfigMapName, intentConfigMapName string) *corev1.Pod {
	if rg.HasKernel() {
		return buildKernelBootPod(guest, rg, intentConfigMapName)
	}
	return buildDiskBootPod(guest, rg, seedConfigMapName, intentConfigMapName)
}

// podAnnotations returns the base annotations for a launcher pod,
// including the Multus networks annotation if secondary NICs are configured.
func podAnnotations(guest *swiftv1alpha1.SwiftGuest) map[string]string {
	multus := BuildMultusAnnotation(guest)
	if multus == "" {
		return nil
	}
	return map[string]string{
		MultusAnnotationKey: multus,
	}
}

func buildKernelBootPod(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest, intentConfigMapName string) *corev1.Pod {
	volumes := []corev1.Volume{
		{
			Name: "run",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "runtime-intent",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: intentConfigMapName},
				},
			},
		},
		{
			Name: "dev-kvm",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/dev/kvm",
					Type: ptr.To(corev1.HostPathType("CharDevice")),
				},
			},
		},
		{
			Name: "kernel-artifacts",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: rg.KernelBoot.LocalPath,
					Type: ptr.To(corev1.HostPathDirectory),
				},
			},
		},
	}

	if rg.HasDataDisk() {
		volumes = append(volumes, corev1.Volume{
			Name: "data-disk",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: rg.GetDataDiskPVCName(),
				},
			},
		})
	}

	mounts := []corev1.VolumeMount{
		{Name: "run", MountPath: RunDirPath},
		{Name: "runtime-intent", MountPath: IntentPath},
		{Name: "dev-kvm", MountPath: "/dev/kvm"},
		{Name: "kernel-artifacts", MountPath: rg.KernelBoot.LocalPath},
	}
	if rg.HasDataDisk() {
		mounts = append(mounts, corev1.VolumeMount{Name: "data-disk", MountPath: DisksDataPath})
	}

	cpu := rg.Resources.CPU
	if cpu < 1 {
		cpu = 1
	}
	mem := rg.Resources.Memory
	if mem < 128 {
		mem = 128
	}

	// SR-IOV: add /dev/vfio volume+mount if any interface is type=sriov.
	addSRIOVVolumesIfNeeded(&volumes, &mounts, guest)

	var initContainers []corev1.Container
	if rg.HasNetwork() {
		initContainers = append(initContainers, networkInitContainer())
	}

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpu), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(mem+LauncherMemoryOverheadMiB)*1024*1024, resource.BinarySI),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpu), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(mem+LauncherMemoryOverheadMiB)*1024*1024, resource.BinarySI),
		},
	}
	AddSRIOVResourceLimits(&resources, guest)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        guest.Name,
			Namespace:   guest.Namespace,
			Annotations: podAnnotations(guest),
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guest.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: initContainers,
			NodeSelector: map[string]string{
				"kubeswift.io/kernel-node": "true",
			},
			Containers: []corev1.Container{
				{
					Name:            "launcher",
					Image:           LauncherImage(),
					ImagePullPolicy: corev1.PullIfNotPresent,
					SecurityContext: launcherSecurityContext(false),
					Env: []corev1.EnvVar{
						{
							Name: "POD_NAME",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
							},
						},
						{
							Name: "POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
							},
						},
					},
					Resources:    resources,
					VolumeMounts: mounts,
				},
			},
			Volumes: volumes,
		},
	}
	return pod
}

func buildDiskBootPod(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest, seedConfigMapName, intentConfigMapName string) *corev1.Pod {
	volumes := []corev1.Volume{
		{
			Name: "run",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "root-disk",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: rg.PreparedImage.PVCName,
				},
			},
		},
		{
			Name: "runtime-intent",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: intentConfigMapName},
				},
			},
		},
		{
			Name: "dev-kvm",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/dev/kvm",
					Type: ptr.To(corev1.HostPathType("CharDevice")),
				},
			},
		},
	}
	if rg.HasSeed() {
		AddSeedVolume(&volumes, seedConfigMapName)
	}
	if rg.HasDataDisk() {
		volumes = append(volumes, corev1.Volume{
			Name: "data-disk",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: rg.GetDataDiskPVCName(),
				},
			},
		})
	}

	var mounts []corev1.VolumeMount
	AddVolumeMounts(&mounts, rg.HasSeed())
	if rg.HasDataDisk() {
		mounts = append(mounts, corev1.VolumeMount{Name: "data-disk", MountPath: DisksDataPath})
	}

	// SR-IOV: add /dev/vfio volume+mount if any interface is type=sriov.
	addSRIOVVolumesIfNeeded(&volumes, &mounts, guest)

	cpu := rg.Resources.CPU
	if cpu < 1 {
		cpu = 1
	}
	mem := rg.Resources.Memory
	if mem < 128 {
		mem = 128
	}

	var initContainers []corev1.Container
	if rg.HasNetwork() {
		initContainers = append(initContainers, networkInitContainer())
	}

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpu), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(mem+LauncherMemoryOverheadMiB)*1024*1024, resource.BinarySI),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpu), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(mem+LauncherMemoryOverheadMiB)*1024*1024, resource.BinarySI),
		},
	}
	AddSRIOVResourceLimits(&resources, guest)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        guest.Name,
			Namespace:   guest.Namespace,
			Annotations: podAnnotations(guest),
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guest.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: initContainers,
			Containers: []corev1.Container{
				{
					Name:            "launcher",
					Image:           LauncherImage(),
					ImagePullPolicy: corev1.PullIfNotPresent,
					SecurityContext: launcherSecurityContext(false),
					Env: []corev1.EnvVar{
						{
							Name: "POD_NAME",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
							},
						},
						{
							Name: "POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
							},
						},
					},
					Resources:    resources,
					VolumeMounts: mounts,
				},
			},
			Volumes: volumes,
		},
	}
	return pod
}

// VolumeMountPaths returns the canonical mount paths for the guest pod.
// Used when building pod spec to ensure alignment with swiftletd.
// intentPath is the directory; runtime-intent.json is the file inside it.
func VolumeMountPaths() (imagePath, seedPath, intentDir string) {
	return DisksRootPath, SeedPath, IntentPath
}

// AddVolumeMounts adds the standard volume mounts to a container.
// Caller must ensure volumes exist for image, seed (if present), intent, and dev-kvm.
func AddVolumeMounts(mounts *[]corev1.VolumeMount, hasSeed bool) {
	imagePath, seedPath, intentDir := VolumeMountPaths()
	*mounts = append(*mounts,
		corev1.VolumeMount{Name: "run", MountPath: RunDirPath},
		corev1.VolumeMount{Name: "root-disk", MountPath: imagePath},
		corev1.VolumeMount{Name: "runtime-intent", MountPath: intentDir},
		corev1.VolumeMount{Name: "dev-kvm", MountPath: "/dev/kvm"},
	)
	if hasSeed {
		*mounts = append(*mounts, corev1.VolumeMount{Name: "seed", MountPath: seedPath})
	}
}

// AddSeedVolume adds the seed ConfigMap volume to the pod. Use when ResolvedGuest has Seed.
// ConfigMap name should be guestName + SeedConfigMapSuffix.
func AddSeedVolume(volumes *[]corev1.Volume, configMapName string) {
	*volumes = append(*volumes, corev1.Volume{
		Name: "seed",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			},
		},
	})
}

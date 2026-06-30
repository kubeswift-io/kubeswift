package swiftguest

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// HasSRIOVInterfaces returns true if any interface has type=sriov.
func HasSRIOVInterfaces(guest *swiftv1alpha1.SwiftGuest) bool {
	for _, iface := range guest.Spec.Interfaces {
		if iface.Type == swiftv1alpha1.InterfaceTypeSRIOV {
			return true
		}
	}
	return false
}

// AddSRIOVResourceLimits adds SR-IOV VF resource requests/limits to the launcher container.
// Each SR-IOV interface with a resourceName adds "1" to the corresponding extended resource.
func AddSRIOVResourceLimits(resources *corev1.ResourceRequirements, guest *swiftv1alpha1.SwiftGuest) {
	for _, iface := range guest.Spec.Interfaces {
		if iface.Type != swiftv1alpha1.InterfaceTypeSRIOV || iface.ResourceName == "" {
			continue
		}
		rn := corev1.ResourceName(iface.ResourceName)
		if resources.Requests == nil {
			resources.Requests = corev1.ResourceList{}
		}
		if resources.Limits == nil {
			resources.Limits = corev1.ResourceList{}
		}
		// Each SR-IOV interface requests exactly 1 VF.
		qty := resource.MustParse("1")
		resources.Requests[rn] = qty
		resources.Limits[rn] = qty
	}
}

// sriovVFIOVolume returns the /dev/vfio hostPath volume needed for SR-IOV VF passthrough.
func sriovVFIOVolume() corev1.Volume {
	return corev1.Volume{
		Name: "dev-vfio",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/dev/vfio",
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		},
	}
}

// sriovVFIOMount returns the /dev/vfio volume mount for the launcher container.
func sriovVFIOMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "dev-vfio", MountPath: "/dev/vfio"}
}

// addSRIOVVolumesIfNeeded adds /dev/vfio volume and mount to the pod spec
// when SR-IOV interfaces are present and the volume doesn't already exist
// (GPU pods already have it).
func addSRIOVVolumesIfNeeded(volumes *[]corev1.Volume, mounts *[]corev1.VolumeMount, guest *swiftv1alpha1.SwiftGuest) {
	if !HasSRIOVInterfaces(guest) {
		return
	}
	// Check if dev-vfio is already present (GPU pods add it).
	for _, v := range *volumes {
		if v.Name == "dev-vfio" {
			return
		}
	}
	*volumes = append(*volumes, sriovVFIOVolume())
	*mounts = append(*mounts, sriovVFIOMount())
}

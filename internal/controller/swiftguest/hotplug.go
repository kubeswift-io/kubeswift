package swiftguest

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/hotplug"
)

// applyHotplugVolumeDevices mounts kubelet's CSI volumeDevices directory
// into the launcher, read-only, for hot-plugged volumes: the launcher reads a
// volume's device from what kubelet published there for its attachment pod
// (internal/hotplug). HostToContainer propagation: kubelet bind-mounts each
// published device after the launcher started, and without it the launcher
// would see the empty mount point instead.
//
// A launcher pod's volumes are fixed at creation, so a guest started before
// the feature was turned on has no such mount, and its launcher does not
// advertise the feature.
func applyHotplugVolumeDevices(pod *corev1.Pod, kubeletRootDir string) {
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: hotplug.LauncherVolumeDevicesVolume,
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: hotplug.HostVolumeDevicesDir(kubeletRootDir),
			Type: ptr.To(corev1.HostPathDirectoryOrCreate),
		}},
	})
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != "launcher" {
			continue
		}
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:             hotplug.LauncherVolumeDevicesVolume,
			MountPath:        hotplug.LauncherVolumeDevicesPath,
			ReadOnly:         true,
			MountPropagation: ptr.To(corev1.MountPropagationHostToContainer),
		})
	}
}

// hotplugVolumesFromPod is the launcher's report of its hot-plugged volumes,
// for status.hotplugVolumes. A report that does not parse reads as none.
func hotplugVolumesFromPod(pod *corev1.Pod) []swiftv1alpha1.HotplugVolumeStatus {
	if pod == nil {
		return nil
	}
	sts, err := hotplug.DecodeStatus(pod.Annotations[hotplug.AnnotationStatus])
	if err != nil || len(sts) == 0 {
		return nil
	}
	out := make([]swiftv1alpha1.HotplugVolumeStatus, 0, len(sts))
	for _, st := range sts {
		out = append(out, swiftv1alpha1.HotplugVolumeStatus{Name: st.Name, Serial: st.Serial, State: st.State, Reason: st.Reason})
	}
	slices.SortFunc(out, func(a, b swiftv1alpha1.HotplugVolumeStatus) int { return strings.Compare(a.Name, b.Name) })
	return out
}

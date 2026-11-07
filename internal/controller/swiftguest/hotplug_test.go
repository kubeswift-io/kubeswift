package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/kubeswift-io/kubeswift/internal/hotplug"
)

func TestApplyHotplugVolumeDevices(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "launcher"}, {Name: "console"}}}}
	applyHotplugVolumeDevices(pod, "/var/lib/k0s/kubelet")
	v := pod.Spec.Volumes[len(pod.Spec.Volumes)-1]
	if v.HostPath == nil || v.HostPath.Path != "/var/lib/k0s/kubelet/plugins/kubernetes.io/csi/volumeDevices" {
		t.Fatalf("volume %+v", v)
	}
	m := pod.Spec.Containers[0].VolumeMounts
	if len(m) != 1 || !m[0].ReadOnly || m[0].MountPath != hotplug.LauncherVolumeDevicesPath ||
		m[0].MountPropagation == nil || *m[0].MountPropagation != corev1.MountPropagationHostToContainer {
		t.Fatalf("mount %+v", m)
	}
	if len(pod.Spec.Containers[1].VolumeMounts) != 0 {
		t.Fatal("only the launcher container mounts it")
	}
}

func TestHotplugVolumesFromPod(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Annotations = map[string]string{hotplug.AnnotationStatus: `[{"name":"b","state":"Attached"},{"name":"a","state":"InUse","reason":"Mounted"}]`}
	got := hotplugVolumesFromPod(pod)
	if len(got) != 2 || got[0].Name != "a" || got[0].Reason != "Mounted" || got[1].State != "Attached" {
		t.Fatalf("%+v", got)
	}
	pod.Annotations[hotplug.AnnotationStatus] = "not json"
	if hotplugVolumesFromPod(pod) != nil {
		t.Fatal("garbage reads as none")
	}
}

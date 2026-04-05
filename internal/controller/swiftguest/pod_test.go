package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

func TestBuildPod_HasInitContainerWhenHasSeed(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "minimal"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "test-seed", "test-intent")

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("initContainers = %d, want 1", len(pod.Spec.InitContainers))
	}
	ic := pod.Spec.InitContainers[0]
	if ic.Name != "network-init" {
		t.Errorf("init container name = %q, want network-init", ic.Name)
	}
	if len(ic.Command) != 2 || ic.Command[0] != "/bin/sh" || ic.Command[1] != "/usr/local/bin/network-init.sh" {
		t.Errorf("init container command = %v, want [/bin/sh /usr/local/bin/network-init.sh]", ic.Command)
	}
}

func TestBuildPod_DataDiskVolume_DiskBoot(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-dd", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc-root"},
		Seed:          nil,
		Network:       true,
		DataDisk:      &resolved.PreparedImage{PVCName: "pvc-data", Ready: true, Format: "raw"},
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	// Check volume exists.
	foundVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "data-disk" {
			foundVol = true
			if v.VolumeSource.PersistentVolumeClaim == nil {
				t.Fatal("data-disk volume should be a PVC")
			}
			if v.VolumeSource.PersistentVolumeClaim.ClaimName != "pvc-data" {
				t.Errorf("data-disk PVC = %q, want pvc-data", v.VolumeSource.PersistentVolumeClaim.ClaimName)
			}
		}
	}
	if !foundVol {
		t.Error("missing data-disk volume")
	}

	// Check mount exists on launcher.
	launcher := pod.Spec.Containers[0]
	foundMount := false
	for _, m := range launcher.VolumeMounts {
		if m.Name == "data-disk" {
			foundMount = true
			if m.MountPath != DisksDataPath {
				t.Errorf("data-disk mountPath = %q, want %q", m.MountPath, DisksDataPath)
			}
		}
	}
	if !foundMount {
		t.Error("launcher missing data-disk mount")
	}
}

func TestBuildPod_NoDataDisk_BackwardCompat(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodd", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          nil,
		Network:       false,
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	for _, v := range pod.Spec.Volumes {
		if v.Name == "data-disk" {
			t.Error("data-disk volume should not be present when DataDisk is nil")
		}
	}
	launcher := pod.Spec.Containers[0]
	for _, m := range launcher.VolumeMounts {
		if m.Name == "data-disk" {
			t.Error("data-disk mount should not be present when DataDisk is nil")
		}
	}
}

func TestBuildPod_NoInterfaces_NoMultusAnnotation(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-no-multus", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Interfaces:    nil,
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	if pod.ObjectMeta.Annotations != nil {
		if _, ok := pod.ObjectMeta.Annotations[MultusAnnotationKey]; ok {
			t.Error("pod should not have Multus annotation when no interfaces are set")
		}
	}
}

func TestBuildPod_WithSecondaryNIC_MultusAnnotation(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-multus", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt", NetworkRef: nil},
				{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{
					Name: "sriov-net",
				}},
			},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "mgmt", NetworkRef: nil},
			{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{
				Name: "sriov-net",
			}},
		},
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	if pod.ObjectMeta.Annotations == nil {
		t.Fatal("pod annotations are nil, want Multus annotation")
	}
	multus, ok := pod.ObjectMeta.Annotations[MultusAnnotationKey]
	if !ok {
		t.Fatal("pod missing Multus annotation")
	}
	if multus == "" {
		t.Error("Multus annotation is empty")
	}
	// Verify it contains the expected NAD name
	if !containsString(multus, "sriov-net") {
		t.Errorf("Multus annotation %q does not contain sriov-net", multus)
	}
}

func TestBuildPod_BackwardCompat_NoInterfacesField(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-compat", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			// Interfaces not set at all
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	// Should behave identically to a pod without multi-NIC: no Multus annotation
	if pod.ObjectMeta.Annotations != nil {
		if _, ok := pod.ObjectMeta.Annotations[MultusAnnotationKey]; ok {
			t.Error("backward-compat pod should not have Multus annotation")
		}
	}

	// Pod should still build successfully with a launcher container
	if len(pod.Spec.Containers) == 0 {
		t.Fatal("pod has no containers")
	}
	if pod.Spec.Containers[0].Name != "launcher" {
		t.Errorf("container name = %q, want launcher", pod.Spec.Containers[0].Name)
	}
}

// containsString is a simple substring check helper.
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && stringContains(s, substr))
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestBuildPod_SRIOVResourceLimits(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sriov", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net"}},
			},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "mgmt"},
			{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net"}},
		},
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	launcher := pod.Spec.Containers[0]
	rn := corev1.ResourceName("intel.com/sriov_netdevice")
	lim, ok := launcher.Resources.Limits[rn]
	if !ok {
		t.Fatal("launcher missing intel.com/sriov_netdevice in Limits")
	}
	if lim.Value() != 1 {
		t.Errorf("Limits[intel.com/sriov_netdevice] = %d, want 1", lim.Value())
	}
	req, ok := launcher.Resources.Requests[rn]
	if !ok {
		t.Fatal("launcher missing intel.com/sriov_netdevice in Requests")
	}
	if req.Value() != 1 {
		t.Errorf("Requests[intel.com/sriov_netdevice] = %d, want 1", req.Value())
	}
}

func TestBuildPod_SRIOVVFIOVolume(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sriov-vfio", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net"}},
			},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "mgmt"},
			{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net"}},
		},
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	// Check dev-vfio volume exists.
	foundVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "dev-vfio" {
			foundVol = true
			if v.HostPath == nil || v.HostPath.Path != "/dev/vfio" {
				t.Errorf("dev-vfio volume path = %v, want /dev/vfio", v.HostPath)
			}
		}
	}
	if !foundVol {
		t.Error("pod missing dev-vfio volume for SR-IOV")
	}

	// Check dev-vfio mount on launcher.
	launcher := pod.Spec.Containers[0]
	foundMount := false
	for _, m := range launcher.VolumeMounts {
		if m.Name == "dev-vfio" {
			foundMount = true
			if m.MountPath != "/dev/vfio" {
				t.Errorf("dev-vfio mount path = %q, want /dev/vfio", m.MountPath)
			}
		}
	}
	if !foundMount {
		t.Error("launcher missing dev-vfio mount for SR-IOV")
	}
}

func TestBuildPod_BridgeOnly_NoSRIOVResources(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-bridge-only", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt", Type: swiftv1alpha1.InterfaceTypeBridge},
				{Name: "data", Type: swiftv1alpha1.InterfaceTypeBridge, NetworkRef: &swiftv1alpha1.NetworkReference{Name: "net-a"}},
			},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "mgmt", Type: swiftv1alpha1.InterfaceTypeBridge},
			{Name: "data", Type: swiftv1alpha1.InterfaceTypeBridge, NetworkRef: &swiftv1alpha1.NetworkReference{Name: "net-a"}},
		},
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	// No SR-IOV extended resources.
	launcher := pod.Spec.Containers[0]
	for rn := range launcher.Resources.Limits {
		if rn != corev1.ResourceCPU && rn != corev1.ResourceMemory {
			t.Errorf("unexpected resource in Limits: %s", rn)
		}
	}

	// No dev-vfio volume.
	for _, v := range pod.Spec.Volumes {
		if v.Name == "dev-vfio" {
			t.Error("bridge-only pod should not have dev-vfio volume")
		}
	}
}

func TestBuildPod_SRIOVMultusAnnotation(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sriov-multus", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-nad"}},
			},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "mgmt"},
			{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-nad"}},
		},
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	if pod.ObjectMeta.Annotations == nil {
		t.Fatal("pod annotations are nil, want Multus annotation for SR-IOV NAD")
	}
	multus, ok := pod.ObjectMeta.Annotations[MultusAnnotationKey]
	if !ok {
		t.Fatal("pod missing Multus annotation")
	}
	if !containsString(multus, "sriov-nad") {
		t.Errorf("Multus annotation %q does not contain sriov-nad", multus)
	}
}

func TestBuildPod_NoInitContainerWhenNoSeed(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          nil,
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("initContainers = %d, want 0 when no seed", len(pod.Spec.InitContainers))
	}
}

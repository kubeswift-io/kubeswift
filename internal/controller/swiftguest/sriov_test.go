package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func TestHasSRIOVInterfaces_NoInterfaces(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{}
	if HasSRIOVInterfaces(guest) {
		t.Error("HasSRIOVInterfaces(nil interfaces) = true, want false")
	}
}

func TestHasSRIOVInterfaces_BridgeOnly(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt", Type: swiftv1alpha1.InterfaceTypeBridge},
				{Name: "data", Type: swiftv1alpha1.InterfaceTypeBridge, NetworkRef: &swiftv1alpha1.NetworkReference{Name: "net-a"}},
			},
		},
	}
	if HasSRIOVInterfaces(guest) {
		t.Error("HasSRIOVInterfaces(all bridge) = true, want false")
	}
}

func TestHasSRIOVInterfaces_WithSRIOV(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net"}},
			},
		},
	}
	if !HasSRIOVInterfaces(guest) {
		t.Error("HasSRIOVInterfaces(one sriov) = false, want true")
	}
}

func TestHasSRIOVInterfaces_Mixed(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt", Type: swiftv1alpha1.InterfaceTypeBridge},
				{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net"}},
			},
		},
	}
	if !HasSRIOVInterfaces(guest) {
		t.Error("HasSRIOVInterfaces(bridge + sriov) = false, want true")
	}
}

func TestAddSRIOVResourceLimits_NoSRIOV(t *testing.T) {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"),
		},
	}
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt", Type: swiftv1alpha1.InterfaceTypeBridge},
				{Name: "data", Type: swiftv1alpha1.InterfaceTypeBridge, NetworkRef: &swiftv1alpha1.NetworkReference{Name: "net-a"}},
			},
		},
	}

	AddSRIOVResourceLimits(&resources, guest)

	// Should only have CPU — no SR-IOV resources added.
	if len(resources.Limits) != 1 {
		t.Errorf("Limits has %d entries, want 1 (CPU only)", len(resources.Limits))
	}
	if len(resources.Requests) != 1 {
		t.Errorf("Requests has %d entries, want 1 (CPU only)", len(resources.Requests))
	}
}

func TestAddSRIOVResourceLimits_OneSRIOV(t *testing.T) {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"),
		},
	}
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net"}},
			},
		},
	}

	AddSRIOVResourceLimits(&resources, guest)

	rn := corev1.ResourceName("intel.com/sriov_netdevice")
	lim, ok := resources.Limits[rn]
	if !ok {
		t.Fatal("Limits missing intel.com/sriov_netdevice")
	}
	if lim.Value() != 1 {
		t.Errorf("Limits[intel.com/sriov_netdevice] = %d, want 1", lim.Value())
	}

	req, ok := resources.Requests[rn]
	if !ok {
		t.Fatal("Requests missing intel.com/sriov_netdevice")
	}
	if req.Value() != 1 {
		t.Errorf("Requests[intel.com/sriov_netdevice] = %d, want 1", req.Value())
	}
}

func TestAddSRIOVResourceLimits_TwoSRIOV(t *testing.T) {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("4"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("4"),
		},
	}
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "rdma1", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net1"}},
				{Name: "rdma2", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "mellanox.com/cx6", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net2"}},
			},
		},
	}

	AddSRIOVResourceLimits(&resources, guest)

	rn1 := corev1.ResourceName("intel.com/sriov_netdevice")
	rn2 := corev1.ResourceName("mellanox.com/cx6")

	lim1, ok := resources.Limits[rn1]
	if !ok {
		t.Fatal("Limits missing intel.com/sriov_netdevice")
	}
	if lim1.Value() != 1 {
		t.Errorf("Limits[intel.com/sriov_netdevice] = %d, want 1", lim1.Value())
	}

	lim2, ok := resources.Limits[rn2]
	if !ok {
		t.Fatal("Limits missing mellanox.com/cx6")
	}
	if lim2.Value() != 1 {
		t.Errorf("Limits[mellanox.com/cx6] = %d, want 1", lim2.Value())
	}

	// Total: CPU + 2 SR-IOV resources = 3 entries.
	if len(resources.Limits) != 3 {
		t.Errorf("Limits has %d entries, want 3", len(resources.Limits))
	}
}

func TestAddSRIOVVolumesIfNeeded_NoSRIOV(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt", Type: swiftv1alpha1.InterfaceTypeBridge},
			},
		},
	}
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	addSRIOVVolumesIfNeeded(&volumes, &mounts, guest)

	if len(volumes) != 0 {
		t.Errorf("volumes = %d, want 0", len(volumes))
	}
	if len(mounts) != 0 {
		t.Errorf("mounts = %d, want 0", len(mounts))
	}
}

func TestAddSRIOVVolumesIfNeeded_WithSRIOV(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net"}},
			},
		},
	}
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	addSRIOVVolumesIfNeeded(&volumes, &mounts, guest)

	if len(volumes) != 1 {
		t.Fatalf("volumes = %d, want 1", len(volumes))
	}
	if volumes[0].Name != "dev-vfio" {
		t.Errorf("volume name = %q, want dev-vfio", volumes[0].Name)
	}
	if volumes[0].HostPath == nil || volumes[0].HostPath.Path != "/dev/vfio" {
		t.Errorf("volume hostPath = %v, want /dev/vfio", volumes[0].HostPath)
	}

	if len(mounts) != 1 {
		t.Fatalf("mounts = %d, want 1", len(mounts))
	}
	if mounts[0].Name != "dev-vfio" {
		t.Errorf("mount name = %q, want dev-vfio", mounts[0].Name)
	}
	if mounts[0].MountPath != "/dev/vfio" {
		t.Errorf("mount path = %q, want /dev/vfio", mounts[0].MountPath)
	}
}

func TestAddSRIOVVolumesIfNeeded_GPUAlreadyHasVFIO(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "sriov-net"}},
			},
		},
	}
	// Simulate GPU pod that already has dev-vfio volume.
	volumes := []corev1.Volume{
		{Name: "dev-vfio", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/vfio"}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "dev-vfio", MountPath: "/dev/vfio"},
	}

	addSRIOVVolumesIfNeeded(&volumes, &mounts, guest)

	// Should not add a duplicate.
	if len(volumes) != 1 {
		t.Errorf("volumes = %d, want 1 (no duplicate)", len(volumes))
	}
	if len(mounts) != 1 {
		t.Errorf("mounts = %d, want 1 (no duplicate)", len(mounts))
	}
}

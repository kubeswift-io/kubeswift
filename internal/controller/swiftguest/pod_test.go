package swiftguest

import (
	"strings"
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

	pod := BuildPod(guest, rg, "test-seed", "test-intent", nil)

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
		DataDisks:     []resolved.ResolvedDataDisk{{Name: "data", PVCName: "pvc-data", HostPath: "/var/lib/kubeswift/disks/data/image.raw", MountPath: "/var/lib/kubeswift/disks/data", Format: "raw", Ready: true}},
	}

	pod := BuildPod(guest, rg, "", "test-intent", nil)

	// Check volume exists.
	foundVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "data-disk-data" {
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
		t.Error("missing data-disk-data volume")
	}

	// Check mount exists on launcher.
	launcher := pod.Spec.Containers[0]
	foundMount := false
	for _, m := range launcher.VolumeMounts {
		if m.Name == "data-disk-data" {
			foundMount = true
			if m.MountPath != DisksDataPath {
				t.Errorf("data-disk mountPath = %q, want %q", m.MountPath, DisksDataPath)
			}
		}
	}
	if !foundMount {
		t.Error("launcher missing data-disk-data mount")
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

	pod := BuildPod(guest, rg, "", "test-intent", nil)

	for _, v := range pod.Spec.Volumes {
		if strings.HasPrefix(v.Name, "data-disk-") {
			t.Error("data-disk volume should not be present when DataDisks is empty")
		}
	}
	launcher := pod.Spec.Containers[0]
	for _, m := range launcher.VolumeMounts {
		if strings.HasPrefix(m.Name, "data-disk-") {
			t.Error("data-disk mount should not be present when DataDisks is empty")
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

	pod := BuildPod(guest, rg, "", "test-intent", nil)

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

	pod := BuildPod(guest, rg, "", "test-intent", nil)

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

	pod := BuildPod(guest, rg, "", "test-intent", nil)

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

	pod := BuildPod(guest, rg, "", "test-intent", nil)

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

	pod := BuildPod(guest, rg, "", "test-intent", nil)

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

	pod := BuildPod(guest, rg, "", "test-intent", nil)

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

	pod := BuildPod(guest, rg, "", "test-intent", nil)

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

	pod := BuildPod(guest, rg, "", "test-intent", nil)

	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("initContainers = %d, want 0 when no seed", len(pod.Spec.InitContainers))
	}
}

func TestBuildPod_CloneGrowInit_WhenNeedsGrowInit(t *testing.T) {
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
		Network:       true,
	}
	clone := &RootDiskCloneResult{
		PVCName:         "pvc",
		NeedsGrowInit:   true,
		SourceSizeBytes: 10 * 1024 * 1024 * 1024,
		TargetSizeBytes: 40 * 1024 * 1024 * 1024,
	}

	pod := BuildPod(guest, rg, "", "test-intent", clone)

	if len(pod.Spec.InitContainers) != 2 {
		t.Fatalf("initContainers = %d, want 2 (clone-grow-init + network-init)", len(pod.Spec.InitContainers))
	}
	grow := pod.Spec.InitContainers[0]
	if grow.Name != "clone-grow-init" {
		t.Errorf("initContainers[0] name = %q, want clone-grow-init", grow.Name)
	}
	if grow.Image != CloneGrowInitImage {
		t.Errorf("initContainers[0] image = %q, want %q", grow.Image, CloneGrowInitImage)
	}
	if pod.Spec.InitContainers[1].Name != "network-init" {
		t.Errorf("initContainers[1] name = %q, want network-init (clone-grow-init must run first)", pod.Spec.InitContainers[1].Name)
	}
	// Script must reference target size and call qemu-img resize + sgdisk -e.
	if len(grow.Command) < 3 {
		t.Fatalf("clone-grow-init command too short: %v", grow.Command)
	}
	script := grow.Command[2]
	for _, want := range []string{"qemu-img resize", "sgdisk -e", "42949672960"} {
		if !contains(script, want) {
			t.Errorf("clone-grow-init script missing %q; got %q", want, script)
		}
	}
	// Must mount root-disk at DisksRootPath.
	mounted := false
	for _, m := range grow.VolumeMounts {
		if m.Name == "root-disk" && m.MountPath == DisksRootPath {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("clone-grow-init missing root-disk mount at %s", DisksRootPath)
	}
}

func TestBuildPod_NoCloneGrowInit_WhenCopyStrategy(t *testing.T) {
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
		Network:       true,
	}
	// Copy path returns NeedsGrowInit=false.
	clone := &RootDiskCloneResult{PVCName: "pvc", NeedsGrowInit: false}

	pod := BuildPod(guest, rg, "", "test-intent", clone)

	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == "clone-grow-init" {
			t.Errorf("clone-grow-init must not be added on copy strategy")
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestBuildPod_NodeName_DiskBoot verifies that spec.NodeName, when set,
// is propagated to pod.Spec.NodeName for disk-boot guests. The Phase 1
// SwiftMigration controller relies on this to pin the launcher pod to
// the destination node during StopAndCopy.
func TestBuildPod_NodeName_DiskBoot(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pin", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			NodeName:      "miles",
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "", "test-intent", nil)

	if pod.Spec.NodeName != "miles" {
		t.Errorf("pod.Spec.NodeName = %q, want miles", pod.Spec.NodeName)
	}
	if pod.Spec.NodeSelector != nil {
		t.Errorf("disk-boot pod should not have NodeSelector when spec.NodeName is set; got %v", pod.Spec.NodeSelector)
	}
}

// TestBuildPod_NodeName_KernelBoot verifies that for kernel-boot guests,
// spec.NodeName is honored AND the existing kubeswift.io/kernel-node
// nodeSelector is preserved. This is the architect's defense-in-depth
// pattern: the webhook validates that NodeName is a kernel-labeled node,
// and the selector remains as a kubelet-time admission backstop.
func TestBuildPod_NodeName_KernelBoot(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-kpin", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			KernelRef:     &corev1.LocalObjectReference{Name: "k"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			NodeName:      "miles",
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources: resolved.Resources{CPU: 2, Memory: 2048},
		KernelBoot: &resolved.KernelBoot{
			LocalPath:     "/var/lib/kubeswift/kernels/default-k",
			KernelCmdline: "console=ttyS0",
		},
		Network: true,
	}

	pod := BuildPod(guest, rg, "", "test-intent", nil)

	if pod.Spec.NodeName != "miles" {
		t.Errorf("pod.Spec.NodeName = %q, want miles", pod.Spec.NodeName)
	}
	if pod.Spec.NodeSelector["kubeswift.io/kernel-node"] != "true" {
		t.Errorf("kubeswift.io/kernel-node selector should be preserved; got %v", pod.Spec.NodeSelector)
	}
}

// TestBuildPod_NodeName_Empty verifies the default behavior: when
// spec.NodeName is unset, pod.Spec.NodeName is empty (the scheduler picks).
// This is the pre-Phase-1 behavior and must be preserved for all existing
// SwiftGuests that haven't been touched by SwiftMigration.
func TestBuildPod_NodeName_Empty(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nopin", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			// NodeName intentionally empty.
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "", "test-intent", nil)

	if pod.Spec.NodeName != "" {
		t.Errorf("pod.Spec.NodeName = %q, want empty (scheduler-picked)", pod.Spec.NodeName)
	}
}

func TestBuildPod_Filesystems(t *testing.T) {
	hp := "/srv/share"
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "fs", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Filesystems: []swiftv1alpha1.Filesystem{
				{Name: "host", Source: swiftv1alpha1.FilesystemSource{HostPath: &hp}},
				{Name: "claim", ReadOnly: true, Source: swiftv1alpha1.FilesystemSource{
					PVCRef: &corev1.LocalObjectReference{Name: "data-pvc"}}},
			},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc-root"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "", "test-intent", nil)

	// hostPath volume (DirectoryOrCreate) at virtiofs-0.
	var v0, v1 *corev1.Volume
	for i := range pod.Spec.Volumes {
		switch pod.Spec.Volumes[i].Name {
		case "virtiofs-0":
			v0 = &pod.Spec.Volumes[i]
		case "virtiofs-1":
			v1 = &pod.Spec.Volumes[i]
		}
	}
	if v0 == nil || v0.HostPath == nil || v0.HostPath.Path != hp {
		t.Fatalf("virtiofs-0 hostPath volume missing/wrong: %+v", v0)
	}
	if v0.HostPath.Type == nil || *v0.HostPath.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("virtiofs-0 hostPath type = %v, want DirectoryOrCreate", v0.HostPath.Type)
	}
	if v1 == nil || v1.PersistentVolumeClaim == nil || v1.PersistentVolumeClaim.ClaimName != "data-pvc" {
		t.Fatalf("virtiofs-1 PVC volume missing/wrong: %+v", v1)
	}

	// Mounts at /var/lib/kubeswift/virtiofs/<name>; readOnly honored on the PVC one.
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		mounts[m.Name] = m
	}
	if m := mounts["virtiofs-0"]; m.MountPath != "/var/lib/kubeswift/virtiofs/host" || m.ReadOnly {
		t.Errorf("virtiofs-0 mount = %+v, want path=.../host readOnly=false", m)
	}
	if m := mounts["virtiofs-1"]; m.MountPath != "/var/lib/kubeswift/virtiofs/claim" || !m.ReadOnly {
		t.Errorf("virtiofs-1 mount = %+v, want path=.../claim readOnly=true", m)
	}
}

func TestBuildPod_VhostUserNet(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "vu", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "fast0", Type: swiftv1alpha1.InterfaceTypeVhostUser, Socket: "/var/run/vhost/fast0.sock"},
				{Name: "fast1", Type: swiftv1alpha1.InterfaceTypeVhostUser, Socket: "/var/run/vhost/fast1.sock"},
			},
		},
	}
	rg := &resolved.ResolvedGuest{
		Meta:          resolved.Meta{Namespace: "default", Name: "vu"},
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc-root"},
		Network:       true,
		Interfaces:    guest.Spec.Interfaces,
	}

	pod := BuildPod(guest, rg, "", "test-intent", nil)

	// Both sockets share /var/run/vhost -> exactly one deduped hostPath volume.
	var vols []corev1.Volume
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil && v.HostPath.Path == "/var/run/vhost" {
			vols = append(vols, v)
		}
	}
	if len(vols) != 1 {
		t.Fatalf("vhost dir volumes = %d, want 1 (deduped)", len(vols))
	}
	if vols[0].HostPath.Type == nil || *vols[0].HostPath.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("vhost volume type = %v, want DirectoryOrCreate", vols[0].HostPath.Type)
	}
	// Mounted at the same node path so CH (in-container) can reach the socket.
	found := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == vols[0].Name && m.MountPath == "/var/run/vhost" {
			found = true
		}
	}
	if !found {
		t.Error("launcher missing /var/run/vhost mount")
	}
}

func TestBuildPod_VhostUserSocketDedup(t *testing.T) {
	// A vhost-user NIC and a vhost-user-blk device share /var/run/vhost ->
	// exactly one deduped mount; a device in /run/spdk gets its own.
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "vud", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "fast0", Type: swiftv1alpha1.InterfaceTypeVhostUser, Socket: "/var/run/vhost/fast0.sock"},
			},
			VhostUserDevices: []swiftv1alpha1.VhostUserDevice{
				{Name: "blk0", Type: swiftv1alpha1.VhostUserDeviceTypeBlk, Socket: "/var/run/vhost/blk0.sock"},
				{Name: "gen0", Type: swiftv1alpha1.VhostUserDeviceTypeGeneric, Socket: "/run/spdk/gen.sock", VirtioID: "block"},
			},
		},
	}
	rg := &resolved.ResolvedGuest{
		Meta:          resolved.Meta{Namespace: "default", Name: "vud"},
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc-root"},
		Network:       true,
		Interfaces:    guest.Spec.Interfaces,
	}
	pod := BuildPod(guest, rg, "", "test-intent", nil)

	dirs := map[string]int{}
	mountPaths := map[string]int{}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil && (v.HostPath.Path == "/var/run/vhost" || v.HostPath.Path == "/run/spdk") {
			dirs[v.HostPath.Path]++
		}
	}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/var/run/vhost" || m.MountPath == "/run/spdk" {
			mountPaths[m.MountPath]++
		}
	}
	// /var/run/vhost mounted exactly once despite NIC + blk both under it.
	if dirs["/var/run/vhost"] != 1 || mountPaths["/var/run/vhost"] != 1 {
		t.Errorf("/var/run/vhost vol=%d mount=%d, want 1/1 (deduped across net+device)", dirs["/var/run/vhost"], mountPaths["/var/run/vhost"])
	}
	if dirs["/run/spdk"] != 1 || mountPaths["/run/spdk"] != 1 {
		t.Errorf("/run/spdk vol=%d mount=%d, want 1/1", dirs["/run/spdk"], mountPaths["/run/spdk"])
	}
}

func TestBuildPod_NetworkInitHasIntentMount(t *testing.T) {
	// Regression: the network-init init container MUST mount the runtime-intent
	// ConfigMap, else network-init.sh's has_nics() can't read the intent and
	// silently falls back to legacy single-NIC br0/tap0 — leaving multi-NIC
	// Multus interfaces unbridged. Also mounts the shared "run" emptyDir.
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "ni", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt"},
				{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "nad"}},
			},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Network:       true,
		Interfaces:    guest.Spec.Interfaces,
	}
	pod := BuildPod(guest, rg, "", "test-intent", nil)

	var ni *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "network-init" {
			ni = &pod.Spec.InitContainers[i]
		}
	}
	if ni == nil {
		t.Fatal("no network-init container")
	}
	got := map[string]string{}
	for _, m := range ni.VolumeMounts {
		got[m.Name] = m.MountPath
	}
	if got["runtime-intent"] != IntentPath {
		t.Errorf("network-init runtime-intent mount = %q, want %q", got["runtime-intent"], IntentPath)
	}
	if got["run"] != RunDirPath {
		t.Errorf("network-init run mount = %q, want %q", got["run"], RunDirPath)
	}
	// The referenced volumes must exist in the pod (else the pod is invalid).
	vols := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = true
	}
	if !vols["runtime-intent"] || !vols["run"] {
		t.Errorf("pod missing volumes for network-init mounts: %v", vols)
	}
}

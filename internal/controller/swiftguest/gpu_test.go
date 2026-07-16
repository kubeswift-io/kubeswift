package swiftguest

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	kscheme "github.com/kubeswift-io/kubeswift/internal/scheme"
)

func gpuGuest(nodeName string, devices []string, partitionID int) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
			GPUProfileRef:  &corev1.LocalObjectReference{Name: "gpu-profile"},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{
			GPU: &swiftv1alpha1.GPUStatus{
				Devices:     devices,
				PartitionID: partitionID,
				NUMANodes:   []int{0},
				Hypervisor:  "qemu",
				NodeName:    nodeName,
			},
		},
	}
}

func gpuResolvedGuest() *resolved.ResolvedGuest {
	return &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 16, Memory: 32768},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc-root"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
		Network:       true,
	}
}

func TestBuildGPUDiskBootPod_NodeSelector(t *testing.T) {
	guest := gpuGuest("gpu-node-42", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	hostname, ok := pod.Spec.NodeSelector["kubernetes.io/hostname"]
	if !ok {
		t.Fatal("missing nodeSelector kubernetes.io/hostname")
	}
	if hostname != "gpu-node-42" {
		t.Errorf("hostname = %q, want gpu-node-42", hostname)
	}
}

func TestBuildGPUDiskBootPod_InitContainers(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0", "0000:3d:00.0"}, 2)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	if len(pod.Spec.InitContainers) < 2 {
		t.Fatalf("initContainers = %d, want at least 2 (gpu-init + network-init)", len(pod.Spec.InitContainers))
	}

	// gpu-init must run first.
	if pod.Spec.InitContainers[0].Name != "gpu-init" {
		t.Errorf("first init container = %q, want gpu-init", pod.Spec.InitContainers[0].Name)
	}
	if pod.Spec.InitContainers[1].Name != "network-init" {
		t.Errorf("second init container = %q, want network-init", pod.Spec.InitContainers[1].Name)
	}

	// Verify gpu-init env vars.
	gpuInit := pod.Spec.InitContainers[0]
	envMap := map[string]string{}
	for _, e := range gpuInit.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["GPU_PCI_ADDRESSES"] != "0000:17:00.0,0000:3d:00.0" {
		t.Errorf("GPU_PCI_ADDRESSES = %q, want comma-separated addresses", envMap["GPU_PCI_ADDRESSES"])
	}
	if envMap["GPU_PARTITION_ID"] != "2" {
		t.Errorf("GPU_PARTITION_ID = %q, want 2", envMap["GPU_PARTITION_ID"])
	}
}

func TestBuildGPUDiskBootPod_Volumes(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	volNames := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		volNames[v.Name] = true
	}

	if !volNames["dev-vfio"] {
		t.Error("missing dev-vfio volume")
	}
	if !volNames["hugepages"] {
		t.Error("missing hugepages volume")
	}
	if !volNames["dev-kvm"] {
		t.Error("missing dev-kvm volume")
	}

	// Verify hugepage volume medium.
	for _, v := range pod.Spec.Volumes {
		if v.Name == "hugepages" && v.VolumeSource.EmptyDir != nil {
			if v.VolumeSource.EmptyDir.Medium != "HugePages-1Gi" {
				t.Errorf("hugepages medium = %q, want HugePages-1Gi", v.VolumeSource.EmptyDir.Medium)
			}
		}
	}

	// Verify launcher mounts include /dev/vfio and /dev/hugepages.
	launcher := pod.Spec.Containers[0]
	mountNames := map[string]bool{}
	for _, m := range launcher.VolumeMounts {
		mountNames[m.Name] = true
	}
	if !mountNames["dev-vfio"] {
		t.Error("launcher missing dev-vfio mount")
	}
	if !mountNames["hugepages"] {
		t.Error("launcher missing hugepages mount")
	}
}

func TestBuildGPUDiskBootPod_NoHugepages(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "", nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == "hugepages" {
			t.Error("hugepages volume should not be present when hugepages is empty")
		}
	}
	launcher := pod.Spec.Containers[0]
	for _, m := range launcher.VolumeMounts {
		if m.Name == "hugepages" {
			t.Error("hugepages mount should not be present when hugepages is empty")
		}
	}
}

func TestBuildGPUDiskBootPod_RestartPolicy(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %v, want Never", pod.Spec.RestartPolicy)
	}
}

func TestBuildPod_NonGPU_Unchanged(t *testing.T) {
	// Regression test: BuildPod for a non-GPU guest should not include GPU volumes.
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "vanilla", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "test-seed", "test-intent", nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == "dev-vfio" {
			t.Error("non-GPU pod should not have dev-vfio volume")
		}
		if v.Name == "hugepages" {
			t.Error("non-GPU pod should not have hugepages volume")
		}
	}
	// No gpu-init container.
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == "gpu-init" {
			t.Error("non-GPU pod should not have gpu-init init container")
		}
	}
}

func TestBuildGPUDiskBootPod_DataDiskVolume(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()
	rg.DataDisks = []resolved.ResolvedDataDisk{{Name: "data", PVCName: "pvc-data", HostPath: "/var/lib/kubeswift/disks/data/image.raw", MountPath: "/var/lib/kubeswift/disks/data", Format: "raw", Ready: true}}

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	foundVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "data-disk-data" {
			foundVol = true
			if v.VolumeSource.PersistentVolumeClaim.ClaimName != "pvc-data" {
				t.Errorf("data-disk PVC = %q, want pvc-data", v.VolumeSource.PersistentVolumeClaim.ClaimName)
			}
		}
	}
	if !foundVol {
		t.Error("GPU pod missing data-disk-data volume")
	}

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
		t.Error("GPU launcher missing data-disk-data mount")
	}
}

func TestBuildGPUDiskBootPod_NoDataDisk(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == "data-disk" {
			t.Error("GPU pod should not have data-disk volume when DataDisk is nil")
		}
	}
}

// --- expandCPURange tests ---

func TestExpandCPURange_Complex(t *testing.T) {
	cpus, err := expandCPURange("0-47,96-143")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cpus) != 96 {
		t.Errorf("len = %d, want 96", len(cpus))
	}
	// Check bounds.
	if cpus[0] != 0 {
		t.Errorf("first CPU = %d, want 0", cpus[0])
	}
	if cpus[47] != 47 {
		t.Errorf("cpus[47] = %d, want 47", cpus[47])
	}
	if cpus[48] != 96 {
		t.Errorf("cpus[48] = %d, want 96 (start of second range)", cpus[48])
	}
	if cpus[95] != 143 {
		t.Errorf("last CPU = %d, want 143", cpus[95])
	}
}

func TestExpandCPURange_Single(t *testing.T) {
	cpus, err := expandCPURange("5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cpus) != 1 || cpus[0] != 5 {
		t.Errorf("cpus = %v, want [5]", cpus)
	}
}

func TestExpandCPURange_Invalid(t *testing.T) {
	_, err := expandCPURange("abc")
	if err == nil {
		t.Error("expected error for invalid CPU range")
	}
}

func TestExpandCPURange_InvalidRange(t *testing.T) {
	_, err := expandCPURange("1-abc")
	if err == nil {
		t.Error("expected error for invalid range bound")
	}
}

// --- buildNUMAAndPinning tests ---

func TestBuildNUMAAndPinning_TwoSockets(t *testing.T) {
	numaSpec := &gpuv1alpha1.NUMATopologySpec{
		Sockets:           2,
		CoresPerSocket:    40,
		ThreadsPerCore:    1,
		MemoryPerSocketMi: 983040,
	}
	allocatedNUMANodes := []int{0, 1}
	hostNUMANodes := []gpuv1alpha1.NUMANodeInfo{
		{ID: 0, CPUs: "0-47,96-143", MemoryMi: 1048576},
		{ID: 1, CPUs: "48-95,144-191", MemoryMi: 1048576},
	}

	numaIntent, pins := buildNUMAAndPinning(numaSpec, allocatedNUMANodes, hostNUMANodes, true)

	if numaIntent == nil {
		t.Fatal("numaIntent is nil")
	}
	if len(numaIntent.Nodes) != 2 {
		t.Fatalf("NUMA nodes = %d, want 2", len(numaIntent.Nodes))
	}

	// Verify virtual NUMA node 0: CPUs 0-39, memoryMi 983040.
	n0 := numaIntent.Nodes[0]
	if n0.ID != 0 || n0.CPUs != "0-39" || n0.MemoryMi != 983040 {
		t.Errorf("node 0 = %+v, want id=0 cpus=0-39 mem=983040", n0)
	}
	n1 := numaIntent.Nodes[1]
	if n1.ID != 1 || n1.CPUs != "40-79" || n1.MemoryMi != 983040 {
		t.Errorf("node 1 = %+v, want id=1 cpus=40-79 mem=983040", n1)
	}

	// Verify pinning exists and maps vCPUs to physical CPUs.
	if len(pins) == 0 {
		t.Fatal("expected vCPU pins, got none")
	}
	// With 2 sockets x 40 cores = 80 vCPUs total, pins should cover both sockets.
	if len(pins) != 80 {
		t.Errorf("pins count = %d, want 80", len(pins))
	}
	// First vCPU (socket 0) should map to physical NUMA 0 CPU.
	if pins[0].VCPU != 0 {
		t.Errorf("first pin vCPU = %d, want 0", pins[0].VCPU)
	}
	if pins[0].HostCPU != 0 {
		t.Errorf("first pin hostCPU = %d, want 0 (first CPU of NUMA 0)", pins[0].HostCPU)
	}
}

func TestBuildNUMAAndPinning_NoPinning(t *testing.T) {
	numaSpec := &gpuv1alpha1.NUMATopologySpec{
		Sockets:           2,
		CoresPerSocket:    40,
		ThreadsPerCore:    1,
		MemoryPerSocketMi: 983040,
	}
	allocatedNUMANodes := []int{0, 1}
	hostNUMANodes := []gpuv1alpha1.NUMANodeInfo{
		{ID: 0, CPUs: "0-47", MemoryMi: 1048576},
		{ID: 1, CPUs: "48-95", MemoryMi: 1048576},
	}

	numaIntent, pins := buildNUMAAndPinning(numaSpec, allocatedNUMANodes, hostNUMANodes, false)

	if numaIntent == nil {
		t.Fatal("numaIntent should still be set when pinning is off")
	}
	if pins != nil {
		t.Errorf("pins should be nil when vcpuPinning=false, got %d pins", len(pins))
	}
}

func TestBuildNUMAAndPinning_SingleCore(t *testing.T) {
	numaSpec := &gpuv1alpha1.NUMATopologySpec{
		Sockets:           1,
		CoresPerSocket:    1,
		ThreadsPerCore:    1,
		MemoryPerSocketMi: 4096,
	}
	allocatedNUMANodes := []int{0}
	hostNUMANodes := []gpuv1alpha1.NUMANodeInfo{
		{ID: 0, CPUs: "0-7", MemoryMi: 65536},
	}

	numaIntent, pins := buildNUMAAndPinning(numaSpec, allocatedNUMANodes, hostNUMANodes, true)

	if len(numaIntent.Nodes) != 1 {
		t.Fatalf("NUMA nodes = %d, want 1", len(numaIntent.Nodes))
	}
	// Single core should produce cpuRange "0" (not "0-0").
	if numaIntent.Nodes[0].CPUs != "0" {
		t.Errorf("cpus = %q, want '0' for single core", numaIntent.Nodes[0].CPUs)
	}
	if len(pins) != 1 {
		t.Fatalf("pins = %d, want 1", len(pins))
	}
	if pins[0].VCPU != 0 || pins[0].HostCPU != 0 {
		t.Errorf("pin = %+v, want vcpu=0 hostCPU=0", pins[0])
	}
}

// --- isGPUAllocated tests ---

func TestIsGPUAllocated_True(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Status: swiftv1alpha1.SwiftGuestStatus{
			Conditions: []metav1.Condition{
				{Type: swiftv1alpha1.ConditionGPUAllocated, Status: metav1.ConditionTrue},
			},
		},
	}
	if !isGPUAllocated(guest) {
		t.Error("expected true when GPUAllocated=True")
	}
}

func TestIsGPUAllocated_False(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Status: swiftv1alpha1.SwiftGuestStatus{
			Conditions: []metav1.Condition{
				{Type: swiftv1alpha1.ConditionGPUAllocated, Status: metav1.ConditionFalse},
			},
		},
	}
	if isGPUAllocated(guest) {
		t.Error("expected false when GPUAllocated=False")
	}
}

func TestIsGPUAllocated_NoCondition(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{}
	if isGPUAllocated(guest) {
		t.Error("expected false when no conditions")
	}
}

// --- GPUIntent construction helper tests ---

func TestGPUIntentDeviceConstruction(t *testing.T) {
	// Verify that VFIODeviceIntent fields round-trip correctly.
	dev := runtimeintent.VFIODeviceIntent{
		HostPath:        "/sys/bus/pci/devices/0000:17:00.0/",
		PCIAddress:      "0000:17:00.0",
		PCIeRootPort:    true,
		GPUDirectClique: 0,
		NoMmap:          true,
		NUMANode:        0,
	}
	if dev.HostPath != "/sys/bus/pci/devices/0000:17:00.0/" {
		t.Errorf("hostPath = %q", dev.HostPath)
	}
	if !dev.PCIeRootPort {
		t.Error("pcieRootPort should be true")
	}
	if !dev.NoMmap {
		t.Error("noMmap should be true")
	}
}

// TestBuildPodDispatcher_NodeNameGPUDisagreement verifies the architect's
// precedence rule (Q2(d)): when both spec.NodeName and a GPU allocation
// are present and they disagree, the dispatcher refuses to build with a
// clear error. This is defense-in-depth — the SwiftMigration validation
// webhook (commit 4) rejects cross-node GPU migrations at submission
// time, so this branch normally never fires; the check exists to catch
// any webhook bypass or future Phase 4 controller bug.
func TestBuildPodDispatcher_NodeNameGPUDisagreement(t *testing.T) {
	guest := gpuGuest("miles", []string{"0000:01:00.0"}, -1)
	guest.Spec.NodeName = "boba" // disagrees with status.GPU.NodeName
	// Mark GPUAllocated=True so the dispatcher reaches the GPU branch
	// (not strictly necessary — the precedence check fires first — but
	// matches the realistic state).
	guest.Status.Conditions = []metav1.Condition{
		{Type: swiftv1alpha1.ConditionGPUAllocated, Status: metav1.ConditionTrue},
	}
	rg := gpuResolvedGuest()

	// Precedence check fires before any API call, so an empty fake client
	// is sufficient.
	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	pod, err := r.buildPod(context.Background(), guest, rg, "seed-cm", "intent-cm", nil)
	if err == nil {
		t.Fatal("buildPod should reject NodeName/GPU.NodeName disagreement; got nil error")
	}
	if pod != nil {
		t.Error("buildPod should not return a pod when NodeName disagrees with GPU.NodeName")
	}
	// Operators reading the Resolved=False condition need to see both
	// node names in the message to diagnose; assert both appear.
	if !strings.Contains(err.Error(), "must commit both together") {
		t.Errorf("error message should mention the cutover-consistency assertion; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "boba") || !strings.Contains(err.Error(), "miles") {
		t.Errorf("error message should name both spec.nodeName (boba) and status.gpu.nodeName (miles); got %q", err.Error())
	}
}

// TestBuildPodDispatcher_NodeNameSetGPUNil verifies the realistic
// startup-order state: SwiftGuest has gpuProfileRef AND spec.NodeName
// set, but status.GPU is nil because the SwiftGPU controller hasn't
// allocated yet. The precedence guard short-circuits (status.GPU != nil
// is false), and the dispatcher's GPUAllocated gate higher up in
// Reconcile would prevent reaching buildPod in practice — but if it
// does reach here, the precedence check must not error spuriously.
func TestBuildPodDispatcher_NodeNameSetGPUNil(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-pending", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
			GPUProfileRef:  &corev1.LocalObjectReference{Name: "gpu-profile"},
			NodeName:       "miles",
		},
		// Status.GPU intentionally nil.
	}
	rg := gpuResolvedGuest()
	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	// Precedence check should NOT fire (status.GPU is nil). The dispatcher
	// will then skip the GPU branch (status.GPU != nil is false) and fall
	// through to BuildPod which honors spec.NodeName.
	_, err := r.buildPod(context.Background(), guest, rg, "seed-cm", "intent-cm", nil)
	if err != nil && strings.Contains(err.Error(), "cross-node GPU migration is not supported") {
		t.Errorf("precedence check fired spuriously when status.GPU is nil; err=%v", err)
	}
}

// TestBuildPodDispatcher_NodeName_RestoreBranch verifies that a guest
// in active Tier B restore (annotated with active-restore) honors
// spec.NodeName. Without this, a SwiftMigration of a guest that's
// mid-restore would silently drop pinning when the restore-branch
// pod is built. This is load-bearing for Phase 1's drive-forward-
// post-cutover pattern (Risk 2 from the architect review): the
// migration controller must be able to recreate launcher pods
// reliably regardless of which dispatcher branch they take.
func TestBuildPodDispatcher_NodeName_RestoreBranch(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restoring",
			Namespace: "default",
			Annotations: map[string]string{
				// The exact annotation key/value comes from
				// RestoreParamsFromAnnotations; it is enough that the
				// annotation is present and parseable. Use the same
				// constant the controller writes.
				"snapshot.kubeswift.io/active-restore": `{"snapshotName":"snap-1","backendType":"local","snapshotPath":"/var/lib/kubeswift/snapshots/default-snap-1","sourceGuest":"src","clone":false}`,
			},
		},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
			NodeName:       "miles",
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc-root"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
		Network:       true,
	}
	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	pod, err := r.buildPod(context.Background(), guest, rg, "seed-cm", "intent-cm", nil)
	if err != nil {
		t.Fatalf("buildPod returned err = %v on restore branch, want nil", err)
	}
	if pod == nil {
		t.Fatal("buildPod returned nil pod on restore branch")
	}
	if pod.Spec.NodeName != "miles" {
		t.Errorf("restore-branch pod.Spec.NodeName = %q, want miles (applyNodeName must run on restore branch)", pod.Spec.NodeName)
	}
}

// TestBuildPodDispatcher_NodeNameGPUAgreement verifies that when
// spec.NodeName matches status.GPU.NodeName, the dispatcher builds the
// pod normally (does not error out).
func TestBuildPodDispatcher_NodeNameGPUAgreement(t *testing.T) {
	guest := gpuGuest("miles", []string{"0000:01:00.0"}, -1)
	guest.Spec.NodeName = "miles" // matches status.GPU.NodeName
	guest.Status.Conditions = []metav1.Condition{
		{Type: swiftv1alpha1.ConditionGPUAllocated, Status: metav1.ConditionTrue},
	}
	rg := gpuResolvedGuest()

	// Provide a SwiftGPUProfile in the fake client so the dispatcher's
	// r.Get call succeeds. Use a minimal profile.
	scheme := runtime.NewScheme()
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	scheme.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(scheme, gvSwift)
	gvGPU := schema.GroupVersion{Group: "gpu.kubeswift.io", Version: "v1alpha1"}
	scheme.AddKnownTypes(gvGPU, &gpuv1alpha1.SwiftGPUProfile{}, &gpuv1alpha1.SwiftGPUProfileList{})
	metav1.AddToGroupVersion(scheme, gvGPU)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1: %v", err)
	}

	profile := &gpuv1alpha1.SwiftGPUProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-profile", Namespace: "default"},
		Spec: gpuv1alpha1.SwiftGPUProfileSpec{
			Count:         1,
			Tier:          "pcie",
			PartitionMode: "isolated",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	pod, err := r.buildPod(context.Background(), guest, rg, "seed-cm", "intent-cm", nil)
	if err != nil {
		t.Fatalf("buildPod returned err = %v, want nil when NodeName matches GPU.NodeName", err)
	}
	if pod == nil {
		t.Fatal("buildPod returned nil pod")
	}
	// GPU pods pin via NodeSelector kubernetes.io/hostname, not spec.NodeName.
	// The architect's Q2(d) rationale: don't risk regression on the GPU e2e
	// validated on Hetzner by switching the GPU path to direct binding;
	// the precedence check above guarantees the pinned node matches either
	// way.
	if pod.Spec.NodeSelector["kubernetes.io/hostname"] != "miles" {
		t.Errorf("GPU pod NodeSelector hostname = %q, want miles", pod.Spec.NodeSelector["kubernetes.io/hostname"])
	}
	// Lock in the architect's "GPU pods stay on selector, not direct
	// binding" decision: pod.Spec.NodeName must remain empty even when
	// spec.NodeName was set (because spec.NodeName == GPU.NodeName, the
	// effective pinning is via the selector).
	if pod.Spec.NodeName != "" {
		t.Errorf("GPU pod.Spec.NodeName = %q, want empty (GPU pods pin via NodeSelector, not direct binding)", pod.Spec.NodeName)
	}
}

// TestBuildPodDispatcher_GPUOnlyNoMigrationNodeName verifies the normal
// (non-migration) GPU flow: spec.NodeName is empty, status.GPU.NodeName
// is set by the SwiftGPU controller, and the dispatcher proceeds to the
// GPU branch. The precedence guard short-circuits cleanly. This is the
// pre-Phase-1 GPU happy path and must not regress.
func TestBuildPodDispatcher_GPUOnlyNoMigrationNodeName(t *testing.T) {
	guest := gpuGuest("miles", []string{"0000:01:00.0"}, -1)
	// spec.NodeName intentionally empty.
	guest.Status.Conditions = []metav1.Condition{
		{Type: swiftv1alpha1.ConditionGPUAllocated, Status: metav1.ConditionTrue},
	}
	rg := gpuResolvedGuest()

	scheme := runtime.NewScheme()
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	scheme.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(scheme, gvSwift)
	gvGPU := schema.GroupVersion{Group: "gpu.kubeswift.io", Version: "v1alpha1"}
	scheme.AddKnownTypes(gvGPU, &gpuv1alpha1.SwiftGPUProfile{}, &gpuv1alpha1.SwiftGPUProfileList{})
	metav1.AddToGroupVersion(scheme, gvGPU)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1: %v", err)
	}
	profile := &gpuv1alpha1.SwiftGPUProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-profile", Namespace: "default"},
		Spec: gpuv1alpha1.SwiftGPUProfileSpec{
			Count:         1,
			Tier:          "pcie",
			PartitionMode: "isolated",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	pod, err := r.buildPod(context.Background(), guest, rg, "seed-cm", "intent-cm", nil)
	if err != nil {
		t.Fatalf("buildPod returned err = %v on normal GPU flow (no migration), want nil", err)
	}
	if pod == nil {
		t.Fatal("buildPod returned nil pod")
	}
	if pod.Spec.NodeSelector["kubernetes.io/hostname"] != "miles" {
		t.Errorf("GPU pod NodeSelector hostname = %q, want miles", pod.Spec.NodeSelector["kubernetes.io/hostname"])
	}
}

// TestBuildGPUIntent_HGXSharedTier2 exercises the FULL Tier-2 intent assembly
// against a hardware-faithful HGX H100 node (real BDFs + the dual-socket NUMA
// split from the NVIDIA WP-12736-002 integration guide): every SXM device
// behind a pcie-root-port with x-no-mmap, per-device NUMA affinity looked up
// from the node inventory, OVMF firmware, 1G hugepages, the FM partition id,
// and the virtual-NUMA + vCPU-pinning layout. This is the allocation→intent
// boundary a real HGX box would otherwise be the first to test.
func TestBuildGPUIntent_HGXSharedTier2(t *testing.T) {
	profile := &gpuv1alpha1.SwiftGPUProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "hgx4", Namespace: "default"},
		Spec: gpuv1alpha1.SwiftGPUProfileSpec{
			Count:         4,
			Tier:          "hgx-shared",
			PartitionMode: "shared",
			PCIeTopology: &gpuv1alpha1.PCIeTopologySpec{
				RootPortPerDevice: true,
				NoMmap:            true,
			},
			NUMATopology: &gpuv1alpha1.NUMATopologySpec{
				Sockets:           2,
				CoresPerSocket:    20,
				ThreadsPerCore:    1,
				MemoryPerSocketMi: 491520,
			},
			Hugepages:   "1Gi",
			VCPUPinning: true,
			FabricManager: &gpuv1alpha1.FabricManagerSpec{
				RequiredVersion: "550.163.01",
			},
		},
	}
	// The allocation the SwiftGPU controller produced: partition 1's members
	// (physicalIds 1-4 → BDFs 86/87/b8/bb, ALL on NUMA 1 — the guide's real
	// Module-ID mapping).
	node := &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: "hgx-0"},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			GPUModel: "H100-SXM",
			Host: gpuv1alpha1.HostTopology{
				NUMANodes: []gpuv1alpha1.NUMANodeInfo{
					{ID: 0, CPUs: "0-55,112-167", MemoryMi: 1048576},
					{ID: 1, CPUs: "56-111,168-223", MemoryMi: 1048576},
				},
			},
			GPUs: []gpuv1alpha1.GPUDevice{
				{Index: 0, PCIAddress: "0000:0f:00.0", Model: "H100-SXM", NUMANode: 0},
				{Index: 1, PCIAddress: "0000:10:00.0", Model: "H100-SXM", NUMANode: 0},
				{Index: 2, PCIAddress: "0000:41:00.0", Model: "H100-SXM", NUMANode: 0},
				{Index: 3, PCIAddress: "0000:44:00.0", Model: "H100-SXM", NUMANode: 0},
				{Index: 4, PCIAddress: "0000:86:00.0", Model: "H100-SXM", NUMANode: 1},
				{Index: 5, PCIAddress: "0000:87:00.0", Model: "H100-SXM", NUMANode: 1},
				{Index: 6, PCIAddress: "0000:b8:00.0", Model: "H100-SXM", NUMANode: 1},
				{Index: 7, PCIAddress: "0000:bb:00.0", Model: "H100-SXM", NUMANode: 1},
			},
			FabricManager: &gpuv1alpha1.FabricManagerStatus{
				Installed: true, Version: "550.163.01", Running: true,
				Partitions: []gpuv1alpha1.FMPartitionStatus{
					{ID: 1, GPUIndices: []int{4, 6, 5, 7}, AllocatedTo: "default/hgx-guest"},
				},
			},
		},
	}
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "hgx-guest", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			GPUProfileRef: &corev1.LocalObjectReference{Name: "hgx4"},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{
			GPU: &swiftv1alpha1.GPUStatus{
				Devices:     []string{"0000:86:00.0", "0000:87:00.0", "0000:b8:00.0", "0000:bb:00.0"},
				PartitionID: 1,
				NUMANodes:   []int{1},
				Hypervisor:  "qemu",
				NodeName:    "hgx-0",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(kscheme.Scheme).
		WithObjects(profile, node, guest).
		Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: kscheme.Scheme}

	gi, err := r.buildGPUIntent(context.Background(), guest)
	if err != nil {
		t.Fatalf("buildGPUIntent: %v", err)
	}
	if gi.Firmware != "ovmf" {
		t.Errorf("firmware = %q, want ovmf (QEMU Tier-2)", gi.Firmware)
	}
	if gi.Hugepages != "1G" {
		t.Errorf("hugepages = %q, want 1G (normalized from 1Gi)", gi.Hugepages)
	}
	if gi.FabricManagerPartitionID != 1 {
		t.Errorf("fmPartition = %d, want 1", gi.FabricManagerPartitionID)
	}
	if len(gi.Devices) != 4 {
		t.Fatalf("devices = %d, want 4", len(gi.Devices))
	}
	for _, d := range gi.Devices {
		if !d.PCIeRootPort {
			t.Errorf("device %s must be behind a pcie-root-port (SXM: CUDA rejects flat)", d.PCIAddress)
		}
		if !d.NoMmap {
			t.Errorf("device %s must carry noMmap (large BARs)", d.PCIAddress)
		}
		if d.NUMANode != 1 {
			t.Errorf("device %s numaNode = %d, want 1 (from the node inventory)", d.PCIAddress, d.NUMANode)
		}
		if d.HostPath != "/sys/bus/pci/devices/"+d.PCIAddress+"/" {
			t.Errorf("device hostPath = %q", d.HostPath)
		}
	}
	if gi.NUMA == nil || len(gi.NUMA.Nodes) != 2 {
		t.Fatalf("virtual NUMA layout missing: %+v", gi.NUMA)
	}
	if gi.NUMA.Nodes[0].MemoryMi != 491520 {
		t.Errorf("numa node 0 memoryMi = %d", gi.NUMA.Nodes[0].MemoryMi)
	}
	if len(gi.VCPUPinning) == 0 {
		t.Error("vcpuPinning must be computed (profile.vcpuPinning=true)")
	}
}

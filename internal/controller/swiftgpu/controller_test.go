package swiftgpu

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

// --- Test fixture helpers ---

func testGPUNode(name string, gpus []gpuv1alpha1.GPUDevice, fm *gpuv1alpha1.FabricManagerStatus) *gpuv1alpha1.SwiftGPUNode {
	free := 0
	model := ""
	for _, g := range gpus {
		if !g.Allocated {
			free++
		}
		if model == "" {
			model = g.Model
		}
	}
	return &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			Phase:    "Ready",
			GPUCount: len(gpus),
			FreeGPUs: free,
			GPUModel: model,
			GPUs:     gpus,
			Host: gpuv1alpha1.HostTopology{
				NUMANodes: []gpuv1alpha1.NUMANodeInfo{
					{ID: 0, CPUs: "0-47", MemoryMi: 1048576},
					{ID: 1, CPUs: "48-95", MemoryMi: 1048576},
				},
				IOMMUEnabled: true,
			},
			FabricManager: fm,
		},
	}
}

func testGPUProfile(name, namespace, tier, model string, count int, partitionMode string) *gpuv1alpha1.SwiftGPUProfile {
	return &gpuv1alpha1.SwiftGPUProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: gpuv1alpha1.SwiftGPUProfileSpec{
			Count:         count,
			Model:         model,
			Tier:          tier,
			PartitionMode: partitionMode,
		},
	}
}

func testSwiftGuest(name, namespace string, gpuProfileRef *corev1.LocalObjectReference) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "ubuntu-focal"},
			GuestClassRef: corev1.LocalObjectReference{Name: "default"},
			GPUProfileRef: gpuProfileRef,
		},
	}
}

func eightGPUs() []gpuv1alpha1.GPUDevice {
	return []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(1, "0000:3d:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(2, "0000:60:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(3, "0000:70:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(4, "0000:80:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(5, "0000:90:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(6, "0000:a0:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(7, "0000:b0:00.0", "NVIDIA H200 SXM", 1, false, ""),
	}
}

func newReconciler(objs ...client.Object) *SwiftGPUReconciler {
	cb := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithStatusSubresource(
		&swiftv1alpha1.SwiftGuest{},
		&gpuv1alpha1.SwiftGPUNode{},
	)
	for _, o := range objs {
		cb = cb.WithObjects(o)
	}
	return &SwiftGPUReconciler{
		Client: cb.Build(),
		Scheme: scheme.Scheme,
	}
}

func reconcileGuest(r *SwiftGPUReconciler, name, namespace string) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
}

func getGuest(r *SwiftGPUReconciler, name, namespace string) (*swiftv1alpha1.SwiftGuest, error) {
	var guest swiftv1alpha1.SwiftGuest
	err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, &guest)
	return &guest, err
}

func getGPUNode(r *SwiftGPUReconciler, name string) (*gpuv1alpha1.SwiftGPUNode, error) {
	var node gpuv1alpha1.SwiftGPUNode
	err := r.Get(context.Background(), types.NamespacedName{Name: name}, &node)
	return &node, err
}

func hasGPUAllocatedCondition(guest *swiftv1alpha1.SwiftGuest, status metav1.ConditionStatus) bool {
	for _, c := range guest.Status.Conditions {
		if c.Type == swiftv1alpha1.ConditionGPUAllocated && c.Status == status {
			return true
		}
	}
	return false
}

// --- Tests ---

func TestReconcile_NoGPUProfileRef(t *testing.T) {
	guest := testSwiftGuest("no-gpu", "default", nil)
	r := newReconciler(guest)

	res, err := reconcileGuest(r, "no-gpu", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", res.RequeueAfter)
	}

	// Verify no finalizer was added.
	g, _ := getGuest(r, "no-gpu", "default")
	for _, f := range g.Finalizers {
		if f == GPUFinalizerName {
			t.Error("finalizer should not be added when no gpuProfileRef")
		}
	}
}

func TestReconcile_AllocateSuccess(t *testing.T) {
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "pcie-profile"})
	profile := testGPUProfile("pcie-profile", "default", "pcie", "", 2, "isolated")
	node := testGPUNode("gpu-node-1", eightGPUs(), nil)

	r := newReconciler(guest, profile, node)

	// First reconcile: adds finalizer.
	_, err := reconcileGuest(r, "gpu-test", "default")
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}

	// Second reconcile: performs allocation.
	_, err = reconcileGuest(r, "gpu-test", "default")
	if err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}

	g, _ := getGuest(r, "gpu-test", "default")
	if !hasGPUAllocatedCondition(g, metav1.ConditionTrue) {
		t.Error("expected GPUAllocated=True")
	}
	if g.Status.GPU == nil {
		t.Fatal("status.gpu is nil")
	}
	if len(g.Status.GPU.Devices) != 2 {
		t.Errorf("allocated %d devices, want 2", len(g.Status.GPU.Devices))
	}
	if g.Status.GPU.Hypervisor != "cloud-hypervisor" {
		t.Errorf("hypervisor = %q, want cloud-hypervisor", g.Status.GPU.Hypervisor)
	}
	if g.Status.GPU.NodeName != "gpu-node-1" {
		t.Errorf("nodeName = %q, want gpu-node-1", g.Status.GPU.NodeName)
	}

	// Verify GPUs marked as allocated on node.
	n, _ := getGPUNode(r, "gpu-node-1")
	allocCount := 0
	for _, gpu := range n.Status.GPUs {
		if gpu.AllocatedTo == "default/gpu-test" {
			allocCount++
		}
	}
	if allocCount != 2 {
		t.Errorf("GPUs allocated on node = %d, want 2", allocCount)
	}
	if n.Status.FreeGPUs != 6 {
		t.Errorf("freeGPUs = %d, want 6", n.Status.FreeGPUs)
	}
}

func TestReconcile_NoCapacity(t *testing.T) {
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "big-profile"})
	profile := testGPUProfile("big-profile", "default", "pcie", "", 8, "isolated")
	// Node with only 2 free GPUs.
	gpus := []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA A100-PCIe", 0, false, ""),
		makeGPU(1, "0000:3d:00.0", "NVIDIA A100-PCIe", 0, false, ""),
	}
	node := testGPUNode("gpu-node-1", gpus, nil)

	r := newReconciler(guest, profile, node)

	// First reconcile: adds finalizer.
	reconcileGuest(r, "gpu-test", "default")
	// Second reconcile: allocation attempt.
	res, err := reconcileGuest(r, "gpu-test", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected requeue when no capacity")
	}

	g, _ := getGuest(r, "gpu-test", "default")
	if !hasGPUAllocatedCondition(g, metav1.ConditionFalse) {
		t.Error("expected GPUAllocated=False with NoCapacity")
	}
}

func TestReconcile_ProfileNotFound(t *testing.T) {
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "missing-profile"})
	r := newReconciler(guest)

	// First reconcile: adds finalizer.
	reconcileGuest(r, "gpu-test", "default")
	// Second reconcile: profile lookup fails.
	res, err := reconcileGuest(r, "gpu-test", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected requeue when profile not found")
	}

	g, _ := getGuest(r, "gpu-test", "default")
	if !hasGPUAllocatedCondition(g, metav1.ConditionFalse) {
		t.Error("expected GPUAllocated=False with ProfileNotFound")
	}
}

func TestReconcile_AlreadyAllocated(t *testing.T) {
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "pcie-profile"})
	// Pre-set GPUAllocated=True on the guest.
	guest.Status.Conditions = []metav1.Condition{
		{
			Type:               swiftv1alpha1.ConditionGPUAllocated,
			Status:             metav1.ConditionTrue,
			Reason:             "Allocated",
			Message:            "already done",
			LastTransitionTime: metav1.Now(),
		},
	}
	guest.Status.GPU = &swiftv1alpha1.GPUStatus{
		Devices:    []string{"0000:17:00.0"},
		Hypervisor: "cloud-hypervisor",
		NodeName:   "gpu-node-1",
	}
	profile := testGPUProfile("pcie-profile", "default", "pcie", "", 1, "isolated")
	r := newReconciler(guest, profile)

	// Should return immediately without error.
	res, err := reconcileGuest(r, "gpu-test", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when already allocated, got %v", res.RequeueAfter)
	}
}

func TestReconcile_Idempotent(t *testing.T) {
	// Simulate: GPUs already marked allocatedTo this guest on the node, but the
	// guest's status patch failed (no GPUAllocated condition).
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "pcie-profile"})
	guest.Finalizers = []string{GPUFinalizerName}
	profile := testGPUProfile("pcie-profile", "default", "pcie", "", 2, "isolated")

	gpus := eightGPUs()
	// Mark 2 GPUs as already allocated to this guest.
	gpus[0].Allocated = true
	gpus[0].AllocatedTo = "default/gpu-test"
	gpus[1].Allocated = true
	gpus[1].AllocatedTo = "default/gpu-test"
	node := testGPUNode("gpu-node-1", gpus, nil)

	r := newReconciler(guest, profile, node)

	_, err := reconcileGuest(r, "gpu-test", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	g, _ := getGuest(r, "gpu-test", "default")
	if !hasGPUAllocatedCondition(g, metav1.ConditionTrue) {
		t.Error("expected GPUAllocated=True from idempotent detection")
	}
	if g.Status.GPU == nil || len(g.Status.GPU.Devices) != 2 {
		t.Errorf("expected 2 devices from idempotent path, got %v", g.Status.GPU)
	}
}

func TestReconcile_DeallocateOnDelete(t *testing.T) {
	now := metav1.Now()
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "pcie-profile"})
	guest.DeletionTimestamp = &now
	guest.Finalizers = []string{GPUFinalizerName}
	guest.Status.GPU = &swiftv1alpha1.GPUStatus{
		Devices:    []string{"0000:17:00.0", "0000:3d:00.0"},
		Hypervisor: "cloud-hypervisor",
		NodeName:   "gpu-node-1",
	}

	gpus := eightGPUs()
	gpus[0].Allocated = true
	gpus[0].AllocatedTo = "default/gpu-test"
	gpus[1].Allocated = true
	gpus[1].AllocatedTo = "default/gpu-test"
	node := testGPUNode("gpu-node-1", gpus, nil)

	r := newReconciler(guest, node)

	_, err := reconcileGuest(r, "gpu-test", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify GPUs freed on node.
	n, _ := getGPUNode(r, "gpu-node-1")
	for _, gpu := range n.Status.GPUs {
		if gpu.AllocatedTo == "default/gpu-test" {
			t.Errorf("GPU %s still allocated to deleted guest", gpu.PCIAddress)
		}
	}
	if n.Status.FreeGPUs != 8 {
		t.Errorf("freeGPUs = %d, want 8 after deallocation", n.Status.FreeGPUs)
	}

	// Verify finalizer removed.
	g, _ := getGuest(r, "gpu-test", "default")
	for _, f := range g.Finalizers {
		if f == GPUFinalizerName {
			t.Error("finalizer should be removed after deallocation")
		}
	}
}

func TestReconcile_DeallocateNodeGone(t *testing.T) {
	now := metav1.Now()
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "pcie-profile"})
	guest.DeletionTimestamp = &now
	guest.Finalizers = []string{GPUFinalizerName}
	guest.Status.GPU = &swiftv1alpha1.GPUStatus{
		Devices:    []string{"0000:17:00.0"},
		Hypervisor: "cloud-hypervisor",
		NodeName:   "gone-node",
	}

	// No SwiftGPUNode object — the node is gone.
	r := newReconciler(guest)

	_, err := reconcileGuest(r, "gpu-test", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify finalizer removed gracefully despite missing node.
	g, _ := getGuest(r, "gpu-test", "default")
	for _, f := range g.Finalizers {
		if f == GPUFinalizerName {
			t.Error("finalizer should be removed even when node is gone")
		}
	}
}

func TestReconcile_FinalizerAdded(t *testing.T) {
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "pcie-profile"})
	profile := testGPUProfile("pcie-profile", "default", "pcie", "", 1, "isolated")
	node := testGPUNode("gpu-node-1", eightGPUs(), nil)

	r := newReconciler(guest, profile, node)

	// First reconcile should add the finalizer.
	_, err := reconcileGuest(r, "gpu-test", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	g, _ := getGuest(r, "gpu-test", "default")
	hasFinalizer := false
	for _, f := range g.Finalizers {
		if f == GPUFinalizerName {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Error("expected finalizer to be added on first reconcile")
	}
}

func TestReconcile_HypervisorSelection_PCIe(t *testing.T) {
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "pcie-profile"})
	profile := testGPUProfile("pcie-profile", "default", "pcie", "", 1, "isolated")
	node := testGPUNode("gpu-node-1", eightGPUs(), nil)

	r := newReconciler(guest, profile, node)

	reconcileGuest(r, "gpu-test", "default") // finalizer
	reconcileGuest(r, "gpu-test", "default") // allocate

	g, _ := getGuest(r, "gpu-test", "default")
	if g.Status.GPU == nil {
		t.Fatal("status.gpu is nil")
	}
	if g.Status.GPU.Hypervisor != "cloud-hypervisor" {
		t.Errorf("hypervisor = %q, want cloud-hypervisor for pcie tier", g.Status.GPU.Hypervisor)
	}
}

func TestReconcile_HypervisorSelection_HGX(t *testing.T) {
	guest := testSwiftGuest("gpu-test", "default", &corev1.LocalObjectReference{Name: "hgx-profile"})
	fm := &gpuv1alpha1.FabricManagerStatus{
		Installed: true,
		Version:   "580.95.05",
		Running:   true,
		Partitions: []gpuv1alpha1.FMPartitionStatus{
			{ID: 0, GPUIndices: []int{0, 1, 2, 3}, AllocatedTo: ""},
		},
	}
	profile := testGPUProfile("hgx-profile", "default", "hgx-shared", "", 4, "shared")
	node := testGPUNode("gpu-node-1", eightGPUs(), fm)

	r := newReconciler(guest, profile, node)

	reconcileGuest(r, "gpu-test", "default") // finalizer
	reconcileGuest(r, "gpu-test", "default") // allocate

	g, _ := getGuest(r, "gpu-test", "default")
	if g.Status.GPU == nil {
		t.Fatal("status.gpu is nil")
	}
	if g.Status.GPU.Hypervisor != "qemu" {
		t.Errorf("hypervisor = %q, want qemu for hgx-shared tier", g.Status.GPU.Hypervisor)
	}
	if g.Status.GPU.PartitionID != 0 {
		t.Errorf("partitionID = %d, want 0", g.Status.GPU.PartitionID)
	}
}

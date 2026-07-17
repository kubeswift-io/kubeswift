package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// oneGPUNode is a single-GPU (GTX 1080-class) Tier-1 SwiftGPUNode — the dev
// lab's boba shape.
func oneGPUNode(name string) *gpuv1alpha1.SwiftGPUNode {
	return &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			Phase: "Ready", GPUCount: 1, FreeGPUs: 1,
			GPUModel: "GTX 1080", GPUVendor: "NVIDIA", VfioReady: true,
			GPUs: []gpuv1alpha1.GPUDevice{
				{Index: 0, PCIAddress: "0000:01:00.0", Model: "GTX 1080", Vendor: "NVIDIA", NUMANode: 0, Driver: "vfio-pci"},
			},
		},
	}
}

func pcieProfile(name, ns string) *gpuv1alpha1.SwiftGPUProfile {
	return &gpuv1alpha1.SwiftGPUProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       gpuv1alpha1.SwiftGPUProfileSpec{Count: 1, Tier: "pcie", PartitionMode: "isolated"},
	}
}

func nativeGPUSandbox(name, ns, profileName string) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image:         "alpine:3",
			GPUProfileRef: &corev1.LocalObjectReference{Name: profileName},
		},
	}
}

func TestReconcileNativeGPU_AllocatesAndReleases(t *testing.T) {
	node := oneGPUNode("boba")
	profile := pcieProfile("gtx", "default")
	sb := nativeGPUSandbox("gpu-sb", "default", "gtx")

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(sb, node, profile).
		WithStatusSubresource(sb, node).
		Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}
	ctx := context.Background()

	ready, _, err := r.reconcileNativeGPU(ctx, sb)
	if err != nil {
		t.Fatalf("reconcileNativeGPU: %v", err)
	}
	if !ready {
		t.Fatal("expected ready=true after native allocation")
	}
	if sb.Status.GPU == nil || len(sb.Status.GPU.Devices) != 1 || sb.Status.GPU.Devices[0] != "0000:01:00.0" {
		t.Fatalf("status.gpu = %+v, want 1 device 0000:01:00.0", sb.Status.GPU)
	}
	if sb.Status.GPU.NodeName != "boba" || sb.Status.GPU.Hypervisor != "cloud-hypervisor" {
		t.Errorf("status.gpu node/hypervisor = %q/%q, want boba/cloud-hypervisor", sb.Status.GPU.NodeName, sb.Status.GPU.Hypervisor)
	}
	if !controllerutil.ContainsFinalizer(sb, sandboxGPUFinalizer) {
		t.Error("release finalizer must be added on native allocation")
	}
	// Node capacity consumed.
	var after gpuv1alpha1.SwiftGPUNode
	if err := c.Get(ctx, client.ObjectKey{Name: "boba"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.FreeGPUs != 0 || after.Status.GPUs[0].AllocatedTo != "sandbox:default/gpu-sb" {
		t.Errorf("node not marked allocated to the sandbox: free=%d allocatedTo=%q", after.Status.FreeGPUs, after.Status.GPUs[0].AllocatedTo)
	}

	// Release frees the node (idempotent).
	if err := r.releaseNativeGPU(ctx, sb); err != nil {
		t.Fatalf("releaseNativeGPU: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "boba"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.FreeGPUs != 1 || after.Status.GPUs[0].AllocatedTo != "" {
		t.Errorf("release did not free the GPU: free=%d allocatedTo=%q", after.Status.FreeGPUs, after.Status.GPUs[0].AllocatedTo)
	}
}

// A hgx-shared profile is rejected for a sandbox (mode-3 is CH-only), not
// silently allocated on a QEMU-less runtime.
func TestReconcileNativeGPU_HGXTierRejected(t *testing.T) {
	node := oneGPUNode("boba")
	profile := pcieProfile("hgx", "default")
	profile.Spec.Tier = "hgx-shared"
	profile.Spec.PartitionMode = "shared"
	sb := nativeGPUSandbox("gpu-sb", "default", "hgx")

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(sb, node, profile).WithStatusSubresource(sb, node).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}

	ready, _, err := r.reconcileNativeGPU(context.Background(), sb)
	if err != nil {
		t.Fatalf("reconcileNativeGPU: %v", err)
	}
	if ready {
		t.Fatal("hgx tier must NOT be ready for a sandbox")
	}
	if sb.Status.GPU != nil {
		t.Errorf("no GPU should be allocated for an unsupported tier, got %+v", sb.Status.GPU)
	}
	cond := findCond(sb, sandboxv1alpha1.SwiftSandboxConditionGPUAllocated)
	if cond == nil || cond.Reason != "UnsupportedTier" {
		t.Errorf("expected GPUAllocated=False reason=UnsupportedTier, got %+v", cond)
	}
}

func TestBuildIntent_NativeGPU_ExplicitDevices(t *testing.T) {
	sb := nativeGPUSandbox("gpu-sb", "default", "gtx")
	sb.Status.GPU = gpuStatus("boba", "0000:01:00.0")
	ri := buildIntent(sb, "gpu-sandbox", "/cache/x.ext4", "", execSpec{Argv: []string{"/bin/sh"}}, false)
	if ri.GPU == nil {
		t.Fatal("native sandbox must carry a GPU intent")
	}
	if ri.GPU.DeviceSource == "env" {
		t.Error("native intent must use explicit devices, not deviceSource=env (that's DRA)")
	}
	if len(ri.GPU.Devices) != 1 || ri.GPU.Devices[0].PCIAddress != "0000:01:00.0" {
		t.Fatalf("intent devices = %+v, want 1 explicit device 0000:01:00.0", ri.GPU.Devices)
	}
	if ri.GPU.Devices[0].HostPath != "/sys/bus/pci/devices/0000:01:00.0/" || ri.GPU.Firmware != "cloudhv" {
		t.Errorf("device hostPath/firmware = %q/%q", ri.GPU.Devices[0].HostPath, ri.GPU.Firmware)
	}
}

func TestBuildPod_NativeGPU_PinnedWithGpuInitEnv(t *testing.T) {
	sb := nativeGPUSandbox("gpu-sb", "default", "gtx")
	sb.Status.GPU = gpuStatus("boba", "0000:01:00.0")
	pod := buildPod(sb, "gpu-sandbox")

	if pod.Spec.NodeSelector["kubernetes.io/hostname"] != "boba" {
		t.Errorf("native GPU pod must pin to the allocated node; nodeSelector=%v", pod.Spec.NodeSelector)
	}
	if pod.Spec.NodeSelector[kernelNodeLabel] != "true" {
		t.Error("native GPU pod must still require the kernel-node label")
	}
	var gpuInit *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == gpuInitName {
			gpuInit = &pod.Spec.InitContainers[i]
		}
	}
	if gpuInit == nil {
		t.Fatal("native GPU pod must have a gpu-init container")
	}
	if v := envVal(gpuInit.Env, "GPU_PCI_ADDRESSES"); v != "0000:01:00.0" {
		t.Errorf("gpu-init GPU_PCI_ADDRESSES = %q, want 0000:01:00.0 (native passes devices via env)", v)
	}
	// Launcher must mount /dev/vfio.
	var launcher *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == launcherName {
			launcher = &pod.Spec.Containers[i]
		}
	}
	if !hasMount(launcher, "dev-vfio", "/dev/vfio") {
		t.Error("launcher must mount /dev/vfio for native GPU passthrough")
	}
	// No DRA ResourceClaim on the native path.
	if len(pod.Spec.ResourceClaims) != 0 {
		t.Errorf("native pod must not carry a DRA ResourceClaim, got %+v", pod.Spec.ResourceClaims)
	}
}

// --- small helpers ---

func gpuStatus(node string, bdfs ...string) *swiftv1alpha1.GPUStatus {
	return &swiftv1alpha1.GPUStatus{Devices: bdfs, NodeName: node, Hypervisor: "cloud-hypervisor", PartitionID: -1}
}

func findCond(sb *sandboxv1alpha1.SwiftSandbox, t string) *metav1.Condition {
	for i := range sb.Status.Conditions {
		if sb.Status.Conditions[i].Type == t {
			return &sb.Status.Conditions[i]
		}
	}
	return nil
}

func envVal(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

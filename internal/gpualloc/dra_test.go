package gpualloc

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

func draGuest() *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			GPUResourceClaim: &swiftv1alpha1.GPUResourceClaimSpec{
				ResourceClaimTemplateName: "gpu-tmpl",
				Tier:                      "pcie",
			},
		},
	}
}

// allocatedClaimPod builds the scheduled launcher pod + the template-minted,
// allocated ResourceClaim that the NVIDIA DRA driver would produce — with a
// known PCI BDF in AllocatedDeviceStatus.Data.
func allocatedClaimPod(bdf string) (*corev1.Pod, *resourcev1.ResourceClaim) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "g", Namespace: "default",
			Labels: map[string]string{guestPodLabel: "g"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node1",
			ResourceClaims: []corev1.PodResourceClaim{
				{Name: "gpu", ResourceClaimTemplateName: ptr.To("gpu-tmpl")},
			},
		},
		Status: corev1.PodStatus{
			ResourceClaimStatuses: []corev1.PodResourceClaimStatus{
				{Name: "gpu", ResourceClaimName: ptr.To("g-claim-abc")},
			},
		},
	}
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "g-claim-abc", Namespace: "default"},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{
						{Request: "gpu", Driver: "gpu.nvidia.com", Pool: "node1", Device: "gpu-0"},
					},
				},
			},
			Devices: []resourcev1.AllocatedDeviceStatus{
				{
					Driver: "gpu.nvidia.com", Pool: "node1", Device: "gpu-0",
					Data: &runtime.RawExtension{Raw: []byte(`{"pciAddress":"` + bdf + `"}`)},
				},
			},
		},
	}
	return pod, claim
}

// TestDRABackend_Resolve_ReadsBDFBack is the headline no-hardware test: it
// proves the DRA backend maps a driver's ResourceClaim allocation result to the
// GPUStatus shape Layer 2 consumes — without any GPU or DRA driver. When the
// real driver Data schema is pinned (P2), only extractDeviceBDFs changes.
func TestDRABackend_Resolve_ReadsBDFBack(t *testing.T) {
	guest := draGuest()
	pod, claim := allocatedClaimPod("0000:41:00.0")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(guest, pod, claim).Build()
	b := NewDRABackend(c)

	res, err := b.Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Ready {
		t.Fatal("expected Ready=true once the claim is allocated")
	}
	if got := res.Status.Devices; len(got) != 1 || got[0] != "0000:41:00.0" {
		t.Errorf("Devices = %v, want [0000:41:00.0]", got)
	}
	if res.Status.NodeName != "node1" {
		t.Errorf("NodeName = %q, want node1 (the scheduled node)", res.Status.NodeName)
	}
	if res.Status.Hypervisor != "cloud-hypervisor" {
		t.Errorf("Hypervisor = %q, want cloud-hypervisor (tier=pcie)", res.Status.Hypervisor)
	}
	if res.Status.PartitionID != -1 {
		t.Errorf("PartitionID = %d, want -1 (FM partitions are native-only)", res.Status.PartitionID)
	}
}

// TestDRABackend_Resolve_NotReady covers the deferred states: pod unscheduled
// and claim not yet allocated both requeue rather than erroring.
func TestDRABackend_Resolve_NotReady(t *testing.T) {
	guest := draGuest()

	// (a) Pod scheduled but claim not allocated yet.
	pod, claim := allocatedClaimPod("0000:41:00.0")
	claim.Status.Allocation = nil
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(guest, pod, claim).Build()
	res, err := NewDRABackend(c).Resolve(context.Background(), guest)
	if err != nil || res.Ready {
		t.Errorf("unallocated claim: want {Ready:false, nil err}, got ready=%v err=%v", res.Ready, err)
	}

	// (b) Pod not scheduled yet (NodeName empty).
	pod2, claim2 := allocatedClaimPod("0000:41:00.0")
	pod2.Spec.NodeName = ""
	c2 := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(guest, pod2, claim2).Build()
	res2, err := NewDRABackend(c2).Resolve(context.Background(), guest)
	if err != nil || res2.Ready {
		t.Errorf("unscheduled pod: want {Ready:false, nil err}, got ready=%v err=%v", res2.Ready, err)
	}

	// (c) No launcher pod at all (DRA pod not built — P1 runtime gap).
	c3 := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(guest).Build()
	res3, err := NewDRABackend(c3).Resolve(context.Background(), guest)
	if err != nil || res3.Ready {
		t.Errorf("no pod: want {Ready:false, nil err}, got ready=%v err=%v", res3.Ready, err)
	}
}

// TestDeviceNameEncoding covers the §A3 tier-2 contract: the reference
// driver's device names encode the BDF, and DecodeDeviceName inverts
// EncodeDeviceName exactly.
func TestDeviceNameEncoding(t *testing.T) {
	for _, bdf := range []string{"0000:01:00.0", "0000:41:00.0", "0001:af:1f.7"} {
		name := EncodeDeviceName(bdf)
		got, ok := DecodeDeviceName(name)
		if !ok || got != bdf {
			t.Errorf("roundtrip %q -> %q -> (%q, %v)", bdf, name, got, ok)
		}
	}
	for _, bad := range []string{"gpu-0000-01-00", "nic-0000-01-00-0", "gpu---", "gpu-a-b-c-d-e"} {
		if _, ok := DecodeDeviceName(bad); ok {
			t.Errorf("DecodeDeviceName(%q) must not match the scheme", bad)
		}
	}
}

// TestDRABackend_Resolve_NameFallback proves the GA-API-only path: when the
// device-status feature is unavailable (no AllocatedDeviceStatus.Data), the
// BDF is decoded from the allocation result's device name.
func TestDRABackend_Resolve_NameFallback(t *testing.T) {
	guest := draGuest()
	pod, claim := allocatedClaimPod("ignored")
	claim.Status.Devices = nil // no device-status feature
	claim.Status.Allocation.Devices.Results[0].Device = EncodeDeviceName("0000:01:00.0")

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(guest, pod, claim).Build()
	res, err := NewDRABackend(c).Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Ready {
		t.Fatal("expected Ready via the name-encoding fallback")
	}
	if got := res.Status.Devices; len(got) != 1 || got[0] != "0000:01:00.0" {
		t.Errorf("Devices = %v, want [0000:01:00.0] (decoded from the device name)", got)
	}
}

// TestDRABackend_Prepare returns a PodBinding (no allocation decided yet).
func TestDRABackend_Prepare(t *testing.T) {
	guest := draGuest()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(guest).Build()
	pr, err := NewDRABackend(c).Prepare(context.Background(), guest)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if pr.Resolved {
		t.Error("DRA Prepare must defer (Resolved=false); the scheduler decides")
	}
	if pr.PodBinding == nil || pr.PodBinding.ResourceClaimTemplateName != "gpu-tmpl" {
		t.Errorf("PodBinding = %+v, want template gpu-tmpl", pr.PodBinding)
	}
	if pr.PodBinding.RequestName != "gpu" {
		t.Errorf("RequestName = %q, want default %q", pr.PodBinding.RequestName, defaultRequestName)
	}
}

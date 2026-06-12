package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func draGPUGuest() *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "dra-test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
			GPUResourceClaim: &swiftv1alpha1.GPUResourceClaimSpec{
				ResourceClaimTemplateName: "gpu-tmpl",
				Tier:                      "pcie",
				Hugepages:                 "1Gi",
			},
		},
		// No Status.GPU — DRA pods are built BEFORE allocation.
	}
}

// TestBuildGPUDiskBootPod_DRA proves the DRA pod shape (design doc §A2/§A6):
// claim-bearing, unpinned, no controller-set GPU envs (CDI injects them), and
// resources.claims on the containers that must receive the CDI edits.
func TestBuildGPUDiskBootPod_DRA(t *testing.T) {
	guest := draGPUGuest()
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", guest.Spec.GPUResourceClaim.Hugepages, nil)

	// UNPINNED: the scheduler + DRA driver pick the node.
	if pod.Spec.NodeSelector != nil {
		t.Errorf("DRA pod must be unpinned; NodeSelector = %v", pod.Spec.NodeSelector)
	}

	// Claim-bearing: pod.spec.resourceClaims references the template.
	if len(pod.Spec.ResourceClaims) != 1 {
		t.Fatalf("ResourceClaims = %v, want exactly one", pod.Spec.ResourceClaims)
	}
	prc := pod.Spec.ResourceClaims[0]
	if prc.Name != "gpu" {
		t.Errorf("claim pod-local name = %q, want gpu", prc.Name)
	}
	if prc.ResourceClaimTemplateName == nil || *prc.ResourceClaimTemplateName != "gpu-tmpl" {
		t.Errorf("ResourceClaimTemplateName = %v, want gpu-tmpl", prc.ResourceClaimTemplateName)
	}
	if prc.ResourceClaimName != nil {
		t.Errorf("ResourceClaimName must be unset when a template is used; got %v", *prc.ResourceClaimName)
	}

	// gpu-init: no controller-set GPU envs (the CDI containerEdits inject
	// GPU_PCI_ADDRESSES/GPU_PARTITION_ID — empty values here would shadow
	// them), and it references the claim.
	var gpuInit *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "gpu-init" {
			gpuInit = &pod.Spec.InitContainers[i]
		}
	}
	if gpuInit == nil {
		t.Fatal("gpu-init init container missing")
	}
	for _, e := range gpuInit.Env {
		if e.Name == "GPU_PCI_ADDRESSES" || e.Name == "GPU_PARTITION_ID" {
			t.Errorf("gpu-init must not set %s in DRA mode (CDI injects it); got value %q", e.Name, e.Value)
		}
	}
	if len(gpuInit.Resources.Claims) != 1 || gpuInit.Resources.Claims[0].Name != "gpu" {
		t.Errorf("gpu-init resources.claims = %v, want [{gpu}]", gpuInit.Resources.Claims)
	}

	// launcher references the claim too (swiftletd reads the same CDI env).
	var launcher *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "launcher" {
			launcher = &pod.Spec.Containers[i]
		}
	}
	if launcher == nil {
		t.Fatal("launcher container missing")
	}
	if len(launcher.Resources.Claims) != 1 || launcher.Resources.Claims[0].Name != "gpu" {
		t.Errorf("launcher resources.claims = %v, want [{gpu}]", launcher.Resources.Claims)
	}
}

// TestBuildGPUDiskBootPod_DRA_SharedClaim covers the pre-created shared-claim
// reference form.
func TestBuildGPUDiskBootPod_DRA_SharedClaim(t *testing.T) {
	guest := draGPUGuest()
	guest.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimName: "shared-gpu"}

	pod := BuildGPUDiskBootPod(guest, gpuResolvedGuest(), "s", "i", "", nil)
	prc := pod.Spec.ResourceClaims[0]
	if prc.ResourceClaimName == nil || *prc.ResourceClaimName != "shared-gpu" {
		t.Errorf("ResourceClaimName = %v, want shared-gpu", prc.ResourceClaimName)
	}
	if prc.ResourceClaimTemplateName != nil {
		t.Errorf("template must be unset for a shared claim; got %v", *prc.ResourceClaimTemplateName)
	}
}

// TestBuildGPUDiskBootPod_NativeUnchanged is the regression guard: a NATIVE
// GPU guest's pod must be completely untouched by the DRA additions.
func TestBuildGPUDiskBootPod_NativeUnchanged(t *testing.T) {
	guest := gpuGuest("gpu-node-42", []string{"0000:17:00.0"}, -1)
	pod := BuildGPUDiskBootPod(guest, gpuResolvedGuest(), "test-seed", "test-intent", "1Gi", nil)

	if pod.Spec.ResourceClaims != nil {
		t.Errorf("native pod must carry no ResourceClaims; got %v", pod.Spec.ResourceClaims)
	}
	if pod.Spec.NodeSelector["kubernetes.io/hostname"] != "gpu-node-42" {
		t.Errorf("native pod must stay pinned; NodeSelector = %v", pod.Spec.NodeSelector)
	}
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.Name != "gpu-init" {
			continue
		}
		found := false
		for _, e := range c.Env {
			if e.Name == "GPU_PCI_ADDRESSES" && e.Value == "0000:17:00.0" {
				found = true
			}
		}
		if !found {
			t.Error("native gpu-init must keep GPU_PCI_ADDRESSES from status.GPU")
		}
		if len(c.Resources.Claims) != 0 {
			t.Errorf("native gpu-init must carry no resources.claims; got %v", c.Resources.Claims)
		}
	}
}

// TestBuildDRAGPUIntent covers the deviceSource=env intent shape.
func TestBuildDRAGPUIntent(t *testing.T) {
	intent := buildDRAGPUIntent(&swiftv1alpha1.GPUResourceClaimSpec{Tier: "pcie", Hugepages: "1Gi"})
	if intent.DeviceSource != "env" {
		t.Errorf("DeviceSource = %q, want env", intent.DeviceSource)
	}
	if len(intent.Devices) != 0 {
		t.Errorf("Devices must be empty pre-allocation; got %v", intent.Devices)
	}
	if intent.Firmware != "cloudhv" {
		t.Errorf("Firmware = %q, want cloudhv (tier pcie)", intent.Firmware)
	}
	if intent.Hugepages != "1G" {
		t.Errorf("Hugepages = %q, want 1G", intent.Hugepages)
	}
	if intent.FabricManagerPartitionID != -1 {
		t.Errorf("FabricManagerPartitionID = %d, want -1", intent.FabricManagerPartitionID)
	}

	hgx := buildDRAGPUIntent(&swiftv1alpha1.GPUResourceClaimSpec{Tier: "hgx-shared"})
	if hgx.Firmware != "ovmf" {
		t.Errorf("hgx Firmware = %q, want ovmf", hgx.Firmware)
	}
}

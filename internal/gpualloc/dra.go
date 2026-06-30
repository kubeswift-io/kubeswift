package gpualloc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// guestPodLabel is the label every launcher pod carries (the canonical guest
// join key, stable across live-migration pod renames). The DRA backend uses it
// to find the scheduled launcher pod and read its ResourceClaim back.
const guestPodLabel = "swift.kubeswift.io/guest"

// defaultRequestName is the device-request name assumed within a ResourceClaim
// when the guest does not specify one.
const defaultRequestName = "gpu"

// DRABackend implements Backend for Dynamic Resource Allocation. It does NOT
// decide allocation — the scheduler + a DRA driver do, at pod-schedule time.
// Prepare only describes the claim binding; Resolve reads the allocation result
// back once the launcher pod is scheduled.
type DRABackend struct {
	client client.Client
}

// NewDRABackend constructs the DRA allocation backend.
func NewDRABackend(c client.Client) *DRABackend {
	return &DRABackend{client: c}
}

// Name implements Backend.
func (b *DRABackend) Name() string { return swiftv1alpha1.GPUBackendDRA }

// Prepare implements Backend: it does not allocate (the scheduler does). It
// returns the PodBinding the launcher pod must carry, with no node/devices.
func (b *DRABackend) Prepare(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*PrepareResult, error) {
	rc := guest.Spec.GPUResourceClaim
	if rc == nil {
		return nil, fmt.Errorf("DRA backend Prepare called on a guest without spec.gpuResourceClaim")
	}
	req := rc.RequestName
	if req == "" {
		req = defaultRequestName
	}
	return &PrepareResult{
		Resolved: false,
		PodBinding: &PodBinding{
			ResourceClaimName:         rc.ResourceClaimName,
			ResourceClaimTemplateName: rc.ResourceClaimTemplateName,
			RequestName:               req,
		},
	}, nil
}

// Resolve implements Backend: read the scheduled launcher pod's ResourceClaim
// allocation result and map it to a GPUStatus. Returns {Ready:false} (requeue)
// until the pod is scheduled and the claim is allocated.
func (b *DRABackend) Resolve(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*Resolution, error) {
	rc := guest.Spec.GPUResourceClaim
	if rc == nil {
		return nil, fmt.Errorf("DRA backend Resolve called on a guest without spec.gpuResourceClaim")
	}

	pod, err := b.findScheduledLauncherPod(ctx, guest)
	if err != nil {
		return nil, err
	}
	if pod == nil || pod.Spec.NodeName == "" {
		// Pod not created or not scheduled yet — the scheduler/DRA has not
		// decided. Requeue.
		return &Resolution{Ready: false}, nil
	}

	claim, err := b.resolveClaim(ctx, guest, pod)
	if err != nil {
		return nil, err
	}
	if claim == nil || claim.Status.Allocation == nil {
		// Scheduled but the claim is not allocated yet (binding in progress).
		return &Resolution{Ready: false}, nil
	}

	bdfs, err := extractDeviceBDFs(claim)
	if err != nil {
		return nil, fmt.Errorf("read allocated device(s) from ResourceClaim %q: %w", claim.Name, err)
	}
	if len(bdfs) == 0 {
		return nil, fmt.Errorf("ResourceClaim %q is allocated but exposes no PCI device address", claim.Name)
	}

	return &Resolution{
		Ready: true,
		Status: &swiftv1alpha1.GPUStatus{
			Devices:     bdfs,
			PartitionID: -1, // FM partitions are a native-only concept in P1.
			NodeName:    pod.Spec.NodeName,
			Hypervisor:  hypervisorForTier(rc.Tier),
		},
	}, nil
}

// Release implements Backend. The minted ResourceClaim (template case) is owned
// by the launcher pod and GC'd with it; a shared claim is operator-owned. So
// Release is a no-op in P1.
func (b *DRABackend) Release(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) error {
	return nil
}

// findScheduledLauncherPod returns the guest's GPU launcher pod (the one
// carrying a ResourceClaim), or nil if not found.
func (b *DRABackend) findScheduledLauncherPod(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := b.client.List(ctx, &pods,
		client.InNamespace(guest.Namespace),
		client.MatchingLabels{guestPodLabel: guest.Name}); err != nil {
		return nil, fmt.Errorf("list launcher pods for guest %q: %w", guest.Name, err)
	}
	for i := range pods.Items {
		if len(pods.Items[i].Spec.ResourceClaims) > 0 {
			return &pods.Items[i], nil
		}
	}
	return nil, nil
}

// resolveClaim returns the ResourceClaim backing the pod's GPU claim — either
// the template-minted claim (named in pod.status.resourceClaimStatuses) or the
// shared claim named directly in the spec.
func (b *DRABackend) resolveClaim(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, pod *corev1.Pod) (*resourcev1.ResourceClaim, error) {
	claimName := ""
	// Template-minted claims are reported back in pod.status.resourceClaimStatuses.
	for _, rcs := range pod.Status.ResourceClaimStatuses {
		if rcs.ResourceClaimName != nil && *rcs.ResourceClaimName != "" {
			claimName = *rcs.ResourceClaimName
			break
		}
	}
	// Shared claim: named directly in the spec.
	if claimName == "" {
		for _, prc := range pod.Spec.ResourceClaims {
			if prc.ResourceClaimName != nil && *prc.ResourceClaimName != "" {
				claimName = *prc.ResourceClaimName
				break
			}
		}
	}
	if claimName == "" {
		return nil, nil
	}
	var claim resourcev1.ResourceClaim
	if err := b.client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: claimName}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get ResourceClaim %q: %w", claimName, err)
	}
	return &claim, nil
}

// draDeviceData is the per-device payload KubeSwift expects in
// AllocatedDeviceStatus.Data for a VFIO-passthrough device. The DRA driver owns
// this RawExtension schema; the exact shape the NVIDIA k8s-dra-driver-gpu emits
// in VFIO/IOMMUFD mode is pinned during the P2 hardware spike. This struct is
// the single point that changes when the real schema lands — keep the parsing
// isolated here.
type draDeviceData struct {
	PCIAddress string `json:"pciAddress,omitempty"`
	PCIBusID   string `json:"pciBusID,omitempty"` // alternate key some drivers use
}

// extractDeviceBDFs pulls the PCI BDF(s) of the allocated VFIO device(s) out of
// a ResourceClaim's allocation status. ISOLATED on purpose (risk #2 in the
// design doc): the driver-specific AllocatedDeviceStatus.Data schema is the one
// thing the skeleton must assume; only this function changes when an external
// driver's schema is pinned.
//
// Two-tier contract (design doc §A3):
//  1. Preferred: AllocatedDeviceStatus.Data {"pciAddress": "<bdf>"} — written
//     by the KubeSwift reference driver (and what an external driver's adapter
//     would populate). Requires the DRAResourceClaimDeviceStatus feature.
//  2. Fallback (GA API only): the allocation result's device NAME encodes the
//     BDF per the reference driver's naming scheme (gpu-0000-01-00-0 ⇄
//     0000:01:00.0) — decoded from Status.Allocation.Devices.Results.
func extractDeviceBDFs(claim *resourcev1.ResourceClaim) ([]string, error) {
	var bdfs []string
	for i := range claim.Status.Devices {
		dev := &claim.Status.Devices[i]
		if dev.Data == nil || len(dev.Data.Raw) == 0 {
			continue
		}
		var d draDeviceData
		if err := json.Unmarshal(dev.Data.Raw, &d); err != nil {
			return nil, fmt.Errorf("device %q: unparseable Data: %w", dev.Device, err)
		}
		switch {
		case d.PCIAddress != "":
			bdfs = append(bdfs, d.PCIAddress)
		case d.PCIBusID != "":
			bdfs = append(bdfs, d.PCIBusID)
		}
	}
	if len(bdfs) > 0 {
		return bdfs, nil
	}
	// Tier 2: decode the reference driver's name encoding from the GA
	// allocation result (works without the device-status feature).
	if claim.Status.Allocation != nil {
		for _, res := range claim.Status.Allocation.Devices.Results {
			if bdf, ok := DecodeDeviceName(res.Device); ok {
				bdfs = append(bdfs, bdf)
			}
		}
	}
	return bdfs, nil
}

// EncodeDeviceName converts a PCI BDF to the DNS-label-safe ResourceSlice
// device name the KubeSwift reference DRA driver publishes:
// "0000:01:00.0" -> "gpu-0000-01-00-0". Part of the reference-driver contract
// (design doc §A3) — DecodeDeviceName must invert it exactly.
func EncodeDeviceName(bdf string) string {
	s := strings.NewReplacer(":", "-", ".", "-").Replace(bdf)
	return "gpu-" + s
}

// DecodeDeviceName inverts EncodeDeviceName: "gpu-0000-01-00-0" ->
// ("0000:01:00.0", true). Returns ok=false for names not using the scheme.
func DecodeDeviceName(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "gpu-")
	if !ok {
		return "", false
	}
	parts := strings.Split(rest, "-")
	if len(parts) != 4 {
		return "", false
	}
	for _, p := range parts {
		if p == "" {
			return "", false
		}
	}
	return fmt.Sprintf("%s:%s:%s.%s", parts[0], parts[1], parts[2], parts[3]), true
}

// hypervisorForTier mirrors the native tier->hypervisor mapping: pcie ->
// cloud-hypervisor, hgx-* -> qemu. Empty/unknown defaults to cloud-hypervisor.
func hypervisorForTier(tier string) string {
	if tier == "hgx-shared" || tier == "hgx-full" {
		return "qemu"
	}
	return "cloud-hypervisor"
}

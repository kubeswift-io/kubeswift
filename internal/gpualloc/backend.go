// Package gpualloc defines the pluggable GPU allocation backend seam for
// KubeSwift. KubeSwift's GPU subsystem is two separable layers coupled by
// exactly one struct — SwiftGuest.Status.GPU:
//
//  1. Allocation + discovery: which GPUs, on which node. This is what the
//     backend owns.
//  2. VM-passthrough translation + runtime: given allocated PCI BDFs, bind them
//     to vfio-pci and pass them into the VM (buildGPUIntent, gpu-init, CH/QEMU).
//     This is independent of HOW allocation is decided.
//
// Anything that populates SwiftGuest.Status.GPU correctly is a valid backend.
// Today there are two:
//
//   - "native": the SwiftGPU model (SwiftGPUProfile/SwiftGPUNode + findAndAllocate).
//     Decides node+devices in the CONTROLLER, before the launcher pod exists.
//   - "dra": Dynamic Resource Allocation. The launcher pod carries a
//     ResourceClaim and the SCHEDULER + a DRA driver allocate the device at
//     pod-schedule time; the controller reads the result back.
//
// The control-flow differs fundamentally (controller-time vs scheduler-time), so
// the Backend interface is TWO-PHASE: Prepare (before the pod) then Resolve
// (after the pod is scheduled). The native backend resolves entirely in Prepare;
// the DRA backend defers to Resolve. See docs/design/dra-gpu-integration.md.
package gpualloc

import (
	"context"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// Backend is a pluggable GPU allocation strategy. Implementations:
// nativeBackend (internal/controller/swiftgpu) and DRABackend (this package).
type Backend interface {
	// Name is "native" or "dra" (swiftv1alpha1.GPUBackend* values) — used in
	// conditions, log fields and metric labels.
	Name() string

	// Prepare runs in the SwiftGPU controller BEFORE the launcher pod is built.
	//
	//   native: findAndAllocate — pick node+GPUs, mark them on the SwiftGPUNode,
	//           and return PrepareResult{Resolved: true, Status: <GPUStatus>}. The
	//           launcher pod is then pinned to Status.NodeName.
	//   dra:    ensure the ResourceClaim referenced by the guest is consumable and
	//           return PrepareResult{Resolved: false, PodBinding: <claim ref>} with
	//           NO node and NO devices — the scheduler decides later.
	//
	// Idempotent.
	Prepare(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*PrepareResult, error)

	// Resolve produces the final GPUStatus once device identity is known. Only
	// meaningful for backends that returned Resolved=false from Prepare.
	//
	//   native: no-op — Prepare already produced the Status.
	//   dra:    read the scheduled launcher pod's ResourceClaim allocation result
	//           + the driver's AllocatedDeviceStatus, map driver/pool/device to a
	//           PCI BDF + the scheduled node, and return a GPUStatus. Returns
	//           Resolution{Ready: false} (requeue) until the claim is allocated.
	//
	// Idempotent; safe to call every reconcile.
	Resolve(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*Resolution, error)

	// Release reverses Prepare on the finalizer/deletion path.
	//   native: deallocateGPUs — free the guest's GPUs on every SwiftGPUNode.
	//   dra:    the owned ResourceClaim is GC'd via ownerRef; Release is a no-op
	//           (or best-effort delete).
	// Idempotent (safe to call when nothing was allocated).
	Release(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) error
}

// PrepareResult is what Prepare returns.
type PrepareResult struct {
	// Resolved is true when Prepare already fully decided the allocation (native).
	// When false (dra), the controller must defer to Resolve after the pod is
	// scheduled.
	Resolved bool
	// Status is the GPUStatus to stamp now; non-nil iff Resolved (native).
	Status *swiftv1alpha1.GPUStatus
	// PodBinding describes how the launcher pod must reference the ResourceClaim;
	// non-nil iff !Resolved (dra). The pod builder reconstructs the same from the
	// guest spec, so this is informational.
	PodBinding *PodBinding
}

// PodBinding tells the pod builder how to wire pod.spec.resourceClaims (DRA).
type PodBinding struct {
	// ResourceClaimName references a shared, pre-created ResourceClaim.
	ResourceClaimName string
	// ResourceClaimTemplateName references a ResourceClaimTemplate (per-pod claim).
	ResourceClaimTemplateName string
	// RequestName is the device-request name within the claim used to match the
	// allocation result back at Resolve time.
	RequestName string
}

// Resolution is what Resolve returns.
type Resolution struct {
	// Ready is true once the device identity is known; false means requeue.
	Ready bool
	// Status is the GPUStatus to stamp; populated iff Ready.
	Status *swiftv1alpha1.GPUStatus
}

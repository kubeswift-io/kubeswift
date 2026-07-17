package swiftsandbox

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftgpu"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// poolGPUFinalizer releases a warm GPU pool's per-slot SwiftGPU allocations
// before the pool is deleted. The slot pods cascade-GC with the pool, but a
// slot's GPU allocation is a status field on a separate SwiftGPUNode — it is not
// owner-ref'd and must be released explicitly.
const poolGPUFinalizer = "kubeswift.io/pool-gpu-allocation"

// slotGPUAllocatedTo is the SwiftGPU allocation identity for a warm slot. It
// reuses the sandbox "sandbox:<ns>/<name>" kind so a claimed slot (whose pod
// keeps the <pool>-slot-<x> name and its GPU) and the GC sweep agree, and so it
// never collides with a SwiftGuest's "<ns>/<name>".
func slotGPUAllocatedTo(namespace, slotName string) string {
	return "sandbox:" + namespace + "/" + slotName
}

func slotGPUPrefix(pool *sandboxv1alpha1.SwiftSandboxPool) string {
	return "sandbox:" + pool.Namespace + "/" + pool.Name + "-slot-"
}

// allocateSlotGPU allocates a native GPU for one warm slot and stamps its
// status.GPU + spec.gpuProfileRef so buildIntent/buildPod produce a GPU-aware
// slot (node pin, gpu-init, explicit device intent — all B1 machinery). Called
// from createWarmSlot BEFORE building the pod. Returns swiftgpu's errNoCapacity
// (wrapped) when no GPU is free — the caller stops warming rather than failing.
func (r *SwiftSandboxPoolReconciler) allocateSlotGPU(ctx context.Context, pool *sandboxv1alpha1.SwiftSandboxPool, slot *sandboxv1alpha1.SwiftSandbox) error {
	var profile gpuv1alpha1.SwiftGPUProfile
	if err := r.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: pool.Spec.GPUProfileRef.Name}, &profile); err != nil {
		return fmt.Errorf("load SwiftGPUProfile %q for pool: %w", pool.Spec.GPUProfileRef.Name, err)
	}
	// A warm slot boots mode-3 (CH direct-kernel) like any sandbox — HGX tiers
	// need the QEMU disk-boot path and cannot be pooled here.
	if profile.Spec.Tier == "hgx-shared" || profile.Spec.Tier == "hgx-full" {
		return fmt.Errorf("warm GPU pools support only tier: pcie (mode-3); profile %q is tier %q", profile.Name, profile.Spec.Tier)
	}

	node, gpus, numa, partID, err := swiftgpu.FindAndAllocateFor(ctx, r.Client, slotGPUAllocatedTo(pool.Namespace, slot.Name), "", &profile)
	if err != nil {
		return err
	}
	devices := make([]string, len(gpus))
	for i, g := range gpus {
		devices[i] = g.PCIAddress
	}
	slot.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: pool.Spec.GPUProfileRef.Name}
	slot.Status.GPU = &swiftv1alpha1.GPUStatus{
		Devices:     devices,
		PartitionID: partID,
		NUMANodes:   numa,
		Hypervisor:  "cloud-hypervisor",
		NodeName:    node.Name,
	}
	return nil
}

// reconcileSlotGPUGC releases the GPU of any of this pool's slots whose pod no
// longer exists — draining (scale-down), checkout completion (the claiming
// SwiftSandbox was deleted → its slot pod GC'd), or churn. liveSlotPods holds
// the names of every pool pod that currently EXISTS (any phase, incl.
// terminating — a terminating pod's CH may still hold the VFIO group, so its
// allocation is kept until the pod is truly gone; mirrors the B1 reuse race).
// Bounded: a handful of GPU nodes. A no-op for non-GPU pools.
func (r *SwiftSandboxPoolReconciler) reconcileSlotGPUGC(ctx context.Context, pool *sandboxv1alpha1.SwiftSandboxPool, liveSlotPods map[string]bool) error {
	if pool.Spec.GPUProfileRef == nil {
		return nil
	}
	prefix := slotGPUPrefix(pool)
	idPrefix := "sandbox:" + pool.Namespace + "/"

	var nodes gpuv1alpha1.SwiftGPUNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return err
	}
	orphans := map[string]bool{}
	for i := range nodes.Items {
		for _, g := range nodes.Items[i].Status.GPUs {
			if !strings.HasPrefix(g.AllocatedTo, prefix) {
				continue
			}
			slotName := strings.TrimPrefix(g.AllocatedTo, idPrefix)
			if !liveSlotPods[slotName] {
				orphans[g.AllocatedTo] = true
			}
		}
	}
	for allocatedTo := range orphans {
		if err := swiftgpu.DeallocateForWorkload(ctx, r.Client, allocatedTo); err != nil {
			return err
		}
	}
	return nil
}

package swiftsandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilrand "k8s.io/apimachinery/pkg/util/rand"

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

// slotSuffixLen is the length of the random suffix in a slot name
// (<pool>-slot-<suffix>). newSlotName generates it and isPoolSlotName matches
// it, so the GPU GC recognises exactly this pool's slots.
const slotSuffixLen = 5

// poolGPUReleaseRecheck paces a terminating GPU pool's wait for its slot pods
// to go away (a claimed slot's checkout may still be running).
const poolGPUReleaseRecheck = 15 * time.Second

// newSlotName returns a fresh name for one of pool's warm slots.
func newSlotName(pool *sandboxv1alpha1.SwiftSandboxPool) string {
	return pool.Name + "-slot-" + utilrand.String(slotSuffixLen)
}

// isPoolSlotName reports whether name has the exact shape of one of pool's slot
// names. A bare "<pool>-slot-" prefix match also caught a standalone sandbox
// named "<pool>-slot-x" and every slot of a pool named "<pool>-slot-y" — and
// the GC then freed THEIR running GPUs, which were handed out again.
func isPoolSlotName(pool *sandboxv1alpha1.SwiftSandboxPool, name string) bool {
	suffix, ok := strings.CutPrefix(name, pool.Name+"-slot-")
	if !ok || len(suffix) != slotSuffixLen {
		return false
	}
	for _, c := range suffix {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// podReader is the uncached reader when wired (production), else the client.
func (r *SwiftSandboxPoolReconciler) podReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// reconcileSlotGPUGC releases the GPU of any of this pool's slots whose pod no
// longer exists — draining (scale-down), checkout completion (the claiming
// SwiftSandbox was deleted → its slot pod GC'd), or churn. A pod that still
// EXISTS (any phase, incl. terminating, warm or claimed) keeps its allocation:
// its CH may still hold the VFIO group. held reports whether any of this pool's
// allocations is still backed by such a pod. A no-op for non-GPU pools.
//
// The live-pod set is read UNCACHED: a slot created moments ago may not be in
// the informer cache yet, and freeing its GPU then hands the device out twice.
func (r *SwiftSandboxPoolReconciler) reconcileSlotGPUGC(ctx context.Context, pool *sandboxv1alpha1.SwiftSandboxPool) (held bool, err error) {
	if pool.Spec.GPUProfileRef == nil {
		return false, nil
	}
	idPrefix := "sandbox:" + pool.Namespace + "/"

	var pods corev1.PodList
	if err := r.podReader().List(ctx, &pods, client.InNamespace(pool.Namespace), client.MatchingLabels{PoolLabelKey: pool.Name}); err != nil {
		return false, err
	}
	live := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		live[pods.Items[i].Name] = true
	}

	var nodes gpuv1alpha1.SwiftGPUNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return false, err
	}
	orphans := map[string]string{} // allocatedTo -> slot name
	for i := range nodes.Items {
		for _, g := range nodes.Items[i].Status.GPUs {
			name, ok := strings.CutPrefix(g.AllocatedTo, idPrefix)
			if !ok || !isPoolSlotName(pool, name) {
				continue
			}
			if live[name] {
				held = true
				continue
			}
			orphans[g.AllocatedTo] = name
		}
	}
	for allocatedTo, name := range orphans {
		// A standalone SwiftSandbox whose name happens to have a slot's shape
		// owns this allocation, and its own controller releases it (after its
		// launcher is gone). Never free it from here.
		var sb sandboxv1alpha1.SwiftSandbox
		if err := r.podReader().Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: name}, &sb); err == nil {
			continue
		} else if !apierrors.IsNotFound(err) {
			return held, err
		}
		if err := swiftgpu.DeallocateForWorkload(ctx, r.Client, allocatedTo); err != nil {
			return held, err
		}
	}
	return held, nil
}

// deleteWarmSlots deletes the pool's idle warm slot pods. They are owned by the
// pool, but while the GPU finalizer holds the pool, background garbage
// collection does not remove them (it waits for the owner to be gone) — and the
// finalizer in turn waits for them, so without this the two would wait on each
// other forever.
func (r *SwiftSandboxPoolReconciler) deleteWarmSlots(ctx context.Context, pool *sandboxv1alpha1.SwiftSandboxPool) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(pool.Namespace),
		client.MatchingLabels{PoolLabelKey: pool.Name, SlotStateLabelKey: slotStateWarm}); err != nil {
		return err
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, &pods.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

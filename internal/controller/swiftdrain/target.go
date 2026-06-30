package swiftdrain

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftgpu"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftmigration"
)

// selectTarget picks the destination node to evacuate the guest to: a node
// that is Ready, schedulable (not cordoned), not the draining node, satisfies
// the guest's node-class requirements (kernel-node label for kernel-boot),
// and has capacity for the guest's class. Among fitting candidates it picks
// the one with the most allocatable CPU (a cheap headroom proxy), breaking
// ties by name for determinism.
//
// Returns an error (no target) when no candidate fits — the drain then stalls
// safely (the eviction webhook keeps denying; the guest is never killed)
// until the operator frees capacity. The SwiftMigration Validating phase
// re-runs the authoritative capacity gate, so a transient race here only
// costs a retry, never a bad cutover.
func (r *Reconciler) selectTarget(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, drainingNode string) (string, error) {
	var class swiftv1alpha1.SwiftGuestClass
	if err := r.Get(ctx, client.ObjectKey{Name: guest.Spec.GuestClassRef.Name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("SwiftGuestClass %q not found", guest.Spec.GuestClassRef.Name)
		}
		return "", fmt.Errorf("get SwiftGuestClass %q: %w", guest.Spec.GuestClassRef.Name, err)
	}

	// For an offline-GPU-migratable guest, resolve its GPU profile so each
	// candidate node is also checked for vfio-readiness + free matching GPUs.
	var gpuProfile *gpuv1alpha1.SwiftGPUProfile
	if guest.OfflineGPUMigratable() {
		var p gpuv1alpha1.SwiftGPUProfile
		if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: guest.Spec.GPUProfileRef.Name}, &p); err != nil {
			return "", fmt.Errorf("get SwiftGPUProfile %q: %w", guest.Spec.GPUProfileRef.Name, err)
		}
		gpuProfile = &p
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}

	type cand struct {
		name     string
		allocCPU int64 // milliCPU, headroom proxy
	}
	var fitting []cand
	var skipped int
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Name == drainingNode {
			continue
		}
		if n.Spec.Unschedulable {
			skipped++
			continue
		}
		if !nodeReady(n) {
			skipped++
			continue
		}
		// Kernel-boot guests require a kernel-node-labeled target (the
		// migration webhook enforces this; pre-filter to avoid a doomed
		// migration object).
		if guest.Spec.KernelRef != nil && n.Labels["kubeswift.io/kernel-node"] != "true" {
			skipped++
			continue
		}
		if err := swiftmigration.NodeHasCapacity(ctx, r.Client, n, &class); err != nil {
			skipped++
			continue
		}
		// GPU guests: the target must also be vfio-ready with free matching GPUs.
		if gpuProfile != nil {
			if err := swiftgpu.GPUNodeHasCapacity(ctx, r.Client, n.Name, gpuProfile); err != nil {
				skipped++
				continue
			}
		}
		fitting = append(fitting, cand{name: n.Name, allocCPU: n.Status.Allocatable.Cpu().MilliValue()})
	}

	if len(fitting) == 0 {
		return "", fmt.Errorf("no Ready, schedulable peer node has capacity for class %q (%d candidate node(s) rejected)",
			class.Name, skipped)
	}

	sort.Slice(fitting, func(i, j int) bool {
		if fitting[i].allocCPU != fitting[j].allocCPU {
			return fitting[i].allocCPU > fitting[j].allocCPU // most headroom first
		}
		return fitting[i].name < fitting[j].name // deterministic tiebreak
	})
	return fitting[0].name, nil
}

// nodeReady reports whether the node's Ready condition is True.
func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

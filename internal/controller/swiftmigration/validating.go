package swiftmigration

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
)

// handleValidating implements the Validating phase.
//
// Responsibilities:
//   - Re-resolve source SwiftGuest and SwiftGuestClass (defense in depth
//     — the webhook already ran, but Phase 1's "verify, then act" rule
//     applies here: fail Validating with a structured Failed phase
//     rather than corrupt later phases on stale data).
//   - Compute and stamp status.SourceNode and status.DestinationNode
//     so operators see them in `kubectl get swiftmigration -o wide`.
//   - Stamp status.Mode = offline (Phase 1 always resolves to offline;
//     status records the actual selection for operator visibility).
//   - Run the manual capacity check (spike Q2): the target node must
//     have headroom for the guest's CPU + memory + launcher overhead.
//   - Set IPWillChange condition when allowIPChange=true triggered.
//   - Set Compatible=True and transition to Preparing on success.
//   - Set Compatible=False and Failed phase on any rejection.
//
// Idempotent on entry: re-running this handler in the Validating phase
// is safe because we never mutate the source guest or destination node;
// we only stamp status fields and decide on phase transition.
func (r *SwiftMigrationReconciler) handleValidating(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	// Phase 3a auto-mode pre-resolution. When spec.Mode=auto and
	// status.Mode is empty (initial entry), resolve to a concrete
	// mode and stamp status.Mode BEFORE the per-mode dispatch fires.
	// This shifts auto-resolution into a pre-dispatch step rather
	// than letting handleValidatingLive handle "auto-but-actually-
	// offline" mid-execution. After resolveAutoMode returns nil,
	// status.Mode is one of "live" or "offline" and isLiveMode is
	// unambiguous.
	if mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeAuto && status.Mode == "" {
		if res := r.resolveAutoMode(ctx, mig, status); res != nil {
			return res
		}
	}

	// Per-mode dispatch. isLiveMode handles both post-resolution
	// (status.Mode=live) and explicit-live initial entry
	// (status.Mode="" + spec.Mode=live).
	if isLiveMode(mig, status) {
		return r.handleValidatingLive(ctx, mig, status)
	}

	// Resolve source SwiftGuest in same namespace.
	var guest swiftv1alpha1.SwiftGuest
	if getErr := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return phaseFailure(fmt.Sprintf("source SwiftGuest %q no longer exists in namespace %q", mig.Spec.GuestRef.Name, mig.Namespace), "")
		}
		return phaseTransient(fmt.Errorf("get source SwiftGuest: %w", getErr))
	}

	// Defense in depth: re-check migration.enabled. The webhook caught
	// this at submission time, but a SwiftGuest mutation between
	// admission and our Reconcile could flip enabled to false.
	if guest.Spec.Migration != nil && guest.Spec.Migration.Enabled != nil && !*guest.Spec.Migration.Enabled {
		return phaseFailure(fmt.Sprintf("SwiftGuest %q has spec.migration.enabled=false", guest.Name), "")
	}

	// Stamp source/destination node and resolved mode. Phase 1 always
	// resolves to offline (the auto-mode logic from the architect's
	// answer to Q2 always picks offline in Phase 1; live mode is webhook-
	// rejected).
	status.SourceNode = guest.Status.NodeName
	status.DestinationNode = mig.Spec.Target.NodeName
	status.Mode = migrationv1alpha1.SwiftMigrationModeOffline

	// IPWillChange surfacing: when the operator opted into
	// allowIPChange and the guest is on default networking, the
	// migration will produce a fresh IP on the destination. Surface
	// this as a Warning condition for kubectl describe visibility.
	if mig.Spec.AllowIPChange && isDefaultNodeLocalNetworking(&guest) && status.SourceNode != "" && status.SourceNode != status.DestinationNode {
		setCondition(status, migrationv1alpha1.SwiftMigrationConditionIPWillChange,
			metav1.ConditionTrue, ReasonIPWillChange,
			fmt.Sprintf("guest %q is on default node-local networking; will receive a fresh IP on %q", guest.Name, status.DestinationNode))
	}

	// Resolve target node and run capacity check.
	var node corev1.Node
	if getErr := r.Get(ctx, client.ObjectKey{Name: mig.Spec.Target.NodeName}, &node); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return phaseFailure(fmt.Sprintf("target node %q no longer exists in cluster", mig.Spec.Target.NodeName), "")
		}
		return phaseTransient(fmt.Errorf("get target node: %w", getErr))
	}
	if node.Spec.Unschedulable {
		return phaseFailure(fmt.Sprintf("target node %q is cordoned (spec.unschedulable=true)", mig.Spec.Target.NodeName), "")
	}
	if !nodeReady(&node) {
		return phaseFailure(fmt.Sprintf("target node %q is not Ready", mig.Spec.Target.NodeName), "")
	}

	// Resolve SwiftGuestClass for resource requirements.
	var class swiftv1alpha1.SwiftGuestClass
	if getErr := r.Get(ctx, client.ObjectKey{Name: guest.Spec.GuestClassRef.Name}, &class); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return phaseFailure(fmt.Sprintf("SwiftGuestClass %q referenced by guest %q not found", guest.Spec.GuestClassRef.Name, guest.Name), "")
		}
		return phaseTransient(fmt.Errorf("get SwiftGuestClass: %w", getErr))
	}

	// Manual capacity check: read node Allocatable, list pods on the
	// node (excluding terminal phases), sum container requests, compute
	// headroom, compare against guest's CPU + Memory + launcher
	// overhead. spike Q2 found this approach is the cleanest gate
	// (server dry-run skips the scheduler; real-pod-probe leaves
	// debris).
	if err := r.checkNodeCapacity(ctx, &node, &class); err != nil {
		return phaseFailure(err.Error(), "")
	}

	// GPU target pre-flight (VFIO release-and-reallocate): the target node must
	// be vfio-ready and have free GPUs matching the guest's profile. Early,
	// clear rejection before the migration stops anything. Offline-only — VFIO
	// guests never reach the live branch (resolveAutoMode sends them offline;
	// the webhook rejects explicit mode=live for them).
	if guest.HasVFIODevices() {
		if res := r.gpuPreflight(ctx, mig, &guest); res != nil {
			return res
		}
	}

	// All checks passed. Mark Compatible=True, transition to Preparing.
	setCondition(status, migrationv1alpha1.SwiftMigrationConditionCompatible,
		metav1.ConditionTrue, ReasonValidating, "validation passed; target node has capacity")
	setReadyCondition(status, metav1.ConditionFalse, ReasonPreparing, "stopping source guest")
	setPhase(status, migrationv1alpha1.SwiftMigrationPhasePreparing)
	setPhaseDetail(status, "stopping source SwiftGuest")
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, "Validated",
			fmt.Sprintf("validation passed; migrating %q from %q to %q", guest.Name, status.SourceNode, status.DestinationNode))
	}
	return phaseAdvance()
}

// checkNodeCapacity verifies the target node has headroom for the
// guest's CPU + memory (including launcher overhead).
//
// Returns a structured error message (not a Go error) when capacity
// is insufficient — caller maps to Failed phase with the message in
// status.failureMessage. Returns nil when the check passes.
func (r *SwiftMigrationReconciler) checkNodeCapacity(
	ctx context.Context,
	node *corev1.Node,
	class *swiftv1alpha1.SwiftGuestClass,
) error {
	return NodeHasCapacity(ctx, r.Client, node, class)
}

// NodeHasCapacity verifies the node has headroom for the guest's CPU +
// memory (including launcher overhead). Exported so the Phase 4 drain
// controller reuses the exact capacity gate the migration Validating phase
// applies, instead of a second, drift-prone copy. Returns nil when the node
// fits; a descriptive error otherwise.
func NodeHasCapacity(
	ctx context.Context,
	c client.Client,
	node *corev1.Node,
	class *swiftv1alpha1.SwiftGuestClass,
) error {
	allocCPU, ok := node.Status.Allocatable[corev1.ResourceCPU]
	if !ok {
		return fmt.Errorf("target node %q has no Allocatable CPU reported", node.Name)
	}
	allocMem, ok := node.Status.Allocatable[corev1.ResourceMemory]
	if !ok {
		return fmt.Errorf("target node %q has no Allocatable memory reported", node.Name)
	}

	// Sum requests from pods on this node, excluding Failed/Succeeded.
	// Use a List with no field selector — fake client compatibility
	// (field selectors require an indexer setup that the fake client
	// doesn't provide by default). The filter by spec.nodeName is
	// done manually below; the cluster is small enough this is cheap.
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err != nil {
		return fmt.Errorf("list pods for capacity check: %w", err)
	}
	usedCPU := *resource.NewQuantity(0, resource.DecimalSI)
	usedMem := *resource.NewQuantity(0, resource.BinarySI)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != node.Name {
			continue
		}
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		for j := range p.Spec.Containers {
			c := &p.Spec.Containers[j]
			if cpu, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				usedCPU.Add(cpu)
			}
			if mem, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				usedMem.Add(mem)
			}
		}
		// Init containers can request resources; per Kubernetes
		// scheduler logic, the effective request is max(initRequest,
		// sum(containerRequests)). For the capacity-check headroom we
		// take the conservative sum: include init container requests
		// alongside main container requests. Init containers are
		// short-lived but during their run they reserve resources, so
		// counting them prevents pessimistic "this fits" outcomes
		// from briefly-stalled scheduling.
		for j := range p.Spec.InitContainers {
			c := &p.Spec.InitContainers[j]
			if cpu, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				usedCPU.Add(cpu)
			}
			if mem, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				usedMem.Add(mem)
			}
		}
	}

	headroomCPU := allocCPU.DeepCopy()
	headroomCPU.Sub(usedCPU)
	headroomMem := allocMem.DeepCopy()
	headroomMem.Sub(usedMem)

	// Required for the new launcher pod: SwiftGuestClass CPU + Memory
	// plus the LauncherMemoryOverheadMiB the pod builder adds to the
	// container's memory limit.
	needCPU := class.Spec.CPU
	needMem := class.Spec.Memory.DeepCopy()
	overhead := resource.NewQuantity(int64(swiftguest.LauncherMemoryOverheadMiB)*1024*1024, resource.BinarySI)
	needMem.Add(*overhead)

	if headroomCPU.Cmp(needCPU) < 0 {
		return fmt.Errorf("target node %q has insufficient CPU headroom: need %s, have %s (allocatable %s, used %s)",
			node.Name, needCPU.String(), headroomCPU.String(), allocCPU.String(), usedCPU.String())
	}
	if headroomMem.Cmp(needMem) < 0 {
		return fmt.Errorf("target node %q has insufficient memory headroom: need %s, have %s (allocatable %s, used %s)",
			node.Name, needMem.String(), headroomMem.String(), allocMem.String(), usedMem.String())
	}
	return nil
}

// isDefaultNodeLocalNetworking mirrors the webhook's helper. Duplicated
// here because importing the webhook package would cycle (webhook
// imports api types; controller imports webhook would cycle through
// scheme registration). Keep this textually identical to
// internal/webhook/swiftmigration/validator.go's copy.
func isDefaultNodeLocalNetworking(guest *swiftv1alpha1.SwiftGuest) bool {
	// See the webhook copy: the guest's primary IP is node-local unless the
	// PRIMARY interface rides a multi-node NAD. Both call the shared api helper
	// (PrimaryIPPreservedCrossNode) so the two paths can no longer drift.
	return !guest.PrimaryIPPreservedCrossNode()
}

// nodeReady mirrors the webhook helper.
func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

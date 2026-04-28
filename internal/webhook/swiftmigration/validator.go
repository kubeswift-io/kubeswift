// Package swiftmigration contains the admission webhook validator for
// SwiftMigration. It enforces the Phase 1 rejection rules from the
// architect review of the live-migration design:
//
//   - mode: live is rejected (Phase 1 ships offline only — Phase 3
//     work)
//   - target.nodeSelector is rejected (Phase 1 ships nodeName only —
//     forward-compatible field, Phase 4 work)
//   - target.nodeName XOR target.nodeSelector (mutually exclusive,
//     never both)
//   - source guest must exist and have spec.migration.enabled != false
//   - target node must exist, be Ready, and not cordoned
//   - source != target (offline same-node migration is meaningless)
//   - default-networking guests cannot cross-node-migrate without
//     spec.allowIPChange opt-in (Constraint 6 from the design doc;
//     spike Q1a confirmed IPs do not preserve across node-local
//     bridges)
//   - GPU guests (gpuProfileRef set OR any sriov interface) cannot
//     cross-node-migrate in Phase 1 (no release-and-reallocate
//     primitive yet — Phase 4+ work)
//   - spec is immutable after creation
//
// The webhook is the user-visible gate; the controller's Validating
// phase (commit 6) runs cheaper-to-verify-stale rules (capacity check,
// storage class compatibility) where transient failure should be
// retryable rather than rejected at submission time.
package swiftmigration

import (
	"context"
	"fmt"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// Phase 1 input bounds. These cap inputs the spec accepts for forward
// compatibility (timeout, parallelConnections) so unbounded values can't
// be planted now and become Phase 3+ footguns. Reason length / charset
// is bounded to prevent event/log injection (the field flows into
// status, events, and audit logs).
const (
	// MaxTimeout is the upper bound on spec.timeout. Phase 1 offline
	// migrations finish in tens of seconds (Longhorn) to a few minutes
	// (cold-cache CoW); 24h is an absurdly generous ceiling that still
	// rejects "999h" footguns.
	MaxTimeout = 24 * time.Hour
	// MaxParallelConnections caps the (Phase 1-ignored) live-mode
	// connection count. CH upstream supports up to 128; 16 covers
	// realistic live-migration uses without leaving room for resource
	// abuse via the Phase 3 controller when it lands.
	MaxParallelConnections = 16
	// MaxReasonLen bounds the free-form reason string. Long enough for
	// a meaningful audit-trail entry, short enough that the field
	// can't host a payload.
	MaxReasonLen = 256
)

// Validator validates SwiftMigration resources.
//
// Client is optional: when nil, only spec-shape rules are enforced.
// Production deployments always pass a Client; unit tests can omit it
// to exercise shape rules in isolation.
type Validator struct {
	Client client.Client
}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	mig, ok := obj.(*migrationv1alpha1.SwiftMigration)
	if !ok {
		return nil, fmt.Errorf("expected SwiftMigration, got %T", obj)
	}
	return v.validate(ctx, mig)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	mig, ok := newObj.(*migrationv1alpha1.SwiftMigration)
	if !ok {
		return nil, fmt.Errorf("expected SwiftMigration, got %T", newObj)
	}
	oldMig, ok := oldObj.(*migrationv1alpha1.SwiftMigration)
	if !ok {
		return nil, fmt.Errorf("expected SwiftMigration, got %T", oldObj)
	}
	if !specsEqual(&oldMig.Spec, &mig.Spec) {
		return nil, fmt.Errorf("SwiftMigration spec is immutable after creation; create a new SwiftMigration to retry with different inputs")
	}
	return v.validate(ctx, mig)
}

func (v *Validator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *Validator) validate(ctx context.Context, mig *migrationv1alpha1.SwiftMigration) (admission.Warnings, error) {
	if err := validateShape(mig); err != nil {
		return nil, err
	}
	if v.Client == nil {
		return nil, nil
	}
	return v.validateClusterState(ctx, mig)
}

// validateShape covers rules that depend only on the SwiftMigration
// itself: mode, target structure, timeoutStrategy. Extracted so unit
// tests can exercise it without a Client.
func validateShape(mig *migrationv1alpha1.SwiftMigration) error {
	if mig.Spec.GuestRef.Name == "" {
		return fmt.Errorf("spec.guestRef.name is required")
	}

	// Mode: live is rejected — Phase 1 ships offline only. The constant
	// PhaseLiveMigrationNotShipped (api/migration/v1alpha1) names the
	// gate so Phase 3 reviewers can find it via grep.
	switch mig.Spec.Mode {
	case "", migrationv1alpha1.SwiftMigrationModeAuto, migrationv1alpha1.SwiftMigrationModeOffline:
		// OK. auto resolves to offline in Phase 1; the controller records
		// the resolved mode in status.mode for operator visibility.
	case migrationv1alpha1.SwiftMigrationModeLive:
		return fmt.Errorf("spec.mode=live is not yet shipped (%s); live migration is reserved for Phase 3 — use mode: offline or mode: auto",
			migrationv1alpha1.PhaseLiveMigrationNotShipped)
	default:
		return fmt.Errorf("spec.mode=%q is not a recognised value (auto|live|offline)", mig.Spec.Mode)
	}

	// Target: exactly one of nodeName / nodeSelector.
	hasNodeName := mig.Spec.Target.NodeName != ""
	hasNodeSelector := len(mig.Spec.Target.NodeSelector) > 0
	if !hasNodeName && !hasNodeSelector {
		return fmt.Errorf("spec.target must specify exactly one of nodeName or nodeSelector")
	}
	if hasNodeName && hasNodeSelector {
		return fmt.Errorf("spec.target.nodeName and spec.target.nodeSelector are mutually exclusive")
	}
	if hasNodeSelector {
		// Phase 1 ships nodeName only. nodeSelector lands in the API for
		// forward compatibility (Phase 4 drain integration may pick a
		// candidate set rather than a specific node) but the Phase 1
		// controller doesn't implement it.
		return fmt.Errorf("spec.target.nodeSelector is not yet shipped; Phase 1 supports spec.target.nodeName only")
	}

	// TimeoutStrategy: ignore is reserved for live mode.
	switch mig.Spec.TimeoutStrategy {
	case "", migrationv1alpha1.SwiftMigrationTimeoutStrategyCancel:
		// OK.
	case migrationv1alpha1.SwiftMigrationTimeoutStrategyIgnore:
		return fmt.Errorf("spec.timeoutStrategy=ignore is reserved for live mode (Phase 3); Phase 1 supports cancel only")
	default:
		return fmt.Errorf("spec.timeoutStrategy=%q is not a recognised value (cancel|ignore)", mig.Spec.TimeoutStrategy)
	}

	// Phase 1 input bounds (security review): cap timeout, parallel
	// connections, and reason length/charset.
	if mig.Spec.Timeout != nil && mig.Spec.Timeout.Duration > MaxTimeout {
		return fmt.Errorf("spec.timeout=%s exceeds maximum %s", mig.Spec.Timeout.Duration, MaxTimeout)
	}
	if mig.Spec.ParallelConnections > MaxParallelConnections {
		return fmt.Errorf("spec.parallelConnections=%d exceeds maximum %d", mig.Spec.ParallelConnections, MaxParallelConnections)
	}
	if mig.Spec.ParallelConnections < 0 {
		return fmt.Errorf("spec.parallelConnections=%d must be non-negative", mig.Spec.ParallelConnections)
	}
	if len(mig.Spec.Reason) > MaxReasonLen {
		return fmt.Errorf("spec.reason length %d exceeds maximum %d characters", len(mig.Spec.Reason), MaxReasonLen)
	}
	if err := validateReasonChars(mig.Spec.Reason); err != nil {
		return err
	}

	return nil
}

// validateReasonChars rejects control characters in spec.reason. The
// field flows into Kubernetes events, status messages, and audit logs;
// permitting newlines, carriage returns, or terminal escapes would let
// an operator plant misleading log lines or spoof event messages.
// Whitespace (space, tab) is permitted; everything else in the C0
// control set and the DEL character is rejected.
func validateReasonChars(reason string) error {
	for i, r := range reason {
		if r == ' ' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("spec.reason contains a control character (0x%x) at offset %d; only printable characters and space/tab are permitted", r, i)
		}
	}
	return nil
}

// validateClusterState covers rules that need lookups: source guest
// existence, migration.enabled, target node Ready/cordoned, networking
// opt-in, GPU cross-node refusal. Returns warnings + error.
//
// Warnings are used to surface the IPWillChange notice when the
// operator opted into spec.allowIPChange — operators see this on
// kubectl apply rather than only after the migration completes.
func (v *Validator) validateClusterState(ctx context.Context, mig *migrationv1alpha1.SwiftMigration) (admission.Warnings, error) {
	// Resolve the source SwiftGuest.
	var guest swiftv1alpha1.SwiftGuest
	err := v.Client.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("source SwiftGuest %q not found in namespace %q", mig.Spec.GuestRef.Name, mig.Namespace)
	}
	if err != nil {
		return nil, fmt.Errorf("look up source SwiftGuest: %w", err)
	}

	// migration.enabled=false pins the guest in place.
	if guest.Spec.Migration != nil && guest.Spec.Migration.Enabled != nil && !*guest.Spec.Migration.Enabled {
		return nil, fmt.Errorf("SwiftGuest %q has spec.migration.enabled=false; migrations are not permitted (set enabled=true to allow)",
			guest.Name)
	}

	// Resolve source node from the guest. status.nodeName is set by the
	// SwiftGuest controller when the launcher pod is scheduled. If empty,
	// the guest hasn't started yet — accept the migration anyway because
	// Phase 1's Preparing phase will handle a not-yet-running source by
	// making the runPolicy patch a no-op.
	sourceNode := guest.Status.NodeName

	target := mig.Spec.Target.NodeName

	// Defensive guard: validateShape requires nodeName be set, but if a
	// future caller bypasses validateShape (or a Phase 4 patch leaves
	// target unset on a nodeSelector path that hasn't resolved yet), the
	// downstream Get with an empty name would surface as a confusing
	// NotFound. Reject explicitly.
	if target == "" {
		return nil, fmt.Errorf("spec.target.nodeName is empty (validation invariant violated)")
	}

	// source != target (same-node offline migration is meaningless).
	// Include source name in the message so operators see where the
	// guest is currently running — diagnoses the misuse on the spot.
	if sourceNode != "" && sourceNode == target {
		return nil, fmt.Errorf("spec.target.nodeName %q equals the SwiftGuest's current node %q; offline same-node migration is meaningless",
			target, sourceNode)
	}

	// Target node must exist and be schedulable.
	var node corev1.Node
	err = v.Client.Get(ctx, client.ObjectKey{Name: target}, &node)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("target node %q not found in cluster", target)
	}
	if err != nil {
		return nil, fmt.Errorf("look up target node: %w", err)
	}
	if node.Spec.Unschedulable {
		return nil, fmt.Errorf("target node %q is cordoned (spec.unschedulable=true); uncordon it or pick a different target",
			target)
	}
	if !nodeReady(&node) {
		return nil, fmt.Errorf("target node %q is not Ready (no Ready=True condition); pick a different target",
			target)
	}

	// Boot-type-specific node-label requirements. Defense-in-depth: when
	// spec.NodeName is propagated to pod.spec.NodeName, the kubelet may
	// admit the pod even on a node that lacks the label, but the launcher
	// will fail at startup because the required hostPath mount won't
	// exist. Catch this earlier here.
	if guest.Spec.KernelRef != nil {
		if node.Labels["kubeswift.io/kernel-node"] != "true" {
			return nil, fmt.Errorf("target node %q is missing label kubeswift.io/kernel-node=true required for kernel-boot guests",
				target)
		}
	}

	// GPU cross-node refusal: Phase 1 has no release-and-reallocate
	// primitive for the SwiftGPU controller. Migrating a GPU guest at
	// all (regardless of whether it's already scheduled) is rejected
	// because:
	//   - If sourceNode != "" and != target: classic cross-node
	//     migration — would orphan the source-node GPU reservation.
	//   - If sourceNode == "": guest is unscheduled, but the SwiftGPU
	//     controller's allocation is independent of spec.NodeName.
	//     Migrating an unscheduled GPU guest creates a race between
	//     SwiftGPU picking the allocation node and SwiftMigration
	//     pinning spec.NodeName, ending in the precedence rule from
	//     commit 3 rejecting the pod build.
	//   - If sourceNode == target: the same-node check above already
	//     rejected; this branch wouldn't be reached.
	// So: reject any GPU SwiftMigration in Phase 1, period.
	hasGPUWorkload := guest.Spec.GPUProfileRef != nil
	for _, iface := range guest.Spec.Interfaces {
		if iface.Type == swiftv1alpha1.InterfaceTypeSRIOV {
			hasGPUWorkload = true
			break
		}
	}
	if hasGPUWorkload {
		return nil, fmt.Errorf("SwiftGuest %q has VFIO devices (gpuProfileRef or sriov interface); cross-node migration is not supported in Phase 1 — Phase 4+ work pending a release-and-reallocate primitive",
			guest.Name)
	}

	// Networking opt-in: default node-local-bridge networking does not
	// preserve guest IPs cross-node (spike Q1a). Operator must opt into
	// spec.allowIPChange to acknowledge.
	if isDefaultNodeLocalNetworking(&guest) && sourceNode != "" && sourceNode != target {
		if !mig.Spec.AllowIPChange {
			return nil, fmt.Errorf("SwiftGuest %q is on KubeSwift's default node-local bridge networking; cross-node migration would change the guest's IP. Add a multi-node networkRef (Multus NAD) to spec.interfaces, or set spec.allowIPChange=true on this SwiftMigration to accept the IP change",
				guest.Name)
		}
		// Operator opted in; surface a warning so they see it on
		// kubectl apply. The controller (commit 5+) sets the
		// IPWillChange=True condition for kubectl describe visibility.
		return admission.Warnings{
			fmt.Sprintf("SwiftGuest %q is on default node-local networking and will receive a fresh IP on the target node (allowIPChange=true)", guest.Name),
		}, nil
	}

	return nil, nil
}

// isDefaultNodeLocalNetworking returns true when the guest uses
// KubeSwift's default node-local bridge networking. Architect Q4
// detection rule: guest is on default networking iff spec.interfaces
// is nil/empty OR every interface has NetworkRef==nil AND Type is
// "" or "bridge".
//
// type==sriov on its own interface counts as multi-node-capable from
// a hypothetical "the VF survives migration" standpoint, BUT the GPU
// cross-node refusal above already blocks SR-IOV migrations
// (sriov is treated as VFIO for the purposes of cross-node Phase 1
// refusal). The two checks compose: a guest with only sriov
// interfaces hits the GPU refusal first; this check would not fire
// because of the type check.
func isDefaultNodeLocalNetworking(guest *swiftv1alpha1.SwiftGuest) bool {
	if len(guest.Spec.Interfaces) == 0 {
		return true
	}
	for _, iface := range guest.Spec.Interfaces {
		if iface.NetworkRef != nil {
			return false
		}
		// type == sriov is multi-node-capable in principle, but blocked
		// by the GPU refusal above. We treat type==sriov as "not
		// default" so this function returns false consistently with
		// its name even when the migration is going to be rejected
		// elsewhere.
		if iface.Type == swiftv1alpha1.InterfaceTypeSRIOV {
			return false
		}
	}
	return true
}

func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// specsEqual is the immutability check helper. SwiftMigration spec
// changes after creation produce a new resource (mirrors SwiftSnapshot's
// immutability rule).
func specsEqual(a, b *migrationv1alpha1.SwiftMigrationSpec) bool {
	if a.GuestRef != b.GuestRef {
		return false
	}
	if a.Target.NodeName != b.Target.NodeName {
		return false
	}
	if !stringMapEqual(a.Target.NodeSelector, b.Target.NodeSelector) {
		return false
	}
	if a.Mode != b.Mode {
		return false
	}
	if !durationPtrEqual(a.DowntimeTarget, b.DowntimeTarget) {
		return false
	}
	if a.ParallelConnections != b.ParallelConnections {
		return false
	}
	if !durationPtrEqual(a.Timeout, b.Timeout) {
		return false
	}
	if a.TimeoutStrategy != b.TimeoutStrategy {
		return false
	}
	if a.Reason != b.Reason {
		return false
	}
	if a.AllowIPChange != b.AllowIPChange {
		return false
	}
	return true
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func durationPtrEqual(a, b *metav1.Duration) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Duration == b.Duration
}

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
	"github.com/projectbeskar/kubeswift/internal/resolved"
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
	// MinLiveTimeout is the lower bound on spec.timeout for mode=live.
	// Phase 3a's cutover ordering invariant (3-step forward-only-retry-
	// in-place sequence) requires headroom for transient-failure
	// retries: controller-runtime's exponential backoff on cutover
	// step retries plus the kernel TCP retransmit window (~127s on
	// network blackhole per Q3-v3 spike) means timeouts under ~60s
	// risk producing FailureReasonTimeout on transient issues that
	// would otherwise self-resolve. Architect-discipline review Q3.2
	// mitigation. Offline mode is unaffected (Phase 1 offline finishes
	// in tens of seconds and has no comparable retry semantics).
	MinLiveTimeout = 60 * time.Second
	// MaxParallelConnections caps the (Phase 1-ignored) live-mode
	// connection count. CH upstream supports up to 128; 16 covers
	// realistic live-migration uses without leaving room for resource
	// abuse via the Phase 3 controller when it lands.
	MaxParallelConnections = 16
	// MinDowntimeTarget / MaxDowntimeTarget bound spec.downtimeTarget,
	// the CH `downtime_ms` vCPU-pause budget (CH >= v52). Below ~10ms a
	// guest with any meaningful dirty rate can never converge under the
	// target and the migration just runs to spec.timeout; above ~10s the
	// "live" migration's frozen window stops being meaningfully live (use
	// offline). Live-mode only; ignored for offline.
	MinDowntimeTarget = 10 * time.Millisecond
	MaxDowntimeTarget = 10 * time.Second
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

// Per-operation validation discipline.
//
// Admission webhooks fire on every operation that touches the resource.
// A "validate everything every time" default is the bug pattern that
// produced this PR's three defects:
//
//   - Bug A (HIGH): finalizer-removal patches issued during DELETE flow
//     hit ValidateUpdate; cluster-state validation tripped on a missing
//     source SwiftGuest. Result: SwiftMigration could not be deleted
//     via any normal kubectl operation.
//   - Bug B (MEDIUM): finalizer-removal patches on terminal-phase
//     migrations (Completed/Failed/Cancelled) hit the same
//     cluster-state validation, rejected because source==target post-
//     cutover. Result: stale completed migrations re-reconciled forever.
//   - Bug C (MEDIUM): in-flight migrations (Pending/Validating/Preparing)
//     whose source guest was deleted mid-flight hit the same rejection
//     on every metadata patch (finalizer add, status flips that bring
//     the controller back through ensureFinalizer). Result: trapped
//     in-flight migrations the controller couldn't fail-and-clean-up.
//
// The discipline is "default to explicit": each operation enumerates
// what validation fires and why. Future SwiftMigration phases (live
// mode, drain integration) and other webhooks in this codebase should
// follow the same pattern.

// ValidateCreate runs full validation: spec shape AND cluster state.
// CREATE is the submission point — when the operator's intent first
// hits the API server. This is the right place to gate on cluster-
// state matching that intent (source guest exists, target node Ready,
// no conflicting in-flight migration, etc).
func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	mig, ok := obj.(*migrationv1alpha1.SwiftMigration)
	if !ok {
		return nil, fmt.Errorf("expected SwiftMigration, got %T", obj)
	}
	return v.validate(ctx, mig)
}

// ValidateUpdate runs spec immutability + spec shape ONLY.
// Cluster-state validation is intentionally skipped because:
//
//  1. Spec immutability (enforced below) means the spec on UPDATE is
//     byte-identical to the spec admitted at CREATE — and CREATE
//     already validated cluster-state. Re-validating against currently-
//     evolving cluster state cannot detect new spec problems (none
//     exist) and only blocks legitimate metadata churn.
//  2. Cluster-state changes between CREATE and UPDATE — source guest
//     deleted, target node moved, source==target after cutover — are
//     the controller's domain to detect and react to (mark migration
//     Failed, drive cleanup), not the webhook's domain to gate. The
//     webhook gating those state changes turns transient cluster
//     conditions into stuck resources.
//  3. UPDATE patches in this codebase are exclusively controller-
//     issued: finalizer add/remove, annotation flips. Status changes
//     don't reach this webhook (we don't subscribe to the status
//     subresource). All controller-issued patches need to succeed
//     idempotently for the controller to make forward progress.
//
// validateShape is kept as a defense-in-depth (cheap, no-op on an
// already-validated unchanged spec) but cluster-state lookups are
// skipped wholesale.
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
	return nil, validateShape(mig)
}

// ValidateDelete is a pass-through. Deletion is always permitted at
// the webhook layer — the controller's finalizer cleanup is the gate
// that ensures cleanup runs before the resource is removed from
// storage.
func (v *Validator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate runs the full validation suite (shape + cluster state).
// Called from ValidateCreate ONLY. ValidateUpdate intentionally does
// not call this — see ValidateUpdate's doc comment for the rationale.
//
// Defense-in-depth: should a future caller re-route an UPDATE through
// this function, the per-operation discipline is undermined. Reviewers
// should reject any such change unless the corresponding rationale is
// updated explicitly.
func (v *Validator) validate(ctx context.Context, mig *migrationv1alpha1.SwiftMigration) (admission.Warnings, error) {
	if err := validateShape(mig); err != nil {
		return nil, err
	}
	if v.Client == nil {
		return nil, nil
	}
	return v.validateClusterState(ctx, mig)
}

// isTerminalPhase returns true for SwiftMigration phases where the
// outcome has been decided. The webhook does not consult this helper
// directly anymore (per-operation discipline obviates the need), but
// the controller's Reconcile short-circuit does, and tests in this
// package lock in the canonical phase set so the controller's
// duplicated copy can be cross-checked against it.
func isTerminalPhase(phase migrationv1alpha1.SwiftMigrationPhase) bool {
	switch phase {
	case migrationv1alpha1.SwiftMigrationPhaseCompleted,
		migrationv1alpha1.SwiftMigrationPhaseFailed,
		migrationv1alpha1.SwiftMigrationPhaseCancelled:
		return true
	}
	return false
}

// validateShape covers rules that depend only on the SwiftMigration
// itself: mode, target structure, timeoutStrategy. Extracted so unit
// tests can exercise it without a Client.
func validateShape(mig *migrationv1alpha1.SwiftMigration) error {
	if mig.Spec.GuestRef.Name == "" {
		return fmt.Errorf("spec.guestRef.name is required")
	}

	// Mode: Phase 3a accepts live (PR #41 shipped swiftletd D1/D2/D3 +
	// this PR's controller). The PhaseLiveMigrationNotShipped constant
	// stays in api/migration/v1alpha1 for historical grep but no longer
	// gates admission — auto/live/offline are all valid Phase 3a modes.
	switch mig.Spec.Mode {
	case "", migrationv1alpha1.SwiftMigrationModeAuto, migrationv1alpha1.SwiftMigrationModeOffline,
		migrationv1alpha1.SwiftMigrationModeLive:
		// OK. The controller's Validating phase resolves auto to live
		// when capabilities permit (live-migration-capable storage AND
		// non-default networking OR allowIPChange AND no GPU/SR-IOV);
		// else auto resolves to offline. Live-mode-specific cluster-
		// state checks (per-source-node concurrency, name length cap,
		// kernel-boot-vs-PVC storage gate) live in validateClusterState.
	default:
		return fmt.Errorf("spec.mode=%q is not a recognised value (auto|live|offline)", mig.Spec.Mode)
	}

	// Phase 3a live-mode shape rule: guest name length cap.
	// Per docs/design/live-migration-phase-3a.md §3.2, the destination
	// pod is named `<guest>-mig-<short-uid>` (5 + 6 = 11 chars suffix);
	// Kubernetes pod names are bounded at 253 chars (DNS-1123 subdomain
	// rule). Cap guest name at 242 to leave headroom. Phase 1 offline
	// mode reuses guest.Name as the post-migration pod name and is
	// unaffected.
	if mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		const liveModeGuestNameMaxLen = 242
		if len(mig.Spec.GuestRef.Name) > liveModeGuestNameMaxLen {
			return fmt.Errorf(
				"spec.guestRef.name length %d exceeds %d for mode=live; the destination pod name `<guest>-mig-<short-uid>` would exceed the 253-char Kubernetes pod name limit. Use a shorter SwiftGuest name or mode=offline (which reuses the guest name unchanged).",
				len(mig.Spec.GuestRef.Name), liveModeGuestNameMaxLen)
		}
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
	// Phase 5 retention: a non-positive ttl would delete the migration the
	// instant it reached terminal (or has no meaning) — reject it.
	if mig.Spec.TTL != nil && mig.Spec.TTL.Duration <= 0 {
		return fmt.Errorf("spec.ttl must be > 0 when set (got %s)", mig.Spec.TTL.Duration)
	}
	// Live-mode minimum: see MinLiveTimeout doc. Only enforced for
	// mode=live; mode=auto and mode=offline are unaffected. Phase 3a
	// architect-discipline Q3.2 mitigation.
	if mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive &&
		mig.Spec.Timeout != nil && mig.Spec.Timeout.Duration > 0 &&
		mig.Spec.Timeout.Duration < MinLiveTimeout {
		return fmt.Errorf("spec.timeout=%s is below minimum %s for mode=live (cutover retries need headroom)",
			mig.Spec.Timeout.Duration, MinLiveTimeout)
	}
	if mig.Spec.ParallelConnections > MaxParallelConnections {
		return fmt.Errorf("spec.parallelConnections=%d exceeds maximum %d", mig.Spec.ParallelConnections, MaxParallelConnections)
	}
	if mig.Spec.ParallelConnections < 0 {
		return fmt.Errorf("spec.parallelConnections=%d must be non-negative", mig.Spec.ParallelConnections)
	}
	// downtimeTarget bound (CH downtime_ms, live mode). Only meaningful for
	// live mode; offline migration's downtime is storage-detach + boot
	// bound, not a CH vCPU-pause budget. Enforced whenever set so a stray
	// value on an auto/offline migration is still caught.
	if dt := mig.Spec.DowntimeTarget; dt != nil && dt.Duration != 0 {
		if dt.Duration < MinDowntimeTarget || dt.Duration > MaxDowntimeTarget {
			return fmt.Errorf("spec.downtimeTarget=%s is outside the allowed range [%s, %s]",
				dt.Duration, MinDowntimeTarget, MaxDowntimeTarget)
		}
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

	// VFIO migration gate. VFIO devices CANNOT live-migrate — Cloud Hypervisor
	// cannot transfer device state to the destination — so mode=live + VFIO is
	// rejected. GPU guests CAN migrate OFFLINE via the release-and-reallocate
	// path (the migration controller reserves target GPUs before stopping the
	// source, frees the source at cutover, stamps status.GPU=target).
	if guest.HasVFIODevices() && mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		return nil, fmt.Errorf("SwiftGuest %q has VFIO devices (gpuProfileRef or sriov interface); they cannot live-migrate — use mode=offline (or auto, which resolves to offline for VFIO GPU guests)",
			guest.Name)
	}
	// SR-IOV NIC passthrough cannot migrate cross-node at all: the offline GPU
	// release-and-reallocate path handles GPUs only; reattaching an SR-IOV NIC
	// on the target is out of scope. (A GPU-only guest, no sriov, IS offline-
	// migratable.)
	if guest.HasSRIOVInterface() {
		return nil, fmt.Errorf("SwiftGuest %q has an SR-IOV interface; cross-node migration of SR-IOV NIC passthrough is not supported",
			guest.Name)
	}
	// Node-local virtio backend gate (virtiofs / vhost-user — design doc
	// vhost-user-devices.md §7). The virtiofsd processes, their source mounts,
	// and the operator's vhost-user backend sockets live in/on the SOURCE pod
	// and node; CH live migration does not transfer them, so the resumed guest's
	// virtiofs mounts / vhost-user devices would break mid-flight. Offline
	// migration is fine: the launcher pod is recreated on the target, where the
	// backends are re-established (auto mode resolves these guests to offline,
	// mirroring VFIO).
	if guest.HasNodeLocalVirtioBackends() && mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		return nil, fmt.Errorf("SwiftGuest %q has node-local virtio backends (spec.filesystems, spec.vhostUserDevices, or a vhost-user interface); they cannot live-migrate — use mode=offline (or auto, which resolves to offline for such guests)",
			guest.Name)
	}

	// Model A (primary OVN-K UDN) live-migration gate. A guest on its namespace's
	// primary OVN-Kubernetes UDN cannot live-migrate in v1: the primary UDN withholds
	// the swiftletd<->swiftletd migration channel from the pod (the destination pod's
	// eth0 is infrastructure-locked, dropping pod-to-pod traffic). Offline migration
	// works (the target acquires a fresh UDN IP); auto resolves to offline
	// (auto_mode.go), mirroring VFIO. See docs/design/udn-primary-integration.md.
	if mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		modelA, err := resolved.NamespaceHasPrimaryUDN(ctx, v.Client, mig.Namespace)
		if err != nil {
			return nil, fmt.Errorf("checking primary-UDN namespace for SwiftGuest %q: %w", guest.Name, err)
		}
		if modelA {
			return nil, fmt.Errorf("SwiftGuest %q is on the namespace primary OVN-K UDN (Model A); live migration is not supported in v1 — the primary UDN withholds the migration channel from the pod. Use mode=offline (or auto, which resolves to offline).",
				guest.Name)
		}
	}

	// Live-mode storage gate (W6 follow-up; Phase 3a kernel-boot
	// adjustment).
	//
	// Live migration of disk-boot guests requires RWX+Block storage
	// (KubeVirt's model and Longhorn's Migratable RWX). Filesystem RWX
	// is NOT live-migration-capable. Kernel-boot guests (kernelRef set,
	// no root-disk PVC) have no shared storage to coordinate at all,
	// so the storage gate doesn't apply — they're live-capable by
	// virtue of having no PVC. Design doc §1 lists kernel-boot as a
	// Phase 3a target workload class; the storage gate must NOT
	// reject them.
	//
	// We RECOMPUTE the resolved storage spec here rather than reading
	// SwiftGuest.status.storage. Status reads are stale during cluster
	// restore (controller hasn't reconciled yet) and would create false
	// rejections.
	if mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		if err := v.gateLiveModeStorage(ctx, &guest); err != nil {
			return nil, err
		}
	}

	// Phase 3a per-source-node concurrency rule (live mode).
	// Per docs/design/live-migration-phase-3a.md §5.4: at most one
	// in-flight live migration per source node. The controller observes
	// both source and destination pods via the labeled-watch (§5.1);
	// concurrent migrations from the same source node would race on
	// the source pod's annotations and produce non-deterministic
	// dispatch outcomes.
	//
	// We list SwiftMigrations cluster-wide and reject if any other
	// non-terminal SwiftMigration has the same status.sourceNode AND
	// mode=live. Reading status of OTHER resources is allowed under
	// per-operation discipline (PR #26): the rule prohibits reading
	// status of THIS resource being admitted; reading peers is fine.
	//
	// Phase 1 offline migrations are exempt — they don't conflict with
	// live mode (different state surfaces) and the per-source-guest
	// annotation conflict check in Preparing is the floor for offline.
	if mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive && sourceNode != "" {
		if err := v.checkPerSourceNodeConcurrency(ctx, mig, sourceNode); err != nil {
			return nil, err
		}
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

// gateLiveModeStorage rejects live-mode migrations for guests whose
// resolved storage spec is not live-migration-capable.
//
// Two cases pass the gate:
//
//  1. Kernel-boot guests (kernelRef set, no root-disk PVC). With no
//     shared storage to coordinate, there is no F2 split-brain
//     concern and no cross-node storage handoff to perform — the
//     guest's root filesystem is the initramfs which lives in CH's
//     migrated memory. Phase 3a design doc §1 lists kernel-boot as
//     a Phase 3a target workload class; the storage gate exists to
//     reject INCAPABLE storage, not to require storage where none
//     exists.
//
//  2. Disk-boot guests with RWX+Block storage (KubeVirt's model;
//     Longhorn's Migratable RWX). Filesystem RWX is NOT
//     live-migration-capable.
//
// Implementation: read the SwiftGuestClass referenced by the guest,
// recompute the merged storage spec via resolved.MergeStorage, and
// reject only when the resolved storage is non-empty AND not
// RWX+Block. Recomputation (rather than reading SwiftGuest.status.
// storage) avoids the controller-write-back race during cluster
// restore.
func (v *Validator) gateLiveModeStorage(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) error {
	// Kernel-boot guests are live-capable by virtue of having no
	// shared storage. The kernelRef field is the discriminator —
	// SwiftGuest CRD admission rejects guests with both kernelRef
	// AND imageRef, so the presence of kernelRef implies absence
	// of disk-boot storage.
	if guest.Spec.KernelRef != nil {
		return nil
	}

	var class swiftv1alpha1.SwiftGuestClass
	err := v.Client.Get(ctx, client.ObjectKey{Name: guest.Spec.GuestClassRef.Name}, &class)
	if apierrors.IsNotFound(err) {
		// Class missing is not a storage-gate concern — the controller
		// will fail resolution and surface ResolutionFailed. Don't
		// double-reject here on an unrelated condition.
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up SwiftGuestClass %q for live-mode storage check: %w",
			guest.Spec.GuestClassRef.Name, err)
	}
	storage := resolved.MergeStorage(guest, &class)
	if storage.IsLiveMigrationCapable() {
		return nil
	}
	return fmt.Errorf(
		"SwiftGuest %q resolved storage is accessMode=%s volumeMode=%s; live migration requires accessMode=ReadWriteMany AND volumeMode=Block (KubeVirt-style live migration; Filesystem RWX is not live-migration-capable). "+
			"Set spec.storage on the SwiftGuest or its SwiftGuestClass to ReadWriteMany+Block, or use spec.mode=offline. See docs/design/storage-access-mode.md.",
		guest.Name, storage.AccessMode, storage.VolumeMode,
	)
}

// checkPerSourceNodeConcurrency rejects a live-mode SwiftMigration
// when another non-terminal live SwiftMigration is already in flight
// from the same source node. Per docs/design/live-migration-phase-3a.md
// §5.4.
//
// Reading peer SwiftMigrations' status is allowed under the
// per-operation discipline (PR #26): the rule prohibits reading
// status of the resource being admitted, not status of OTHER
// resources observed at admission time.
func (v *Validator) checkPerSourceNodeConcurrency(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	sourceNode string,
) error {
	var migs migrationv1alpha1.SwiftMigrationList
	if err := v.Client.List(ctx, &migs); err != nil {
		return fmt.Errorf("list SwiftMigrations for per-source-node concurrency check: %w", err)
	}
	for i := range migs.Items {
		other := &migs.Items[i]
		// Skip the resource being admitted (CREATE has no UID match
		// yet; compare by namespaced name).
		if other.Namespace == mig.Namespace && other.Name == mig.Name {
			continue
		}
		// Only live mode conflicts with live mode. Phase 1 offline
		// migrations don't share state with live mode.
		if other.Spec.Mode != migrationv1alpha1.SwiftMigrationModeLive {
			continue
		}
		// Skip terminal-phase peers.
		if isTerminalPhase(other.Status.Phase) {
			continue
		}
		// Per-source-node match: reject.
		if other.Status.SourceNode == sourceNode {
			return fmt.Errorf(
				"another live SwiftMigration %q is in flight from source node %q (phase=%s); per-source-node concurrency limits to one in-flight live migration. Wait for the existing migration to complete or fail.",
				client.ObjectKeyFromObject(other), sourceNode, other.Status.Phase,
			)
		}
	}
	return nil
}

// isDefaultNodeLocalNetworking returns true when the guest uses
// KubeSwift's default node-local bridge networking. Architect Q4
// detection rule: guest is on default networking iff spec.interfaces
// is nil/empty OR every interface has NetworkRef==nil AND Type is
// "" or "bridge".
//
// Keep in sync with internal/controller/swiftmigration/validating.go's
// isDefaultNodeLocalNetworking. Both copies must remain textually
// identical; the controller can't import the webhook (cycle) and the
// webhook only imports api types.
//
// type==sriov on its own interface counts as multi-node-capable from
// a hypothetical "the VF survives migration" standpoint, BUT the GPU
// cross-node refusal above already blocks SR-IOV migrations
// (sriov is treated as VFIO for the purposes of cross-node Phase 1
// refusal). The two checks compose: a guest with only sriov
// interfaces hits the GPU refusal first; this check would not fire
// because of the type check.
func isDefaultNodeLocalNetworking(guest *swiftv1alpha1.SwiftGuest) bool {
	// The guest's primary IP is node-local (and changes on migration) unless
	// the PRIMARY interface rides a multi-node NAD. A secondary NAD does not
	// preserve the primary IP, so it no longer flips this to false (the prior
	// "any networkRef" heuristic was the §7.2 gap). SR-IOV guests are blocked
	// by the VFIO refusal before this is consulted.
	return !guest.PrimaryIPPreservedCrossNode()
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

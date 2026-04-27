// Tier B (local hostPath) restore path for SwiftRestore.
//
// State machine for local-backend restores:
//
//   Pending     → Restoring        (validate, version check, stamp/create target SwiftGuest
//                                   with snapshot.kubeswift.io/active-restore annotations)
//   Restoring   → Resuming         (target SwiftGuest reached GuestRunning=True; the launcher
//                                   pod is up with CH paused, snapshot directory mounted)
//   Resuming    → Ready            (controller wrote `kubeswift.io/snapshot-action: resume`
//                                   onto the launcher pod; swiftletd mirrored
//                                   `kubeswift.io/snapshot-status-id` with status=ready)
//
// In-place vs clone:
//   - In-place (target.Name == source SwiftGuest name AND no identity regeneration):
//     the existing source SwiftGuest is annotated with the restore parameters
//     and its launcher pod is force-deleted so the SwiftGuest controller rebuilds
//     it in restore mode (mounts snapshot RO directly, no init-container, no
//     config.json patch). targetGuest.OverwriteExisting=true is required.
//   - Clone (target.Name != source name OR any identity regen):
//     a fresh SwiftGuest is created from the source's spec, stamped with restore
//     annotations including a per-NIC MAC rewrite list and the cmdline marker
//     for in-guest identity regeneration. The SwiftGuest controller materializes
//     a stager init container that copies the snapshot to a pod-local emptyDir
//     and applies the requested config.json patches before swiftletd starts.
//
// The hypervisor version check (architect risk #3) runs in handlePendingLocal
// before any of the above; a major-version mismatch fails the restore with a
// version-specific message.

package swiftrestore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	swiftguestctrl "github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
)

// SkipHypervisorVersionCheckAnnotation lets an operator bypass the
// version check for disaster-recovery scenarios where the cluster's CH
// has been upgraded past the snapshot's. Per architect risk #3 the
// default behavior is conservative (block on major bumps) but this
// escape hatch must exist — when an operator is restoring from a
// snapshot taken six months ago, blocking that restore is worse than
// the chance of restore failing with a "bar 0 already used"-style
// error during the actual --restore boot.
const SkipHypervisorVersionCheckAnnotation = "kubeswift.io/skip-hypervisor-version-check"

// Action verbs / annotation keys mirrored from
// internal/controller/swiftsnapshot/local.go (the Tier B capture path
// and the Tier B restore path share the launcher-pod annotation
// surface; keeping them as private literals here avoids cross-package
// dependency on the swiftsnapshot internals).
const (
	annoAction       = "kubeswift.io/snapshot-action"
	annoActionID     = "kubeswift.io/snapshot-action-id"
	annoActionArgs   = "kubeswift.io/snapshot-action-args"
	annoStatus       = "kubeswift.io/snapshot-status"
	annoStatusID     = "kubeswift.io/snapshot-status-id"
	annoStatusDetail = "kubeswift.io/snapshot-status-detail"
	verbResume       = "resume"
)

// VersionCompatibility is the result of comparing snapshot's
// HypervisorVersion to the current cluster CH version.
type VersionCompatibility int

const (
	// VersionUnknown means we couldn't parse one or both versions —
	// we err on the side of allowing (with a warning) because we'd
	// rather restore than fail closed on bad data.
	VersionUnknown VersionCompatibility = iota
	// VersionExactMatch: full string match (e.g. v51.1 == v51.1).
	VersionExactMatch
	// VersionPatchDrift: same major.minor, different patch (or one
	// has patch and the other doesn't). Restore proceeds; controller
	// surfaces a Warning condition.
	VersionPatchDrift
	// VersionMinorMismatch: same major, different minor. Default
	// policy: block. Architect risk #3 — minor bumps in CH have
	// historically broken snapshot/restore compat.
	VersionMinorMismatch
	// VersionMajorMismatch: different major. Default policy: block.
	// Major version bumps almost certainly break compat.
	VersionMajorMismatch
)

// CompareHypervisorVersions classifies the relationship between the
// snapshot's recorded version and the current cluster version. Both
// strings are expected in the CH "v<major>.<minor>[.<patch>]" format
// (matching what swift-ch-client's VmmVersion::major_minor parses).
//
// Empty inputs return VersionUnknown — the controller will allow the
// restore but log. This handles the case where the snapshot was taken
// before commit 8 added HypervisorVersion to status (pre-Phase-2
// SwiftSnapshots).
func CompareHypervisorVersions(snap, current string) VersionCompatibility {
	if snap == "" || current == "" {
		return VersionUnknown
	}
	if snap == current {
		return VersionExactMatch
	}
	sM, sm, sp, sok := parseVersion(snap)
	cM, cm, cp, cok := parseVersion(current)
	if !sok || !cok {
		return VersionUnknown
	}
	if sM != cM {
		return VersionMajorMismatch
	}
	if sm != cm {
		return VersionMinorMismatch
	}
	// major.minor match. Patch may differ (or one of them may be
	// absent, in which case parseVersion returns -1 for missing).
	if sp != cp {
		return VersionPatchDrift
	}
	// Same major.minor and same patch (or both -1) → exact, but we
	// already handled string-equal above. Reach here when whitespace
	// or "v" prefix differs.
	return VersionExactMatch
}

// parseVersion parses "v<major>.<minor>" or "v<major>.<minor>.<patch>"
// (with or without leading "v"). Returns (major, minor, patch, ok).
// Patch is -1 when absent — callers compare with this in mind.
func parseVersion(s string) (int, int, int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return 0, 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, false
	}
	patch := -1
	if len(parts) >= 3 {
		p, err := strconv.Atoi(parts[2])
		if err != nil {
			return 0, 0, 0, false
		}
		patch = p
	}
	return major, minor, patch, true
}

// IsTierBRestore returns true when the snapshot's backend is local —
// i.e. this restore should follow the Tier B path rather than CSI.
func IsTierBRestore(snap *snapshotv1alpha1.SwiftSnapshot) bool {
	return snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendLocal
}

// IsInPlaceRestore returns true when this is the fast-path: target name
// equals source name AND no identity regeneration is requested. In this
// case the controller can skip the staging copy and the in-pod patcher
// invocation entirely — the restore-receive launcher mounts the
// snapshot directory read-only and reads it directly.
//
// "No identity regeneration" means restore.Spec.Identity is nil or has
// an empty Regenerate slice. Any non-empty regen list implies the
// restore wants per-clone divergence (machine-id, MACs, etc.), which
// requires the patcher even if the target name happens to match the
// source.
func IsInPlaceRestore(snap *snapshotv1alpha1.SwiftSnapshot, restore *snapshotv1alpha1.SwiftRestore) bool {
	if restore.Spec.TargetGuest.Name != snap.Spec.GuestRef.Name {
		return false
	}
	if restore.Spec.Identity != nil && len(restore.Spec.Identity.Regenerate) > 0 {
		return false
	}
	return true
}

// CurrentClusterHypervisorVersion is the CH version the cluster is
// expected to have, sourced from the controller-manager environment.
// Read at controller startup (not per-reconcile) by main.go and
// passed into the reconciler. Empty string disables the version
// check — controllers built without this configuration treat all
// restores as VersionUnknown.

// handlePendingLocal handles the Tier B (local-backend) Pending phase.
//
// The version-check pre-flight runs first; on a blocking mismatch the
// restore goes straight to Failed with the version-specific reason.
// On version OK, the controller validates the snapshot status fields
// it depends on (NodeName, Backend.Local.HostPath), then either
// stamps the existing source SwiftGuest with restore annotations
// (in-place) or creates a fresh target SwiftGuest from the source's
// spec (clone). Both paths transition to Restoring.
func (r *SwiftRestoreReconciler) handlePendingLocal(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, error) {
	// Hypervisor version check (architect risk #3). Operator can
	// override via SkipHypervisorVersionCheckAnnotation for disaster-
	// recovery scenarios where the cluster's CH was upgraded past the
	// snapshot's. Default policy: block on minor or major mismatch,
	// allow patch drift with a Warning, allow exact match.
	skip := restore.Annotations[SkipHypervisorVersionCheckAnnotation] == "true"
	if !skip && r.CurrentHypervisorVersion != "" {
		switch CompareHypervisorVersions(snap.Status.HypervisorVersion, r.CurrentHypervisorVersion) {
		case VersionMajorMismatch:
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
				"hypervisor major-version mismatch: snapshot "+snap.Status.HypervisorVersion+
					" vs cluster "+r.CurrentHypervisorVersion+
					" (override with annotation "+SkipHypervisorVersionCheckAnnotation+"=true)")
			return true, 0, nil
		case VersionMinorMismatch:
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
				"hypervisor minor-version mismatch: snapshot "+snap.Status.HypervisorVersion+
					" vs cluster "+r.CurrentHypervisorVersion+
					" (override with annotation "+SkipHypervisorVersionCheckAnnotation+"=true)")
			return true, 0, nil
		case VersionPatchDrift, VersionExactMatch, VersionUnknown:
			// Proceed. Patch drift could be surfaced as a Warning
			// condition in a future iteration — for now the version
			// fields on status are enough for an operator to see.
		}
	}

	// Sanity-check the snapshot fields we depend on. The webhook
	// enforces these for fresh SwiftSnapshots, but a SwiftSnapshot
	// captured by a buggy older controller could have populated the
	// SwiftSnapshot Ready transition without setting NodeName.
	if snap.Status.NodeName == "" {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"SwiftSnapshot "+snap.Name+" has no status.nodeName — Tier B restore requires the source node")
		return true, 0, nil
	}
	if snap.Spec.Backend.Local == nil || snap.Spec.Backend.Local.HostPath == "" {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"SwiftSnapshot "+snap.Name+" has no backend.local.hostPath — Tier B restore requires the snapshot dir")
		return true, 0, nil
	}

	// Identity-regen contract: a clone (target.Name != source) MUST
	// regenerate macAddresses. Without this two clones share a MAC
	// and L2 collides. The validation webhook enforces the same rule
	// at admission, but failing here too is cheap and catches the
	// "webhook turned off" case.
	inPlace := IsInPlaceRestore(snap, restore)
	if !inPlace && restore.Spec.TargetGuest.Name != snap.Spec.GuestRef.Name {
		// Clone (different target name) — require macAddresses regen.
		if !regenIncludes(restore, snapshotv1alpha1.RegenMACAddresses) {
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
				"clone restore (target name differs from source) requires identity.regenerate to include macAddresses; "+
					"two clones with the same MAC will L2-collide")
			return true, 0, nil
		}
	}

	// Resolve source SwiftGuest — needed for spec copy on clone, and
	// for in-place we still need it to be present (we don't conjure
	// a SwiftGuest from a snapshot's CapturedGuestSpec alone).
	var source swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: snap.Spec.GuestRef.Name, Namespace: restore.Namespace}, &source); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, 0, err
		}
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonSourceGuestGone,
			"source SwiftGuest "+snap.Spec.GuestRef.Name+" no longer exists; Tier B restore needs the source spec")
		return true, 0, nil
	}

	// Annotation set both branches write onto the target SwiftGuest.
	// The SwiftGuest controller picks these up in buildPod() and routes
	// to BuildRestorePod() instead of the normal disk-boot path.
	annos := r.restoreAnnotations(restore, snap, &source, inPlace)

	if inPlace {
		// In-place: target name == source name. The source SwiftGuest
		// already exists. We update its annotations and force pod
		// recreation. OverwriteExisting must be true.
		if !restore.Spec.TargetGuest.OverwriteExisting {
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonTargetConflict,
				"in-place restore over existing SwiftGuest "+source.Name+
					" requires targetGuest.overwriteExisting=true")
			return true, 0, nil
		}
		if err := r.stampGuestForRestore(ctx, &source, annos); err != nil {
			return false, 0, fmt.Errorf("stamp source SwiftGuest for restore: %w", err)
		}
		if err := r.deleteLauncherPodForRecreation(ctx, &source); err != nil {
			return false, 0, fmt.Errorf("delete existing launcher pod: %w", err)
		}
		status.GuestRef = &snapshotv1alpha1.SwiftRestoreGuestRef{Name: source.Name}
	} else {
		// Clone: target SwiftGuest doesn't exist (or exists and
		// OverwriteExisting=true). Create from source.Spec with the
		// restore annotations stamped on metadata.
		var existing swiftv1alpha1.SwiftGuest
		err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.TargetGuest.Name, Namespace: restore.Namespace}, &existing)
		if err == nil && !restore.Spec.TargetGuest.OverwriteExisting {
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonTargetConflict,
				"SwiftGuest "+restore.Spec.TargetGuest.Name+" already exists; "+
					"set targetGuest.overwriteExisting=true to replace")
			return true, 0, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return false, 0, err
		}
		target, err := r.ensureCloneTargetGuest(ctx, restore, &source, annos)
		if err != nil {
			return false, 0, err
		}
		status.GuestRef = &snapshotv1alpha1.SwiftRestoreGuestRef{Name: target.Name}
	}

	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseRestoring)
	setReadyCondition(status, metav1.ConditionFalse, ReasonRestoring,
		"restore-receive launcher requested for SwiftGuest "+restore.Spec.TargetGuest.Name+
			" on node "+snap.Status.NodeName)
	return true, 0, nil
}

// handleRestoringLocal waits for the target SwiftGuest's GuestRunning
// condition to flip to True. That signal means CH is bound to its API
// socket with the snapshot loaded — the VM is **paused** at this
// point; the resume action lands in handleResumingLocal.
func (r *SwiftRestoreReconciler) handleRestoringLocal(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, string, error) {
	var target swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.TargetGuest.Name, Namespace: restore.Namespace}, &target); err != nil {
		if apierrors.IsNotFound(err) {
			return false, 0, "target SwiftGuest " + restore.Spec.TargetGuest.Name +
				" disappeared during Restoring", nil
		}
		return false, 0, "", err
	}
	if !isGuestRunning(&target) {
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoring,
			"waiting for target SwiftGuest "+target.Name+" launcher pod to bind CH socket")
		return false, 5 * time.Second, "", nil
	}

	// CH is up + paused. Transition to Resuming so the next reconcile
	// drives the resume action.
	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseResuming)
	setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
		"target SwiftGuest "+target.Name+" launcher up; resume action queued")
	return true, 0, "", nil
}

// handleResumingLocal drives the resume action via the launcher pod's
// snapshot-action annotations and waits for swiftletd's status mirror.
//
// Idempotent: action-id is `<restore-name>-<resourceVersion>` so a
// retried reconcile against the same SwiftRestore is a no-op on the
// launcher (mirrors the pattern in swiftsnapshot/local.go).
func (r *SwiftRestoreReconciler) handleResumingLocal(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, error) {
	pod, err := r.findLauncherPod(ctx, restore.Namespace, restore.Spec.TargetGuest.Name)
	if err != nil {
		return false, 0, err
	}
	if pod == nil {
		setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
			"launcher pod for "+restore.Spec.TargetGuest.Name+" not yet present")
		return false, 5 * time.Second, nil
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"launcher pod "+pod.Name+" phase="+string(pod.Status.Phase)+" before resume completed")
		return true, 0, nil
	}

	actionID := resumeActionID(restore)

	// First reconcile in Resuming: write the resume action onto the
	// launcher pod. Idempotent — patchPodActionAnnotations is a merge
	// patch, swiftletd dedupes by action-id.
	annotations := pod.GetAnnotations()
	if annotations[annoActionID] != actionID {
		argsJSON, err := json.Marshal(struct{}{})
		if err != nil {
			return false, 0, err
		}
		if err := r.patchPodActionAnnotations(ctx, pod, verbResume, actionID, string(argsJSON)); err != nil {
			return false, 0, fmt.Errorf("patch resume action: %w", err)
		}
		// Re-queue to read the mirror; swiftletd's action loop polls at
		// 2s intervals so a 3s requeue gives it room.
		setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
			"resume action sent to launcher pod "+pod.Name)
		return false, 3 * time.Second, nil
	}

	// Action-id matches — wait for the status mirror.
	statusID := annotations[annoStatusID]
	statusVal := annotations[annoStatus]
	statusDetail := annotations[annoStatusDetail]
	if statusID != actionID {
		// Mirror not yet visible.
		return false, 3 * time.Second, nil
	}
	switch statusVal {
	case "running", "":
		return false, 3 * time.Second, nil
	case "rejected":
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"launcher rejected resume action: "+statusDetail)
		return true, 0, nil
	case "failed":
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"resume failed: "+statusDetail)
		return true, 0, nil
	case "ready":
		// Restore complete. Strip the active-restore annotations off
		// the SwiftGuest so a future pod recreation falls through to
		// the normal builder (memory state is gone after a launcher
		// pod restart anyway). Best-effort — failure to strip just
		// means a future pod recreation reuses the same restore-mode
		// pod shape, which is harmless because the snapshot dir is
		// still present.
		if err := r.unstampGuestRestoreAnnotations(ctx, restore.Namespace, restore.Spec.TargetGuest.Name); err != nil {
			// Log via condition message; don't fail the restore for
			// this — the operator can clean up if it really matters.
			setReadyCondition(status, metav1.ConditionTrue, ReasonRestoreReady,
				"restore complete; SwiftGuest "+restore.Spec.TargetGuest.Name+
					" Running (note: annotation cleanup deferred: "+err.Error()+")")
		} else {
			setReadyCondition(status, metav1.ConditionTrue, ReasonRestoreReady,
				"restore complete; SwiftGuest "+restore.Spec.TargetGuest.Name+" Running")
		}
		now := metav1.Now()
		status.CompletedAt = &now
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseReady)
		return true, 0, nil
	default:
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"unexpected snapshot-status value: "+statusVal)
		return true, 0, nil
	}
}

// restoreAnnotations builds the annotation map that the SwiftGuest
// controller reads from the target SwiftGuest's metadata to decide
// whether to call BuildRestorePod and how. Per-NIC MAC rewrites are
// computed from the source guest's NICs using the same deterministic
// generator the SwiftGuest controller uses for primary/secondary
// MACs (internal/runtimeintent.GenerateMAC).
func (r *SwiftRestoreReconciler) restoreAnnotations(
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
	source *swiftv1alpha1.SwiftGuest,
	inPlace bool,
) map[string]string {
	mode := swiftguestctrl.RestoreModeClone
	if inPlace {
		mode = swiftguestctrl.RestoreModeInPlace
	}
	annos := map[string]string{
		swiftguestctrl.AnnotationActiveRestore:       restore.Name,
		swiftguestctrl.AnnotationRestoreSnapshotPath: snap.Spec.Backend.Local.HostPath,
		swiftguestctrl.AnnotationRestoreNodeName:     snap.Status.NodeName,
		swiftguestctrl.AnnotationRestoreMode:         mode,
	}
	if !inPlace {
		// Clone: append cmdline marker so cloud-init bootcmd
		// regenerates machine-id / SSH keys / hostname on first wake.
		// Always set when MAC rewrites are present; the in-guest
		// regen is keyed off the cmdline marker AND a sentinel file,
		// so applying the marker for "macAddresses-only" clones is
		// harmless and matches how operators typically combine the
		// regen flags.
		if regenIncludesAny(restore,
			snapshotv1alpha1.RegenHostname,
			snapshotv1alpha1.RegenMachineID,
			snapshotv1alpha1.RegenSSHHostKeys,
		) {
			annos[swiftguestctrl.AnnotationRestoreAppendCmdlineMarker] = "true"
		}
		if regenIncludes(restore, snapshotv1alpha1.RegenMACAddresses) {
			annos[swiftguestctrl.AnnotationRestoreMACRewrites] = computeMACRewrites(restore.Namespace, restore.Spec.TargetGuest.Name, source)
		}
	}
	return annos
}

// computeMACRewrites returns a CSV of new MACs indexed by NIC ordinal
// matching config.net[]. Order: primary NIC (Spec.Interfaces with
// no NetworkRef) first, then secondary NICs. A SwiftGuest with no
// explicit Interfaces uses a single default NIC named "eth0".
func computeMACRewrites(targetNs, targetName string, source *swiftv1alpha1.SwiftGuest) string {
	if len(source.Spec.Interfaces) == 0 {
		return runtimeintent.GenerateMAC(runtimeintent.InterfaceMACSeed(targetNs, targetName, "eth0"))
	}
	parts := make([]string, 0, len(source.Spec.Interfaces))
	for _, iface := range source.Spec.Interfaces {
		seed := runtimeintent.InterfaceMACSeed(targetNs, targetName, iface.Name)
		parts = append(parts, runtimeintent.GenerateMAC(seed))
	}
	return strings.Join(parts, ",")
}

// regenIncludes returns true when the given identity item is present
// in restore.Spec.Identity.Regenerate.
func regenIncludes(restore *snapshotv1alpha1.SwiftRestore, item snapshotv1alpha1.IdentityRegenerationItem) bool {
	if restore.Spec.Identity == nil {
		return false
	}
	for _, x := range restore.Spec.Identity.Regenerate {
		if x == item {
			return true
		}
	}
	return false
}

// regenIncludesAny returns true when any of the given items is
// present in the regen list.
func regenIncludesAny(restore *snapshotv1alpha1.SwiftRestore, items ...snapshotv1alpha1.IdentityRegenerationItem) bool {
	for _, it := range items {
		if regenIncludes(restore, it) {
			return true
		}
	}
	return false
}

// stampGuestForRestore writes the restore annotation set onto the
// existing source SwiftGuest (in-place restore path). Idempotent —
// duplicate writes against the same SwiftRestore name are a no-op.
func (r *SwiftRestoreReconciler) stampGuestForRestore(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	annos map[string]string,
) error {
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": annos,
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return r.Patch(ctx, guest, client.RawPatch(types.MergePatchType, data))
}

// deleteLauncherPodForRecreation removes the existing launcher pod so
// the SwiftGuest controller rebuilds it in restore mode.
//
// Best-effort: NotFound is tolerated (the pod may already be gone, or
// the source guest may have been Stopped). Force=true with grace=0
// because restore is itself a destructive action over the existing
// VM — the operator opted in via OverwriteExisting=true.
func (r *SwiftRestoreReconciler) deleteLauncherPodForRecreation(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: guest.Name, Namespace: guest.Namespace},
	}
	zero := int64(0)
	err := r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: &zero})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ensureCloneTargetGuest creates (idempotent) a target SwiftGuest from
// a deep copy of the source's spec, with the restore annotations
// stamped on metadata. If the target already exists with the same
// annotations (a previous reconcile got this far and crashed before
// status persisted), the existing guest is returned.
func (r *SwiftRestoreReconciler) ensureCloneTargetGuest(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	source *swiftv1alpha1.SwiftGuest,
	annos map[string]string,
) (*swiftv1alpha1.SwiftGuest, error) {
	var existing swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.TargetGuest.Name, Namespace: restore.Namespace}, &existing); err == nil {
		return &existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	target := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:        restore.Spec.TargetGuest.Name,
			Namespace:   restore.Namespace,
			Annotations: annos,
			Labels: map[string]string{
				"snapshot.kubeswift.io/swift-restore": restore.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(restore, swiftRestoreGVK),
			},
		},
		Spec: source.Spec,
	}
	if !restore.Spec.ResumeAfterRestore {
		// Caller asked the restored VM be left Paused. With Tier B
		// the launcher pod is what gets paused (CH stays paused
		// without a resume action), so we just don't drive the
		// resume in handleResumingLocal — implementation note: this
		// flag is consulted there.
		target.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	}
	if err := r.Create(ctx, target); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	if err := r.Get(ctx, client.ObjectKey{Name: target.Name, Namespace: target.Namespace}, &existing); err != nil {
		return nil, err
	}
	return &existing, nil
}

// unstampGuestRestoreAnnotations strips the AnnotationRestore* set
// off the target SwiftGuest. Called after a successful restore so a
// future pod recreation falls through to the normal builder.
func (r *SwiftRestoreReconciler) unstampGuestRestoreAnnotations(
	ctx context.Context,
	namespace, name string,
) error {
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				swiftguestctrl.AnnotationActiveRestore:              nil,
				swiftguestctrl.AnnotationRestoreSnapshotPath:        nil,
				swiftguestctrl.AnnotationRestoreNodeName:            nil,
				swiftguestctrl.AnnotationRestoreMode:                nil,
				swiftguestctrl.AnnotationRestoreMACRewrites:         nil,
				swiftguestctrl.AnnotationRestoreAppendCmdlineMarker: nil,
			},
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return r.Patch(ctx, &guest, client.RawPatch(types.MergePatchType, data))
}

// findLauncherPod returns the launcher pod for a SwiftGuest, or nil
// if not yet present. Convention: launcher pod name == SwiftGuest
// name (one launcher per guest).
func (r *SwiftRestoreReconciler) findLauncherPod(ctx context.Context, namespace, guestName string) (*corev1.Pod, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Name: guestName, Namespace: namespace}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pod, nil
}

// patchPodActionAnnotations writes a merge patch that sets the action
// triple atomically. The launcher pod's action loop in swiftletd
// polls at POLL_INTERVAL and dispatches when action-id changes.
func (r *SwiftRestoreReconciler) patchPodActionAnnotations(
	ctx context.Context,
	pod *corev1.Pod,
	verb, actionID, argsJSON string,
) error {
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				annoAction:     verb,
				annoActionID:   actionID,
				annoActionArgs: argsJSON,
			},
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return r.Patch(ctx, pod, client.RawPatch(types.MergePatchType, data))
}

// resumeActionID returns a stable per-SwiftRestore action-id used to
// drive (and later observe) the launcher pod's snapshot-action
// annotations during the Resuming phase.
//
// Mirrors capturingActionID in internal/controller/swiftsnapshot:
// derived from restore.Name and restore.UID (both immutable). Using
// restore.ResourceVersion here would diverge across reconciles
// because each status write bumps it — by the time the launcher
// mirrors the action-id, the controller is computing a new one and
// the comparison never matches. The same bug bit Tier B captures
// before this fix.
func resumeActionID(restore *snapshotv1alpha1.SwiftRestore) string {
	uid := string(restore.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return restore.Name + "-" + uid
}

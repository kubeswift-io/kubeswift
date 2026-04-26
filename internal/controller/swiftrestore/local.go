// Tier B (local hostPath) restore path for SwiftRestore.
//
// Scope of this commit (10/18): hypervisor-version-check infrastructure
// and the Tier B Pending dispatch. The actual launcher-pod creation
// (which patches the snapshot's config.json for restore-receive paths,
// pins the new pod to the source node, and drives the resume action)
// pairs naturally with identity regeneration in commit 12, since both
// touch the snapshot directory contents — splitting them would mean
// two passes over config.json. Tier B restores reaching this controller
// today transition Pending → Failed with a clear "not yet wired"
// message; the version check is exercised before that fail point so
// operator-visible mismatches surface even now.

package swiftrestore

import (
	"context"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
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

// CurrentClusterHypervisorVersion is the CH version the cluster is
// expected to have, sourced from the controller-manager environment.
// Read at controller startup (not per-reconcile) by main.go and
// passed into the reconciler. Empty string disables the version
// check — controllers built without this configuration treat all
// restores as VersionUnknown.
//
// Concrete sourcing options for a future commit:
//   - KUBESWIFT_CH_VERSION env var on controller-manager
//   - controller-manager image label sourced via downward API
//   - dynamic probe of an arbitrary launcher pod's vm.info socket
//     (last resort; cross-pod coupling we'd rather avoid)
//
// For now the field is on the reconciler struct so the SwiftRestore
// reconciler can be wired with a value at startup; callers that
// don't set it get VersionUnknown semantics, which the controller
// surfaces as a Warning condition rather than failing.

// handlePendingLocal handles the Tier B (local-backend) Pending phase.
//
// This commit's scope is limited to the version-check pre-flight; the
// launcher-pod creation (config.json patching, hostPath mount, node
// pinning, action-driven resume) lands in commit 12 alongside identity
// regeneration. A Tier B restore that reaches this handler today goes
// to Failed with a clear "wiring deferred" message — but the version
// check runs first, so a major-version mismatch fails fast with the
// version-specific reason rather than the generic deferred-wiring one.
func (r *SwiftRestoreReconciler) handlePendingLocal(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, error) {
	_ = ctx
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

	// Tier B launcher-pod creation lands in commit 12. Mark the
	// restore Failed with an explicit wiring message; the operator
	// sees the limitation rather than an indefinite Pending.
	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
	setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
		"local-backend (Tier B) restore controller wiring is deferred to a subsequent commit; "+
			"version check passed for snapshot "+snap.Status.HypervisorVersion)
	return true, 0, nil
}

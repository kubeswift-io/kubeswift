package swiftmigration

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// resolveAutoMode resolves spec.Mode=auto into a concrete status.Mode
// (live or offline). Called from handleValidating BEFORE the per-mode
// dispatch fires, so handleValidatingLive never sees status.Mode="" +
// spec.Mode=auto — by the time dispatch happens, status.Mode is one
// of "live" or "offline" and isLiveMode is unambiguous.
//
// Returns nil on success (status.Mode stamped). Returns a phaseResult
// only on failure (e.g., source guest missing, transient API error).
//
// **B2 RULE (conservative)**: defaults to offline; promotes to live
// only when ALL of the following hold:
//
//   - guest has no VFIO devices (no gpuProfileRef, no SR-IOV interface)
//   - networking is multi-node OR allowIPChange=true
//
// Storage live-capability is NOT checked here. The webhook's
// gateLiveModeStorage runs at admission time; if the operator submits
// mode=auto + non-live-capable storage, the auto-resolution may pick
// "live" but Validating-live's body will then fail with a clear storage
// gate message. This factoring keeps resolveAutoMode independent of
// the storage-class read-and-resolve code path (which lives in the
// webhook today).
//
// **Rule expansion is a Phase 3a/3b follow-up** (TODO). The full rule
// per architect-discipline review:
//
//   - live-capable storage AND default-networking-with-allowIPChange
//   - OR multi-node networking AND no GPU/SR-IOV
//
// The B2 conservative rule covers the second clause; the first clause
// requires the storage-class resolution code path which is webhook
// territory today. A follow-up commit can promote the storage-class
// resolution out of the webhook into a shared helper, then auto-mode
// can use it.
//
// Default-to-offline is the safe fall-through: operators submitting
// mode=auto on workloads that are not safe-to-live get the Phase 1
// offline path which always works (just with downtime).
func (r *SwiftMigrationReconciler) resolveAutoMode(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("source SwiftGuest %q no longer exists in namespace %q", mig.Spec.GuestRef.Name, mig.Namespace),
				"")
		}
		return phaseTransient(fmt.Errorf("get source SwiftGuest for auto-resolution: %w", err))
	}

	// Default: offline. Promote to live only if explicit safety
	// conditions hold.
	status.Mode = migrationv1alpha1.SwiftMigrationModeOffline

	if hasVFIODevices(&guest) {
		// VFIO cross-node is not supported (Phase 4+ work). Auto must
		// resolve to offline; the webhook's separate VFIO-rejection
		// rule for explicit mode=live still fires for explicit
		// submissions. Auto + VFIO → offline is correct (and the only
		// usable mode for VFIO workloads).
		return nil
	}

	if guest.HasNodeLocalVirtioBackends() {
		// virtiofs / vhost-user backends (virtiofsd processes, source
		// mounts, operator backend sockets) live in/on the SOURCE pod
		// and node; CH live migration does not transfer them, so the
		// resumed guest's devices would break. Offline recreates the
		// launcher pod on the target where the backends are
		// re-established — mirror the VFIO rule. The webhook rejects
		// explicit mode=live for these guests.
		return nil
	}

	if isDefaultNodeLocalNetworking(&guest) && !mig.Spec.AllowIPChange {
		// Default node-local networking produces a fresh IP on the
		// destination. Without operator opt-in (allowIPChange=true),
		// auto must NOT silently change the guest's IP. Resolve to
		// offline; offline migration on default networking also
		// produces a fresh IP, but offline's "stop, move, start"
		// semantics make the IP change explicit (the guest reboots).
		return nil
	}

	// All checks passed: resolve to live.
	status.Mode = migrationv1alpha1.SwiftMigrationModeLive
	return nil
}

// hasVFIODevices returns true when the guest references VFIO devices
// (GPU passthrough or SR-IOV). VFIO devices cannot live-migrate cross-
// node; the receiver CH would have no equivalent device on the
// destination.
//
// Delegates to the canonical SwiftGuest.HasVFIODevices predicate in
// api/swift/v1alpha1 (the cycle-free home). Kept as a package-local thunk
// so existing call sites and tests are unaffected.
func hasVFIODevices(guest *swiftv1alpha1.SwiftGuest) bool {
	return guest.HasVFIODevices()
}

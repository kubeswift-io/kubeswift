package swiftmigration

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
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
//
//   - networking is multi-node OR allowIPChange=true
//
//   - storage is live-migration-capable (resolved.LiveMigrationStorageError:
//     kernel-boot, or RWX+Block root storage)
//
// Storage IS checked here. It used not to be — on the assumption that
// Validating-live would reject incapable storage, which it never did — so
// every drain migration (always mode=auto) of a default disk-boot guest
// (RWO/Filesystem) resolved to live, its destination pod hit Multi-Attach
// and failed DstNeverReady, and the drain stayed blocked instead of taking
// the offline path the docs promise.
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

	// Model A: a guest on the namespace primary OVN-K UDN cannot live-migrate in v1
	// (the primary UDN withholds the swiftletd<->swiftletd migration channel from the
	// pod — the dst eth0 is infrastructure-locked, dropping pod-to-pod traffic). Auto
	// resolves to offline, which works: the target acquires a fresh UDN IP. The webhook
	// rejects explicit mode=live for these guests. Mirrors the VFIO rule above.
	if modelA, err := resolved.NamespaceHasPrimaryUDN(ctx, r.Client, mig.Namespace); err != nil {
		return phaseTransient(fmt.Errorf("checking primary-UDN namespace for auto-resolution: %w", err))
	} else if modelA {
		return nil
	}

	// Storage must be live-capable. A missing SwiftGuestClass also resolves
	// offline (the safe default); resolution surfaces the missing class.
	gate, classFound, err := r.liveStorageGate(ctx, &guest)
	if err != nil {
		return phaseTransient(fmt.Errorf("checking storage live-capability for auto-resolution: %w", err))
	}
	if gate != nil || !classFound {
		return nil
	}

	// All checks passed: resolve to live.
	status.Mode = migrationv1alpha1.SwiftMigrationModeLive
	return nil
}

// liveStorageGate applies the shared live-migration storage rule
// (resolved.LiveMigrationStorageError) to the guest. gate is non-nil when the
// storage cannot be live-migrated, carrying the reason; err is a transient
// read failure. classFound is false when the SwiftGuestClass does not exist —
// a resolution problem reported elsewhere, which callers treat on their own
// terms (auto resolves offline; the Validating-live gate defers to resolution,
// mirroring the webhook).
func (r *SwiftMigrationReconciler) liveStorageGate(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
) (gate error, classFound bool, err error) {
	if guest.Spec.KernelRef != nil {
		return nil, true, nil
	}
	var class swiftv1alpha1.SwiftGuestClass
	if err := r.Get(ctx, client.ObjectKey{Name: guest.Spec.GuestClassRef.Name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return resolved.LiveMigrationStorageError(guest, &class), true, nil
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

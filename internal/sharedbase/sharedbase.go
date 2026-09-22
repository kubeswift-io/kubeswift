// Package sharedbase answers one question — does this guest's root disk live in
// a node-local dm-thin pool rather than a PVC — for the validators and
// controllers that have to refuse the features that answer rules out.
//
// It exists as its own package because the rule is one rule and must not drift
// into three slightly different copies. A guest on a shared base has no PVC
// behind its root disk, so:
//
//   - Tier A (CSI VolumeSnapshot) has nothing to snapshot;
//   - live migration has no shared storage to migrate over;
//   - nothing replicates the disk, so losing the node loses the guest.
//
// The reasoning is recorded in docs/design/shared-base-root-disk.md §7.2, §7.4
// and §7.6.
package sharedbase

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// GuestUsesSharedBase reports whether the named SwiftGuest's SwiftGuestClass
// opts into a shared base root disk, and which class decided it.
//
// The class name comes back because every refusal message names it: the setting
// lives on the class, not the guest, so an operator told only "this guest
// cannot" has to go looking for where that was decided.
//
// A guest or class that cannot be found reports false rather than an error.
// This is a guard on top of validation that already exists elsewhere: a
// snapshot naming a guest that is not there is rejected for THAT reason, with a
// message about the missing guest, and turning it into "shared base could not
// be determined" would replace a clear error with a confusing one.
//
// Real API failures are returned. A caller must not treat an error as "not
// shared" — that would let exactly the combination this refuses through on a
// transient read failure.
func GuestUsesSharedBase(ctx context.Context, c client.Client, namespace, guestName string) (shared bool, className string, err error) {
	if c == nil || guestName == "" {
		return false, "", nil
	}
	var guest swiftv1alpha1.SwiftGuest
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: guestName}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("getting SwiftGuest %s/%s: %w", namespace, guestName, err)
	}
	name := guest.Spec.GuestClassRef.Name
	shared, err = ClassUsesSharedBase(ctx, c, name)
	return shared, name, err
}

// ClassUsesSharedBase reports whether a SwiftGuestClass opts in.
//
// SwiftGuestClass is CLUSTER-scoped, so this takes no namespace. Passing one
// would be silently wrong rather than an error, which is how a guard ends up
// looking correct and never firing.
func ClassUsesSharedBase(ctx context.Context, c client.Client, className string) (bool, error) {
	if c == nil || className == "" {
		return false, nil
	}
	var class swiftv1alpha1.SwiftGuestClass
	if err := c.Get(ctx, client.ObjectKey{Name: className}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting SwiftGuestClass %q: %w", className, err)
	}
	return class.Spec.SharedBaseDisk, nil
}

// CSISnapshotRefusal is the message for a Tier A snapshot of a shared-base
// guest.
//
// Every refusal here names the guest, the class, and a way forward that
// ACTUALLY WORKS. An earlier version of these messages pointed at offline
// migration and at includeDisk snapshots, neither of which does anything useful
// for a node-local disk; a refusal that recommends another dead end is worse
// than a bare "no", because the operator follows it.
func CSISnapshotRefusal(guestName, className string) string {
	return fmt.Sprintf(
		"guest %q uses SwiftGuestClass %q with sharedBaseDisk: true, so its root disk is a "+
			"thin snapshot in a node-local pool and has no PersistentVolumeClaim for a CSI "+
			"VolumeSnapshot to capture. Memory snapshots still work: backend.type local, or s3/oci "+
			"without includeDisk. Set sharedBaseDisk: false on the class if this guest needs CSI "+
			"snapshots",
		guestName, className)
}

// IncludeDiskRefusal is the message for a full-state (includeDisk) snapshot of
// a shared-base guest.
//
// The chunk Job that exports the disk reads the guest's root PVC, and a
// shared-base guest has none. The design allows for it — the guest's thin
// device reads back as a complete merged disk, so exporting it needs only the
// Job pinned to the guest's node and handed that device — but that is not built,
// and until it is this must be refused rather than left to fail inside a Job
// looking for a PVC that was never there.
func IncludeDiskRefusal(guestName, className string) string {
	return fmt.Sprintf(
		"guest %q uses SwiftGuestClass %q with sharedBaseDisk: true; full-state snapshots "+
			"(includeDisk) are not supported for shared-base guests yet, because the disk export "+
			"reads a root PVC these guests do not have. A memory snapshot (includeDisk: false) "+
			"works. Set sharedBaseDisk: false on the class if this guest needs full-state snapshots",
		guestName, className)
}

// MigrationRefusal is the message for ANY SwiftMigration of a shared-base
// guest, whatever its mode.
//
// Not only live. Offline migration stops the guest, sets spec.nodeName to the
// target and starts it again: it copies nothing and relies on the disk being on
// shared storage. A shared-base disk is node-local, so on the target the guest
// would either be refused (it is pinned to the node holding its disk) or, were
// the pin ever missing, be handed a fresh snapshot of the base — a pristine
// disk, every write gone, booting as if new. There is no mode that moves it.
func MigrationRefusal(guestName, className string) string {
	return fmt.Sprintf(
		"guest %q uses SwiftGuestClass %q with sharedBaseDisk: true, so its root disk is "+
			"node-local and cannot be migrated in any mode: live migration has no shared storage to "+
			"move over, and offline migration only repins the guest, which would leave its disk "+
			"behind. Recreate the guest on the target node instead, or set sharedBaseDisk: false on "+
			"the class if this guest needs to move",
		guestName, className)
}

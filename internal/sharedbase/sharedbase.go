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
// Every refusal here names the guest, the class, and the way out, because the
// operator's next question is always "then how do I snapshot this". Refusing
// without an alternative is how a guard gets worked around.
func CSISnapshotRefusal(guestName, className string) string {
	return fmt.Sprintf(
		"guest %q uses SwiftGuestClass %q with sharedBaseDisk: true, so its root disk is a "+
			"thin snapshot in a node-local pool and has no PersistentVolumeClaim for a CSI "+
			"VolumeSnapshot to capture. Use backend.type local (Tier B) or s3/oci (Tier C), "+
			"which capture through the running guest and are unaffected; or set "+
			"sharedBaseDisk: false on the class if this guest needs CSI snapshots",
		guestName, className)
}

// LiveMigrationRefusal is the message for a live migration of a shared-base
// guest.
func LiveMigrationRefusal(guestName, className string) string {
	return fmt.Sprintf(
		"guest %q uses SwiftGuestClass %q with sharedBaseDisk: true, so its root disk is "+
			"node-local and live migration has no shared storage to move over. Use mode "+
			"offline, which works unchanged; or set sharedBaseDisk: false on the class if "+
			"this guest needs live migration",
		guestName, className)
}

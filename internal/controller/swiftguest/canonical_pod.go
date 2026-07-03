package swiftguest

import (
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// canonicalPodName returns the launcher pod name to look up for this
// SwiftGuest. Pre-migration (and Phase 1 offline migration), the pod
// name equals guest.Name and this helper is a no-op pass-through.
// Post-cutover for Phase 3a live migration, status.PodRef.Name is set
// by the SwiftMigration controller's cutover step 1 to the destination
// launcher pod's name (`<guest>-mig-<short-uid>`); this helper returns
// that value so the SwiftGuest controller's pod lookups find the
// migrated pod rather than NotFound on the now-deleted source pod.
//
// Phase 3a PR 1 minimum-viable scope: this helper lands at the
// SwiftGuest reconcile loop's three pod-Get sites in controller.go,
// the minimum surface required for live-mode cluster integration
// testing. PR 2 expands the resolution to swiftctl logs/console/ssh and
// adds a comprehensive call-site audit + edge cases (defense-in-
// depth label/ownerRef verification per security-engineer review).
//
// Phase 1 offline migration: status.PodRef may be set by the
// existing SetPodRef status helper after the launcher pod is
// scheduled, but its Name field always equals guest.Name (offline
// reuses the same pod name). The helper returns guest.Name in that
// case, preserving Phase 1 behavior unchanged. The fallback to
// guest.Name when PodRef is nil or has empty Name covers fresh
// guests pre-scheduling.
func canonicalPodName(guest *swiftv1alpha1.SwiftGuest) string {
	if guest.Status.PodRef != nil && guest.Status.PodRef.Name != "" {
		return guest.Status.PodRef.Name
	}
	return guest.Name
}

// staleMigrationPodRef reports whether status.PodRef points at a
// launcher pod other than the canonical guest.Name pod — i.e. a
// migration-renamed pod (<guest>-mig-<uid>) left over from a prior live
// migration's cutover. It is equivalent to
// canonicalPodName(guest) != guest.Name, named for intent.
//
// Why it matters: the controller always (re)creates the launcher pod as
// guest.Name (see pod.go), but looks it up via canonicalPodName. After a
// live migration, status.PodRef.Name is the <guest>-mig-<uid> dst pod.
// Once that pod is gone — the guest was offline-migrated (its renamed
// pod deleted in the migration's Preparing phase), or the launcher pod
// was lost to node failure/eviction — and the controller must recreate
// the pod, the canonical lookup misses the (deleted) renamed name and
// the create path makes guest.Name. Unless the stale PodRef is cleared,
// the next reconcile's canonicalPodName still resolves to the deleted
// name → NotFound → Create(guest.Name) → AlreadyExists, looping forever
// and never reaching MapPodToStatus to self-correct. Clearing the stale
// PodRef on the create path lets canonicalPodName fall back to
// guest.Name and find the recreated pod (TFU #18 secondary trap).
//
// This never fires during a healthy live migration: the dst <guest>-mig-
// pod exists while status.PodRef points at it, so the canonical lookup
// finds it and takes the update branch, never the create branch.
func staleMigrationPodRef(guest *swiftv1alpha1.SwiftGuest) bool {
	return guest.Status.PodRef != nil &&
		guest.Status.PodRef.Name != "" &&
		guest.Status.PodRef.Name != guest.Name
}

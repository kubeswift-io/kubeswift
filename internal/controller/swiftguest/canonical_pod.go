package swiftguest

import (
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
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
// testing. Per docs/design/live-migration-phase-3a.md §3.3,
// PR 2 expands the resolution to swiftctl logs/console/ssh and
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

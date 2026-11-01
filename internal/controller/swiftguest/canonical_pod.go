package swiftguest

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// LauncherPod returns the guest's launcher pod, or nil when it has none: the
// pod status.podRef names (after a live migration, the renamed destination
// pod), else the pod named like the guest, and only if the guest controls it.
// Other controllers that act on a guest's launcher (snapshot, restore) find it
// this way. Looking it up by the guest's name alone missed a migrated guest's
// launcher, so a snapshot of it waited forever (#824), and could take a pod
// left by a previous guest of the same name for this one's (#819).
func LauncherPod(ctx context.Context, c client.Reader, guest *swiftv1alpha1.SwiftGuest) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: canonicalPodName(guest)}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !metav1.IsControlledBy(&pod, guest) {
		return nil, nil
	}
	return &pod, nil
}

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
	// Only once launcherRefStale has confirmed, uncached, that status.PodRef
	// is the guest's current reference: on a guest read from a cache that
	// predates a cutover, a missing PodRef pod is the cutover's deleted
	// source, not a lost launcher.
	return guest.Status.PodRef != nil &&
		guest.Status.PodRef.Name != "" &&
		guest.Status.PodRef.Name != guest.Name
}

// liveGuest reads the guest uncached, for the checks made just before a
// launcher is created. Without an APIReader it returns the guest as given; nil
// when the guest is gone.
func (r *SwiftGuestReconciler) liveGuest(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*swiftv1alpha1.SwiftGuest, error) {
	if r.APIReader == nil {
		return guest, nil
	}
	var live swiftv1alpha1.SwiftGuest
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(guest), &live); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return &live, nil
}

// launcherRefStale reports whether the guest's launcher reference
// (canonicalPodName) has moved on the API server (live) since the copy being
// reconciled was cached.
func launcherRefStale(guest, live *swiftv1alpha1.SwiftGuest) bool {
	return canonicalPodName(live) != canonicalPodName(guest)
}

// offlineMigrationHoldRetry is how often a guest held by an offline migration
// looks again. The migration's cutover patch and its cleanup on failure both
// write the guest, which wakes it sooner.
const offlineMigrationHoldRetry = 5 * time.Second

// offlineMigrationHold returns the offline SwiftMigration that holds the guest
// before its cutover, or "". Such a migration claimed the guest (the
// migration-in-progress marker, runPolicy Stopped), deleted the launcher, and
// starts the guest on the target itself, with one patch of runPolicy Running and
// spec.nodeName. Until that patch, a runPolicy Running written by someone else
// (a GitOps controller correcting drift, a kubectl apply) must not bring the
// launcher back on the source: it would hold the disk the migration is moving
// and fail the migration with the guest left pinned to a node it is not on
// (#899). live is the guest read uncached, so a cache older than the claim
// cannot miss the marker.
func (r *SwiftGuestReconciler) offlineMigrationHold(ctx context.Context, live *swiftv1alpha1.SwiftGuest) (string, error) {
	name := live.Annotations[migrationv1alpha1.AnnotationMigrationInProgress]
	if name == "" {
		return "", nil
	}
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var mig migrationv1alpha1.SwiftMigration
	if err := reader.Get(ctx, client.ObjectKey{Namespace: live.Namespace, Name: name}, &mig); err != nil {
		// A marker without its migration holds nothing.
		return "", client.IgnoreNotFound(err)
	}
	switch mig.Status.Phase {
	case migrationv1alpha1.SwiftMigrationPhaseCompleted,
		migrationv1alpha1.SwiftMigrationPhaseFailed,
		migrationv1alpha1.SwiftMigrationPhaseCancelled:
		return "", nil
	}
	if target := mig.Spec.Target.NodeName; target != "" && live.Spec.NodeName == target {
		return "", nil // cut over: the launcher is the destination's
	}
	return name, nil
}

// Package swiftdrain implements the Phase 4 drain controller: the "controller
// creates" half of the webhook-marks / controller-creates / PDB-guarantees
// architecture (design doc §3).
//
// The eviction webhook (internal/webhook/eviction) stamps
// kubeswift.io/drain-requested:<node> on a SwiftGuest when an eviction of its
// launcher pod is intercepted and the guest is auto-migratable. This
// controller watches SwiftGuests and, for a marked guest still on the
// draining node, creates a SwiftMigration (deterministic name, mode resolved
// from drainPolicy, target node chosen by capacity) to evacuate it. Once the
// guest is off the draining node, it clears the marker; the next eviction
// retry then finds the pod gone (or moved) and is allowed, so drain proceeds.
//
// VFIO/GPU guests are NOT handled here: cross-node migration of VFIO devices
// needs a release-and-reallocate primitive that does not exist yet (tracked
// follow-up). The eviction webhook denies those without marking; this
// controller guards defensively and surfaces a manual-handling event if a
// VFIO guest is somehow marked.
package swiftdrain

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// Reconciler drives drain-requested SwiftGuests to a SwiftMigration.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile implements the drain state machine for one SwiftGuest.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, req.NamespacedName, &guest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if guest.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	drainingNode := guest.Annotations[swiftv1alpha1.AnnotationDrainRequested]
	if drainingNode == "" {
		return ctrl.Result{}, nil // no drain in progress for this guest
	}

	// Guest already off the draining node → drain achieved; clear the marker.
	if guest.Status.NodeName != "" && guest.Status.NodeName != drainingNode {
		if err := r.clearMarker(ctx, &guest); err != nil {
			return ctrl.Result{}, err
		}
		r.event(&guest, corev1.EventTypeNormal, "DrainComplete",
			fmt.Sprintf("guest evacuated from %q to %q; drain marker cleared", drainingNode, guest.Status.NodeName))
		return ctrl.Result{}, nil
	}

	// VFIO/GPU guests cannot be auto-migrated yet (release-and-reallocate
	// pending). The eviction webhook should not have marked them; guard
	// defensively and surface a manual-handling event rather than create a
	// migration the SwiftMigration webhook would reject.
	if guest.HasVFIODevices() {
		r.event(&guest, corev1.EventTypeWarning, "DrainUnsupported",
			fmt.Sprintf("guest on node %q uses VFIO/GPU devices; automatic evacuation is not supported yet (release-and-reallocate pending) — handle manually", drainingNode))
		return ctrl.Result{}, nil
	}

	migName := drainMigrationName(guest.Name, drainingNode)
	var mig migrationv1alpha1.SwiftMigration
	getErr := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: migName}, &mig)
	if getErr == nil {
		return r.observeMigration(ctx, &guest, &mig, drainingNode)
	}
	if !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, fmt.Errorf("get drain migration %q: %w", migName, getErr)
	}

	// No drain migration yet. If some OTHER migration is already in flight for
	// this guest (operator-initiated, or a prior drain of a different node),
	// don't create a second — wait for it to settle.
	if inflight := guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress]; inflight != "" {
		log.Info("drain deferred: another migration is in flight for this guest",
			"guest", guest.Name, "migration", inflight)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	target, err := r.selectTarget(ctx, &guest, drainingNode)
	if err != nil {
		r.event(&guest, corev1.EventTypeWarning, "DrainNoTarget",
			fmt.Sprintf("cannot evacuate %q from %q: %v — drain stalls safely until capacity frees up", guest.Name, drainingNode, err))
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if err := r.createDrainMigration(ctx, &guest, migName, target, drainingNode); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// observeMigration reacts to an existing drain migration's phase.
func (r *Reconciler) observeMigration(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, mig *migrationv1alpha1.SwiftMigration, drainingNode string) (ctrl.Result, error) {
	switch mig.Status.Phase {
	case migrationv1alpha1.SwiftMigrationPhaseCompleted:
		// Cutover done; status.NodeName should flip to the target shortly.
		// Requeue so the top-of-reconcile node check clears the marker.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	case migrationv1alpha1.SwiftMigrationPhaseFailed, migrationv1alpha1.SwiftMigrationPhaseCancelled:
		// Don't retry-storm: leave the marker (the eviction webhook keeps
		// denying — the VM is protected, never killed) and surface the
		// failure. Operator deletes the SwiftMigration to retry, or handles
		// the guest manually.
		r.event(guest, corev1.EventTypeWarning, "DrainMigrationFailed",
			fmt.Sprintf("migration %q evacuating %q from %q ended %s: %s — marker left (drain stays blocked); delete the SwiftMigration to retry or handle manually",
				mig.Name, guest.Name, drainingNode, mig.Status.Phase, failureDetail(mig)))
		return ctrl.Result{}, nil
	default:
		// In progress (Pending/Validating/Preparing/StopAndCopy/Resuming).
		// The Owns watch re-triggers reconcile on the migration's status
		// changes; nothing to do now.
		return ctrl.Result{}, nil
	}
}

// createDrainMigration creates the guest-owned SwiftMigration that evacuates
// the guest to target.
func (r *Reconciler) createDrainMigration(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, migName, target, drainingNode string) error {
	mode := drainMode(drainPolicyOf(guest))
	mig := &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migName,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "migration",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
				"kubeswift.io/drain":           "true",
			},
		},
		Spec: migrationv1alpha1.SwiftMigrationSpec{
			GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: guest.Name},
			Target:   migrationv1alpha1.SwiftMigrationTarget{NodeName: target},
			Mode:     mode,
			Reason:   "node-drain",
			// Drain is an operator-initiated evacuation: the guest must move,
			// so opt into the cross-node IP change inherent to default
			// node-local networking. Without this the migration webhook
			// rejects the cross-node move and the drain would stall.
			AllowIPChange: true,
		},
	}
	if err := ctrl.SetControllerReference(guest, mig, r.Scheme); err != nil {
		return fmt.Errorf("set ownerRef on drain migration %q: %w", migName, err)
	}
	if err := r.Create(ctx, mig); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // raced with another reconcile; next loop observes it
		}
		return fmt.Errorf("create drain migration %q: %w", migName, err)
	}
	r.event(guest, corev1.EventTypeNormal, "DrainMigrationCreated",
		fmt.Sprintf("created migration %q (mode %s) to evacuate %q from %q to %q", migName, mode, guest.Name, drainingNode, target))
	return nil
}

// clearMarker removes the drain-requested annotation via a merge patch.
func (r *Reconciler) clearMarker(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) error {
	if _, ok := guest.Annotations[swiftv1alpha1.AnnotationDrainRequested]; !ok {
		return nil
	}
	patch := client.MergeFrom(guest.DeepCopy())
	delete(guest.Annotations, swiftv1alpha1.AnnotationDrainRequested)
	if err := r.Patch(ctx, guest, patch); err != nil {
		return fmt.Errorf("clear drain-requested marker on %q: %w", guest.Name, err)
	}
	return nil
}

func (r *Reconciler) event(obj client.Object, eventtype, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventtype, reason, msg)
	}
}

// failureDetail returns the most specific failure description available.
func failureDetail(mig *migrationv1alpha1.SwiftMigration) string {
	if mig.Status.FailureMessage != "" {
		return mig.Status.FailureMessage
	}
	if mig.Status.FailureReason != "" {
		return mig.Status.FailureReason
	}
	return "no detail reported"
}

// SetupWithManager registers the drain controller. It watches SwiftGuests
// (the marker lives there) and Owns the SwiftMigrations it creates (so a
// migration's status changes re-trigger reconcile of the owning guest).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("swiftdrain").
		For(&swiftv1alpha1.SwiftGuest{}).
		Owns(&migrationv1alpha1.SwiftMigration{}).
		Complete(r)
}

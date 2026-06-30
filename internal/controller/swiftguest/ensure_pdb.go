package swiftguest

import (
	"context"
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// guestPodLabelKey is the launcher-pod label the per-guest PDB selects on.
// Matches the label written by the pod builder (pod.go).
const guestPodLabelKey = "swift.kubeswift.io/guest"

// ensureMigrationPDB creates (idempotently) the per-guest PodDisruptionBudget
// with maxUnavailable: 0 — the Phase 4 "hard floor" that protects the VM from
// voluntary eviction (kubectl drain / the eviction API) EVEN WHEN the eviction
// webhook is down (design §3, §4.2). Universal: every guest with a launcher
// pod gets one.
//
// It selects the guest's launcher pod by the swift.kubeswift.io/guest label
// (which both the steady-state guest.Name pod and a post-migration
// <guest>-mig-<uid> pod carry, so protection follows the guest across a live
// migration), and is owned by the SwiftGuest so it is garbage-collected on
// guest delete.
//
// A maxUnavailable:0 PDB does NOT impede the happy-path drain: the eviction
// webhook denies (429, retry) and the migration's cutover Deletes the source
// pod directly (a Delete, not an Eviction — PDBs gate only the eviction API),
// so the guest moves and drain proceeds. The PDB only bites when the webhook
// is unavailable — exactly the failure mode it backstops.
//
// Called past the pod-ensure block, so the launcher pod exists (§8: "not
// before the launcher pod exists"). The spec is deterministic (selector keyed
// on the immutable guest name, constant maxUnavailable), so this is
// create-if-absent and leaves an existing PDB untouched.
func (r *SwiftGuestReconciler) ensureMigrationPDB(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) error {
	name := guest.Name
	var existing policyv1.PodDisruptionBudget
	err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: name}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get PodDisruptionBudget %q: %w", name, err)
	}

	zero := intstr.FromInt32(0)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "guest",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &zero,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{guestPodLabelKey: guest.Name},
			},
		},
	}
	if err := controllerutil.SetControllerReference(guest, pdb, r.Scheme); err != nil {
		return fmt.Errorf("set ownerRef on PodDisruptionBudget %q: %w", name, err)
	}
	if err := r.Create(ctx, pdb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create PodDisruptionBudget %q: %w", name, err)
	}
	return nil
}

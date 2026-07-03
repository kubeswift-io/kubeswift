package swiftguest

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// Pluggable OVN CNI backends.
//
// When a SwiftGuest's PRIMARY interface rides an OVN-managed NAD, the guest's
// portable IP lives on a real OVN logical switch. OVN binds each logical-switch
// port (LSP) to the POD interface's MAC and answers ARP for the port IP, while
// KubeSwift's datapath bridges the guest's OWN (distinct) hypervisor MAC behind
// the pod NIC (network-init's setup_primary_nad_nic). So unless the LSP identity
// is programmed to BE the guest, OVN delivers the guest's traffic to the wrong MAC
// and the guest is unreachable on the segment. Each OVN-based CNI has its own way
// to program that identity (and its own live-migration IP-overlap contract); the
// ovnBackend interface is the single seam those differences live behind.
//
// The bar is TWO implementations, not a framework (design principle #1): a slice
// of backends, first-match-wins on Detect — no registry, loader, or config. Today
// the slice holds only kubeOVNBackend (shipped #239/#240); ovnKubernetesBackend
// lands in P2. The datapath stays single-sourced
// in network-init.sh (CNI-agnostic); nothing here touches Rust/RuntimeIntent.

// ovnIdentity is what an ovnBackend computes for a guest whose primary rides one
// of its NADs: the per-pod annotations that program the OVN port to BE the guest,
// plus the extra annotations the live-migration DST pod needs to keep the src's IP
// through cutover.
type ovnIdentity struct {
	// PodAnnotations to add to the launcher pod at boot (the OVN port identity:
	// the guest MAC, plus an optional static IP pin once it is known).
	PodAnnotations map[string]string
	// MigrationDstAnnotations to add to the live-migration DST pod so it keeps the
	// src's IP through cutover (kube-ovn: the IP pin + kubevirt.io/migrationJobName,
	// which lets its IPAM skip the conflict check). Empty when the backend needs
	// nothing beyond the carried-over Multus annotation, or has no live-overlap
	// mechanism (the latter would make live migration offline-only — surfaced by the
	// migration webhook gate, never a silent unreachable boot).
	MigrationDstAnnotations map[string]string
	// ClaimsToEnsure are per-guest cluster objects a backend needs created BEFORE
	// the launcher pod references them — e.g. an OVN-Kubernetes IPAMClaim (OVN-K
	// does not auto-create it; spike §8). The stamp dispatch creates each one
	// owner-referenced to the guest (so it GCs with the guest via cascade) before
	// applying PodAnnotations. Nil for kube-ovn (it needs none).
	ClaimsToEnsure []*unstructured.Unstructured
}

// ovnBackend is the internal seam for one OVN-based CNI's primary-on-NAD identity.
// Unexported: it is an internal abstraction, not a public API.
type ovnBackend interface {
	// Name is for logs/conditions ("kube-ovn", "ovn-kubernetes").
	Name() string
	// Detect inspects the guest's PRIMARY NAD config. ok=true means "this backend
	// owns this guest's primary network". A NAD Get failure returns err (fail
	// closed → the caller requeues). ok=false,nil means "not mine" → next backend.
	Detect(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest) (ok bool, err error)
	// Identity computes the launcher-pod + migration-dst annotations for a guest
	// this backend Detected. guest.Status carries the assigned IP once known (for
	// the IP pin). migName is the SwiftMigration name for the dst set's
	// migration marker; it is "" for the boot-time pod-identity computation (whose
	// caller uses only PodAnnotations).
	Identity(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest, migName string) (ovnIdentity, error)
}

// ovnBackends is the ordered backend list (first-match-wins on Detect). Two
// implementations is the explicit bar; the disjoint NAD config types make order
// immaterial for correctness — it is just a stable iteration.
func ovnBackends() []ovnBackend {
	return []ovnBackend{
		kubeOVNBackend{},
		ovnKubernetesBackend{},
	}
}

// resolveOVNBackend returns the backend that owns the guest's primary network, or
// (nil, nil) if the guest's primary does not ride an OVN-backend NAD, or (nil, err)
// on a NAD Get failure (fail closed).
func resolveOVNBackend(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest) (ovnBackend, error) {
	for _, b := range ovnBackends() {
		ok, err := b.Detect(ctx, c, guest)
		if err != nil {
			return nil, err
		}
		if ok {
			return b, nil
		}
	}
	return nil, nil
}

// stampOVNIdentity adds the OVN LSP-identity annotations to a launcher pod when
// the guest's primary interface rides an OVN-backend NAD; a no-op for every other
// networking mode (node-local bridge, non-OVN NAD, SR-IOV).
//
// A NAD Get failure is returned (fails closed): identity is a boot-time
// correctness requirement — a guest that boots without it is unreachable on the
// OVN segment — so requeuing rather than booting a broken guest is correct.
func (r *SwiftGuestReconciler) stampOVNIdentity(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, pod *corev1.Pod) error {
	b, err := resolveOVNBackend(ctx, r.Client, guest)
	if err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	id, err := b.Identity(ctx, r.Client, guest, "")
	if err != nil {
		return err
	}
	// Ensure any backend-required objects (e.g. an OVN-Kubernetes IPAMClaim) exist
	// BEFORE the pod references them — fail closed so a create failure requeues
	// rather than booting a pod that references a non-existent claim.
	for _, claim := range id.ClaimsToEnsure {
		if err := r.ensureOVNClaim(ctx, guest, claim); err != nil {
			return err
		}
	}
	if len(id.PodAnnotations) == 0 {
		return nil
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	for k, v := range id.PodAnnotations {
		pod.Annotations[k] = v
	}
	return nil
}

// ensureOVNClaim creates a backend-required cluster object (e.g. an OVN-Kubernetes
// IPAMClaim) owner-referenced to the guest so it GCs with the guest via cascade.
// Idempotent: AlreadyExists is success (the claim is stable across pod recreate and
// the migration dst pod, which references the same name). Create-only — no Get, so
// it opens no informer (no extra list/watch RBAC needed beyond create).
func (r *SwiftGuestReconciler) ensureOVNClaim(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, claim *unstructured.Unstructured) error {
	if err := controllerutil.SetControllerReference(guest, claim, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// OVNMigrationDstAnnotations returns the OVN backend's extra annotations for a
// live-migration DESTINATION pod of the guest, or an empty map when the guest's
// primary does not ride an OVN-backend NAD (so the call is inert for every other
// networking mode). Resolved from the guest. A NAD Get failure is returned (fail
// closed) so the migration step requeues rather than building a dst pod without
// the identity. Exported for the swiftmigration dst-pod builder, which lives in a
// different package and has no access to the unexported backend seam.
func OVNMigrationDstAnnotations(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest, migName string) (map[string]string, error) {
	b, err := resolveOVNBackend(ctx, c, guest)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, nil
	}
	id, err := b.Identity(ctx, c, guest, migName)
	if err != nil {
		return nil, err
	}
	return id.MigrationDstAnnotations, nil
}

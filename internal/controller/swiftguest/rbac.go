// Per-namespace RBAC bootstrapping for swiftletd.
//
// The launcher pod runs as the workload namespace's `default`
// ServiceAccount (kubelet-default; no `serviceAccountName` set in the
// pod spec). swiftletd needs `pods/get,patch` and
// `swiftguests/status:patch` to write the IP / runtime / status
// annotations the controller observes. Phase 2 PR-B added the
// `kubeswift-swiftletd-reporter` ClusterRole that grants exactly
// these verbs.
//
// Prior to 2026-04-29 the matching RoleBinding had to be applied
// manually per workload namespace (see snapshot walkthrough finding
// F2, Phase 2 walkthrough finding W3, and the
// `config/rbac/swiftletd-role.yaml` history note). Operators following
// the SwiftGuest sample manifests in any non-default namespace hit
// silent failures: the lease poller's IP-annotation patch returned
// 403, the action loop's `pods/get` returned 403, and the
// SwiftGuest's `status.network.primaryIP` stayed empty forever
// (compounded by lease.rs's prior buggy "return on first patch
// attempt regardless of result" — fixed in the same change as this
// helper).
//
// `EnsureSwiftletdRBAC` runs idempotently at the start of every
// SwiftGuest reconcile. It creates a `swiftletd-reporter` RoleBinding
// in the SwiftGuest's namespace if absent; on `IsAlreadyExists` it
// returns success (the binding from a prior reconcile is
// load-bearing across all SwiftGuests in that namespace, not owned
// by any single one).

package swiftguest

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SwiftletdReporterClusterRoleName is the name of the cluster-scoped
// Role swiftletd uses. Defined as a const here (and matched in
// `config/rbac/swiftletd-role.yaml` + the Helm chart) so a future
// rename has one source of truth.
const SwiftletdReporterClusterRoleName = "kubeswift-swiftletd-reporter"

// SwiftletdReporterRoleBindingName is the per-namespace RoleBinding
// name created by `EnsureSwiftletdRBAC`. Same name in every namespace.
const SwiftletdReporterRoleBindingName = "swiftletd-reporter"

// LauncherServiceAccountName is the ServiceAccount the launcher pod
// runs as. Currently hardcoded to `default` (kubelet-default when
// `serviceAccountName` is absent from the pod spec). If we later
// switch to a dedicated `swiftletd-launcher` SA, update both here
// and the pod-builder.
const LauncherServiceAccountName = "default"

// EnsureSwiftletdRBAC creates the `swiftletd-reporter` RoleBinding in
// the given namespace if it does not already exist. The binding
// references the cluster-scoped `kubeswift-swiftletd-reporter`
// ClusterRole and binds it to the namespace's `default` ServiceAccount.
//
// Idempotent: returns nil on AlreadyExists. Safe to call from every
// SwiftGuest reconcile; the binding is shared across all SwiftGuests
// in the namespace (no controller-reference; the binding outlives
// individual SwiftGuests by design — deleting all SwiftGuests in a
// namespace does not remove the binding).
//
// Failure path: returns the underlying error. Callers (the SwiftGuest
// controller's `Reconcile`) should NOT proceed to pod creation if this
// fails — without the binding the launcher pod boots but its
// annotation-writing paths are silently broken. Better to surface the
// RBAC error to the operator via a status condition than to ship a
// pod that looks Running but has no IP annotation.
func EnsureSwiftletdRBAC(ctx context.Context, c client.Client, namespace string) error {
	// Fast-path: check if the binding already exists. Avoids hitting
	// the apiserver for create+AlreadyExists on every reconcile.
	var existing rbacv1.RoleBinding
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: SwiftletdReporterRoleBindingName}, &existing)
	if err == nil {
		// Binding present. We do NOT mutate (operators may have
		// customized subjects to add their own SA); the controller's
		// only invariant is that SOME binding for this Role exists.
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get rolebinding %s/%s: %w", namespace, SwiftletdReporterRoleBindingName, err)
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SwiftletdReporterRoleBindingName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "swiftletd-rbac",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     SwiftletdReporterClusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      LauncherServiceAccountName,
				Namespace: namespace,
			},
		},
	}

	if err := c.Create(ctx, rb); err != nil {
		// AlreadyExists race: another reconcile (or a parallel
		// SwiftGuest in the same namespace) created the binding
		// between our Get and our Create. Treat as success.
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create rolebinding %s/%s: %w", namespace, SwiftletdReporterRoleBindingName, err)
	}
	return nil
}

package swiftguest

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// LauncherRBACReconciler retires the legacy `default` subject from launcher
// RoleBindings in namespaces that no longer run any legacy launcher.
//
// # The gap this closes
//
// EnsureLauncherRBAC converges the subject set, but it only runs from a
// SwiftGuest or SwiftSandbox reconcile. Once the last guest in a namespace is
// deleted nothing reconciles there again, so a namespace that has been fully
// drained keeps `default` bound to the reporter ClusterRole forever — the exact
// namespace-wide `pods: patch` grant #443 exists to remove, left behind in a
// namespace that no longer has any launcher to justify it.
//
// Draining is also the moment the grant is least defensible: with no VM left,
// the only thing the binding can still do is hand `pods: patch` to whatever
// else lives in that namespace.
//
// # Why a RoleBinding watch rather than a timer
//
// Reconciling the binding itself makes the object that carries the stale
// subject the thing that drives its own cleanup, so convergence keeps running
// after every CR is gone. The manager's 30s SyncPeriod resyncs watched objects,
// which gives a drained namespace a bounded retirement window without a bespoke
// ticker.
//
// It is deliberately NOT owned by any CR: these objects are shared by every
// launcher in the namespace and must outlive any individual guest.
type LauncherRBACReconciler struct {
	client.Client
}

// Reconcile re-runs subject convergence for the namespace of the RoleBinding
// that triggered it.
//
// Convergence is idempotent and has a no-op guard, so a resync that finds
// nothing to do writes nothing and does not re-enqueue itself.
func (r *LauncherRBACReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rb rbacv1.RoleBinding
	if err := r.Get(ctx, req.NamespacedName, &rb); err != nil {
		// Deleted, or not ours. Nothing to converge; the next launcher created
		// in this namespace recreates the binding from scratch.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	class, ok := launcherClassForBinding(req.Name)
	if !ok {
		return ctrl.Result{}, nil
	}
	if err := EnsureLauncherRBAC(ctx, r.Client, req.Namespace, class); err != nil {
		// A namespace being torn down races this: the binding exists when the
		// watch fires and the namespace is gone by the time we write. That is
		// ordinary, not an error worth backing off on.
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("converge launcher rbac in %s: %w", req.Namespace, err)
	}
	return ctrl.Result{}, nil
}

// launcherClassForBinding maps a RoleBinding name back to the launcher class it
// serves. Anything else in the cluster is ignored.
func launcherClassForBinding(name string) (LauncherClass, bool) {
	switch name {
	case SwiftletdReporterRoleBindingName:
		return GuestLauncher, true
	case SandboxReporterRoleBindingName:
		return SandboxLauncher, true
	default:
		// NB: LauncherClass is an int and GuestLauncher is its zero value, so
		// there is no "invalid" sentinel to return. The bool is the only
		// trustworthy signal — callers must check it, not the class.
		return GuestLauncher, false
	}
}

// SetupWithManager registers the reconciler on RoleBindings, filtered by name
// so the controller does not wake for unrelated bindings in the cluster.
func (r *LauncherRBACReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("launcher-rbac").
		For(&rbacv1.RoleBinding{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			_, ok := launcherClassForBinding(o.GetName())
			return ok
		}))).
		Complete(r)
}

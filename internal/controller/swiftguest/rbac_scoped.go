package swiftguest

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Per-launcher-pod RBAC (#515) — defence in depth after #443.
//
// WHAT THIS IS NOT. It is not what closes the escalation. That was proven on a
// live cluster: RBAC is additive and the launcher ServiceAccount is SHARED, so a
// resourceNames-scoped Role still hands an attacker holding that SA the union of
// every launcher pod it names. The ValidatingAdmissionPolicy from #443/#514 is
// what stops an attacker obtaining the SA in the first place. This narrows what
// the token is worth if they get it some other way.
//
// WHY PER-POD RATHER THAN PER-NAMESPACE. resourceNames must be a literal list —
// RBAC has no prefix or wildcard matching — and launcher pod names are not known
// until a workload exists. A namespace-wide Role whose list is edited as pods
// come and go has a window where a just-created pod is not yet named in it, and
// the launcher 403s on its first status write. Per-pod objects have no such
// window.
//
// WHY THE OWNER IS THE WORKLOAD CR, NOT THE POD. Ordering. These are created in
// the same reconcile that already calls EnsureLauncherRBAC, which runs BEFORE
// the pod is created — so the grant is always in place before swiftletd's first
// patch. Owning them by the pod would invert that: the pod must exist first,
// reintroducing the race this design exists to avoid. Owning them by the CR also
// gives exact garbage collection, since the pod never outlives its workload.
//
// SCOPE VALIDATED, MECHANISM NOT. A guest was booted on dev under a Role scoped
// to exactly its own name and completed its full lifecycle — boot, IP discovery,
// status reporting, stop — with zero RBAC denials, while `patch pod/<other>`
// was denied. See the probe on #515. That settles that no launcher path needs
// namespace-wide access; it says nothing about the machinery below.

// ScopedRoleNameFor returns the Role/RoleBinding name for a launcher pod's
// scoped grant. One name for both objects: they are created and deleted
// together, and a single name makes an orphan obvious.
func ScopedRoleNameFor(podName string) string {
	return "swiftletd-scoped-" + podName
}

// ScopedOnly reports whether the shared namespace-wide launcher binding should
// be REMOVED, leaving the per-pod scoped grants as the only access.
//
// This is the switch that actually narrows anything. Creating scoped Roles
// alongside the shared binding changes no effective permission — RBAC is a
// union — so without this the feature is inert. Off by default: turning it on
// deletes a live grant, and an operator should choose that moment.
//
// Set from the controller flag wired to the chart's
// `scopedLauncherRBAC.enabled`.
var ScopedOnly bool

// RemoveSharedLauncherBinding deletes the namespace-wide RoleBinding for a
// launcher class, and is a no-op when it is already gone.
//
// Called only when ScopedOnly is set, and only AFTER the per-pod grant for the
// pod about to be created exists — otherwise a launcher briefly has neither.
//
// Deleting RBAC is not something a controller should do lightly, and it is done
// here for a specific reason: the alternative is to stop *creating* the shared
// binding and leave any pre-existing one in place, which on every upgraded
// cluster means the operator flips the switch, believes they are narrowed, and
// is not. A security control that silently does nothing is worse than one that
// is visibly off.
func RemoveSharedLauncherBinding(ctx context.Context, c client.Client, namespace string, class LauncherClass) error {
	_, _, bindingName := launcherRBACNames(class)
	var rb rbacv1.RoleBinding
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: bindingName}, &rb)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get shared binding %s/%s: %w", namespace, bindingName, err)
	}
	if err := c.Delete(ctx, &rb); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete shared binding %s/%s: %w", namespace, bindingName, err)
	}
	return nil
}

// scopedRulesFor returns the rules a launcher of the given class may exercise,
// narrowed to podName.
//
// The class split mirrors the shared ClusterRoles: a sandbox launcher gets NO
// swiftguests/status, because it runs untrusted code and there is no SwiftGuest
// CR for a sandbox to report to (#519). Adding it here would hand an escaped
// sandbox the ability to forge guest status.
func scopedRulesFor(class LauncherClass, podName string) []rbacv1.PolicyRule {
	rules := []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"pods"},
		Verbs:         []string{"get", "patch"},
		ResourceNames: []string{podName},
	}}
	if class == SandboxLauncher {
		return rules
	}
	// A guest launcher reports GuestRunning on its own SwiftGuest. The CR and
	// the launcher pod share a name, so one resourceNames value covers both.
	return append(rules, rbacv1.PolicyRule{
		APIGroups:     []string{"swift.kubeswift.io"},
		Resources:     []string{"swiftguests/status"},
		Verbs:         []string{"get", "patch"},
		ResourceNames: []string{podName},
	})
}

// EnsureScopedLauncherRBAC creates (or converges) a Role + RoleBinding granting
// the launcher SA access to exactly podName, owned by owner.
//
// MUST be called before the launcher pod is created. Callers must not proceed to
// pod creation if it fails: without the grant the pod boots and looks healthy
// while every status write 403s, which reaches the operator as a guest that
// never reports an IP.
func EnsureScopedLauncherRBAC(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	podName string,
	class LauncherClass,
) error {
	if podName == "" {
		return fmt.Errorf("scoped launcher RBAC: empty pod name")
	}
	namespace := owner.GetNamespace()
	saName, _, _ := launcherRBACNames(class)
	name := ScopedRoleNameFor(podName)

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    rbacLabels("swiftletd-rbac"),
		},
		Rules: scopedRulesFor(class, podName),
	}
	if err := controllerutil.SetControllerReference(owner, role, scheme); err != nil {
		return fmt.Errorf("own scoped role %s/%s: %w", namespace, name, err)
	}
	if err := createOrUpdateRole(ctx, c, role); err != nil {
		return err
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    rbacLabels("swiftletd-rbac"),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: namespace,
		}},
	}
	if err := controllerutil.SetControllerReference(owner, binding, scheme); err != nil {
		return fmt.Errorf("own scoped rolebinding %s/%s: %w", namespace, name, err)
	}
	return createOrUpdateBinding(ctx, c, binding)
}

func createOrUpdateRole(ctx context.Context, c client.Client, want *rbacv1.Role) error {
	var existing rbacv1.Role
	err := c.Get(ctx, types.NamespacedName{Namespace: want.Namespace, Name: want.Name}, &existing)
	if apierrors.IsNotFound(err) {
		if cerr := c.Create(ctx, want); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return fmt.Errorf("create scoped role %s/%s: %w", want.Namespace, want.Name, cerr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get scoped role %s/%s: %w", want.Namespace, want.Name, err)
	}
	// No-op guard: without it every reconcile writes, the watch re-enqueues and
	// the controller spins — the same trap ensureConvergedBinding documents.
	if rulesEqual(existing.Rules, want.Rules) {
		return nil
	}
	existing.Rules = want.Rules
	if err := c.Update(ctx, &existing); err != nil {
		return fmt.Errorf("update scoped role %s/%s: %w", want.Namespace, want.Name, err)
	}
	return nil
}

func createOrUpdateBinding(ctx context.Context, c client.Client, want *rbacv1.RoleBinding) error {
	var existing rbacv1.RoleBinding
	err := c.Get(ctx, types.NamespacedName{Namespace: want.Namespace, Name: want.Name}, &existing)
	if apierrors.IsNotFound(err) {
		if cerr := c.Create(ctx, want); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return fmt.Errorf("create scoped rolebinding %s/%s: %w", want.Namespace, want.Name, cerr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get scoped rolebinding %s/%s: %w", want.Namespace, want.Name, err)
	}
	if subjectsEqual(existing.Subjects, want.Subjects) {
		return nil
	}
	// roleRef is immutable, so only subjects can be converged. A binding whose
	// roleRef drifted must be deleted by an operator; we do not delete RBAC.
	existing.Subjects = want.Subjects
	if err := c.Update(ctx, &existing); err != nil {
		return fmt.Errorf("update scoped rolebinding %s/%s: %w", want.Namespace, want.Name, err)
	}
	return nil
}

func rulesEqual(a, b []rbacv1.PolicyRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !stringsEqual(a[i].APIGroups, b[i].APIGroups) ||
			!stringsEqual(a[i].Resources, b[i].Resources) ||
			!stringsEqual(a[i].Verbs, b[i].Verbs) ||
			!stringsEqual(a[i].ResourceNames, b[i].ResourceNames) {
			return false
		}
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

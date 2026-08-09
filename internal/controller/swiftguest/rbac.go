// Per-namespace RBAC bootstrapping for swiftletd launcher pods.
//
// swiftletd needs `pods: get,patch` (to write the IP / runtime / status
// annotations the controller observes) and, for a SwiftGuest launcher only,
// `swiftguests/status: get,patch` (the GuestRunning condition).
//
// # Why a dedicated ServiceAccount, and what it does NOT buy
//
// Until v0.13.6 the launcher ran as the workload namespace's `default`
// ServiceAccount, and the reporter ClusterRole was bound to `default`. That
// meant EVERY pod in the namespace — every Job, sidecar, CronJob and kubectl
// debug pod — silently inherited `pods: patch`. Since `spec.containers[*].image`
// is mutable and kubelet restarts a container whose spec hash changed
// (regardless of `restartPolicy: Never`), any tenant who could create a pod
// could repoint the PRIVILEGED launcher's image and obtain node root.
//
// Be precise about what the dedicated SA fixes, because it is easy to overclaim:
// Kubernetes has NO RBAC gate on which ServiceAccount a pod may reference. A
// tenant who can create pods can still write `serviceAccountName:
// kubeswift-launcher` on their own pod and receive the token, and
// `automountServiceAccountToken: false` on the SA does not help — the attacker
// sets `true` on theirs. So this change buys three things:
//
//   - no INCIDENTAL inheritance (the common case: ordinary workloads in the
//     namespace no longer hold pods:patch by accident),
//   - the grant becomes attributable in an audit log,
//   - it is the prerequisite for the fix that actually closes the hole —
//     `resourceNames`-scoping pods:get,patch to the launcher pod names.
//
// The residual (namespace-wide pods:get,patch for anything that names the SA)
// is tracked in #443 and recorded in docs/security-audit.md. A change that only
// renames the SA must not claim the escalation is closed.
//
// # Two roles, not one
//
// A sandbox launcher provably does not need `swiftguests/status`: pod.go stamps
// `KUBESWIFT_REPORT_GUEST_CR=false` and swiftletd gates the CR patch on it.
// Granting it anyway would let an escaped sandbox — the one workload class
// KubeSwift markets as running untrusted code — forge GuestRunning/Failed on
// any SwiftGuest in the namespace, which the controller's state machine and the
// drain path both consume.
//
// # Convergence, not create-once
//
// The previous implementation early-returned when the binding existed. Renaming
// the SA under that logic would leave an upgraded cluster with
// `subjects: [default]`, the new SA unbound, and every annotation patch
// returning 403 — the guest boots and runs fine while
// `status.network.primaryIP` stays empty forever. This codebase has been burned
// by exactly that silent shape twice (findings F2 and W3).
//
// Equally, dropping `default` outright would break already-running launcher
// pods, which keep their original SA until the VM is stopped or migrated —
// potentially weeks.
//
// So the subject set converges on observed reality: the new SA is always bound,
// and `default` stays bound only while a non-terminal launcher pod in that
// namespace is still running as it. Operator-added subjects are preserved
// untouched.

package swiftguest

import (
	"context"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LauncherClass distinguishes the two launcher kinds, which get different
// ServiceAccounts and different ClusterRoles.
type LauncherClass int

const (
	// GuestLauncher is a SwiftGuest launcher: needs swiftguests/status.
	GuestLauncher LauncherClass = iota
	// SandboxLauncher is a SwiftSandbox (or warm-pool slot) launcher: pods only.
	SandboxLauncher
)

const (
	// GuestLauncherServiceAccountName is the SA a SwiftGuest launcher runs as.
	GuestLauncherServiceAccountName = "kubeswift-launcher"
	// SandboxLauncherServiceAccountName is the SA a SwiftSandbox launcher runs
	// as. Separate from the guest SA so it can hold a strictly smaller role.
	SandboxLauncherServiceAccountName = "kubeswift-sandbox-launcher"

	// SwiftletdReporterClusterRoleName grants pods:get,patch +
	// swiftguests/status:get,patch. Matched in config/rbac/swiftletd-role.yaml
	// and the Helm chart.
	SwiftletdReporterClusterRoleName = "kubeswift-swiftletd-reporter"
	// SandboxReporterClusterRoleName grants pods:get,patch only.
	SandboxReporterClusterRoleName = "kubeswift-sandbox-reporter"

	// SwiftletdReporterRoleBindingName is the per-namespace binding for the
	// guest launcher. Unchanged from previous releases so an upgrade converges
	// the existing object rather than orphaning it.
	SwiftletdReporterRoleBindingName = "swiftletd-reporter"
	// SandboxReporterRoleBindingName is the per-namespace binding for the
	// sandbox launcher.
	SandboxReporterRoleBindingName = "sandbox-reporter"

	// legacyServiceAccountName is the SA launchers used before v0.13.6. Retired
	// automatically once no launcher pod is running as it — see the file
	// comment.
	legacyServiceAccountName = "default"

	// LauncherContainerName is the swiftletd container's name in every launcher
	// pod — guest, GPU, restore and sandbox builders all use it. Verified
	// against all four; the legacy-SA sweep below selects on it, and a mismatch
	// there would retire `default` while a running pod still needed it.
	// (swiftmigration declares its own copy for the dst-pod builder.)
	LauncherContainerName = "launcher"
)

// launcherRBACNames returns the SA, ClusterRole and RoleBinding names for a class.
func launcherRBACNames(class LauncherClass) (sa, clusterRole, binding string) {
	if class == SandboxLauncher {
		return SandboxLauncherServiceAccountName, SandboxReporterClusterRoleName, SandboxReporterRoleBindingName
	}
	return GuestLauncherServiceAccountName, SwiftletdReporterClusterRoleName, SwiftletdReporterRoleBindingName
}

// LauncherServiceAccountFor returns the ServiceAccount name a launcher pod of
// the given class must run as. Pod builders MUST call this — a builder that
// omits ServiceAccountName silently keeps `default` and loses its grant once
// the legacy subject retires.
func LauncherServiceAccountFor(class LauncherClass) string {
	sa, _, _ := launcherRBACNames(class)
	return sa
}

func rbacLabels(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "kubeswift",
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
	}
}

// EnsureSwiftletdRBAC provisions the guest launcher's SA and binding.
// Kept under its original name because three controllers call it.
func EnsureSwiftletdRBAC(ctx context.Context, c client.Client, namespace string) error {
	return EnsureLauncherRBAC(ctx, c, namespace, GuestLauncher)
}

// EnsureLauncherRBAC creates the launcher ServiceAccount and its RoleBinding in
// namespace, and converges the binding's subject set.
//
// Idempotent and safe to call from every reconcile. The objects are shared by
// all launchers in the namespace and carry no owner reference: they must
// outlive any individual SwiftGuest or SwiftSandbox.
//
// Callers must NOT proceed to pod creation if this fails. Without the binding
// the launcher pod boots and looks healthy while every annotation write 403s,
// which surfaces to the operator as a guest that never reports an IP.
func EnsureLauncherRBAC(ctx context.Context, c client.Client, namespace string, class LauncherClass) error {
	saName, clusterRole, bindingName := launcherRBACNames(class)

	if err := ensureServiceAccount(ctx, c, namespace, saName); err != nil {
		return err
	}
	return ensureConvergedBinding(ctx, c, namespace, bindingName, clusterRole, saName)
}

func ensureServiceAccount(ctx context.Context, c client.Client, namespace, name string) error {
	var existing corev1.ServiceAccount
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing)
	if err == nil {
		// Present. Never mutate: operators attach imagePullSecrets to this SA
		// for private registries, and rewriting it would drop them.
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get serviceaccount %s/%s: %w", namespace, name, err)
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    rbacLabels("launcher-rbac"),
		},
	}
	if err := c.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create serviceaccount %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ensureConvergedBinding creates the RoleBinding if absent, and otherwise
// converges its subjects: the launcher SA is always bound; `default` is bound
// only while a launcher pod still runs as it; everything else is preserved.
func ensureConvergedBinding(ctx context.Context, c client.Client, namespace, bindingName, clusterRole, saName string) error {
	legacyInUse, err := legacyLauncherPodRunning(ctx, c, namespace)
	if err != nil {
		return err
	}

	var existing rbacv1.RoleBinding
	err = c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: bindingName}, &existing)
	if apierrors.IsNotFound(err) {
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bindingName,
				Namespace: namespace,
				Labels:    rbacLabels("swiftletd-rbac"),
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     clusterRole,
			},
			Subjects: desiredSubjects(nil, namespace, saName, legacyInUse),
		}
		if cerr := c.Create(ctx, rb); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return fmt.Errorf("create rolebinding %s/%s: %w", namespace, bindingName, cerr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get rolebinding %s/%s: %w", namespace, bindingName, err)
	}

	want := desiredSubjects(existing.Subjects, namespace, saName, legacyInUse)
	if subjectsEqual(existing.Subjects, want) {
		// No-op guard. Without it every reconcile writes, the watch re-enqueues,
		// and the controller spins.
		return nil
	}
	existing.Subjects = want
	if err := c.Update(ctx, &existing); err != nil {
		return fmt.Errorf("update rolebinding %s/%s: %w", namespace, bindingName, err)
	}
	return nil
}

// desiredSubjects computes the converged subject list: preserve everything the
// controller does not manage, always bind saName, and bind `default` only while
// a legacy launcher pod still needs it.
func desiredSubjects(current []rbacv1.Subject, namespace, saName string, legacyInUse bool) []rbacv1.Subject {
	managed := map[string]bool{saName: true, legacyServiceAccountName: true}

	out := make([]rbacv1.Subject, 0, len(current)+2)
	for _, s := range current {
		// Anything that is not one of ours is operator-added — keep it verbatim.
		if s.Kind != rbacv1.ServiceAccountKind || s.Namespace != namespace || !managed[s.Name] {
			out = append(out, s)
		}
	}
	out = append(out, rbacv1.Subject{
		Kind: rbacv1.ServiceAccountKind, Name: saName, Namespace: namespace,
	})
	if legacyInUse {
		out = append(out, rbacv1.Subject{
			Kind: rbacv1.ServiceAccountKind, Name: legacyServiceAccountName, Namespace: namespace,
		})
	}
	return out
}

func subjectsEqual(a, b []rbacv1.Subject) bool {
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

// legacyLauncherPodRunning reports whether any non-terminal launcher pod in the
// namespace is still running as the legacy `default` ServiceAccount.
//
// Selecting on the container name rather than a label is deliberate: guest,
// sandbox and warm-pool slot pods label themselves differently, and a missed
// label here would drop `default` from the binding while a pod still depended
// on it — breaking a running VM's status reporting with no error anywhere.
func legacyLauncherPodRunning(ctx context.Context, c client.Client, namespace string) (bool, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return false, fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		// An empty ServiceAccountName means kubelet defaulted it.
		if p.Spec.ServiceAccountName != "" && p.Spec.ServiceAccountName != legacyServiceAccountName {
			continue
		}
		for j := range p.Spec.Containers {
			if p.Spec.Containers[j].Name == LauncherContainerName {
				return true, nil
			}
		}
	}
	return false, nil
}

// LauncherImagePullSecretsEnv carries a comma-separated list of Secret names to
// attach to every launcher pod.
//
// This exists because of the switch away from `default`. Operators on private
// registries conventionally patch `imagePullSecrets` onto the namespace's
// `default` ServiceAccount, and a pod inherits its SA's pull secrets. Moving
// the launcher to a dedicated SA silently drops that inheritance and yields
// cluster-wide ImagePullBackOff on the next guest — a regression that would
// look nothing like an RBAC change. Setting them on the pod is explicit and
// works regardless of which SA the pod names. (#443)
const LauncherImagePullSecretsEnv = "KUBESWIFT_LAUNCHER_IMAGE_PULL_SECRETS"

// LauncherImagePullSecrets returns the pull secrets every launcher pod should
// carry, or nil when unset (the overwhelmingly common public-registry case).
func LauncherImagePullSecrets() []corev1.LocalObjectReference {
	raw := os.Getenv(LauncherImagePullSecretsEnv)
	if raw == "" {
		return nil
	}
	var out []corev1.LocalObjectReference
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, corev1.LocalObjectReference{Name: name})
		}
	}
	return out
}

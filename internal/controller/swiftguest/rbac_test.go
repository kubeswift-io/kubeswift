package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func rbacScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	return s
}

// TestEnsureSwiftletdRBAC_CreatesBindingWhenAbsent is the headline test
// for the RBAC bootstrap fix (Phase 2 walkthrough finding W3 / snapshot
// walkthrough finding F2). Before this helper existed, swiftletd in any
// non-default namespace hit 403 forbidden on its annotation writes; the
// SwiftGuest's status.network.primaryIP stayed empty forever. This test
// pins the contract that EnsureSwiftletdRBAC creates a RoleBinding in
// the target namespace, references the cluster-scoped
// `kubeswift-swiftletd-reporter` ClusterRole, and binds it to the
// namespace's `default` ServiceAccount.
func TestEnsureSwiftletdRBAC_CreatesBindingWhenAbsent(t *testing.T) {
	scheme := rbacScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	if err := EnsureSwiftletdRBAC(ctx, c, "team-a"); err != nil {
		t.Fatalf("EnsureSwiftletdRBAC failed: %v", err)
	}

	var rb rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: "team-a",
		Name:      SwiftletdReporterRoleBindingName,
	}, &rb); err != nil {
		t.Fatalf("expected RoleBinding to exist: %v", err)
	}

	if rb.RoleRef.Kind != "ClusterRole" {
		t.Errorf("RoleRef.Kind = %q, want ClusterRole", rb.RoleRef.Kind)
	}
	if rb.RoleRef.Name != SwiftletdReporterClusterRoleName {
		t.Errorf("RoleRef.Name = %q, want %q", rb.RoleRef.Name, SwiftletdReporterClusterRoleName)
	}
	if len(rb.Subjects) != 1 {
		t.Fatalf("len(Subjects) = %d, want 1", len(rb.Subjects))
	}
	if rb.Subjects[0].Kind != rbacv1.ServiceAccountKind {
		t.Errorf("Subjects[0].Kind = %q, want %q", rb.Subjects[0].Kind, rbacv1.ServiceAccountKind)
	}
	// The dedicated SA, not `default`. In a fresh namespace no launcher pod is
	// running as the legacy SA, so it is not bound at all (#443).
	if rb.Subjects[0].Name != GuestLauncherServiceAccountName {
		t.Errorf("Subjects[0].Name = %q, want %q", rb.Subjects[0].Name, GuestLauncherServiceAccountName)
	}
	// Subject namespace MUST equal the binding's namespace — this is
	// the part that the kustomize-based pattern got wrong (the
	// rolebinding template hardcoded `namespace: default` and the
	// operator had to remember to patch it after every apply).
	if rb.Subjects[0].Namespace != "team-a" {
		t.Errorf("Subjects[0].Namespace = %q, want %q", rb.Subjects[0].Namespace, "team-a")
	}
}

// TestEnsureSwiftletdRBAC_IdempotentOnExisting pins the convergence contract on
// a pre-existing binding: operator-added subjects survive untouched, the
// dedicated SA is added, and the legacy `default` subject is retired because no
// launcher pod in the namespace is running as it (#443).
func TestEnsureSwiftletdRBAC_IdempotentOnExisting(t *testing.T) {
	scheme := rbacScheme(t)
	preExisting := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SwiftletdReporterRoleBindingName,
			Namespace: "team-b",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     SwiftletdReporterClusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: "team-b"},
			// Operator-added extra subject — must not be removed by
			// the helper.
			{Kind: rbacv1.ServiceAccountKind, Name: "operator-tooling", Namespace: "team-b"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(preExisting).Build()
	ctx := context.Background()

	if err := EnsureSwiftletdRBAC(ctx, c, "team-b"); err != nil {
		t.Fatalf("EnsureSwiftletdRBAC failed: %v", err)
	}

	var rb rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: "team-b",
		Name:      SwiftletdReporterRoleBindingName,
	}, &rb); err != nil {
		t.Fatalf("expected RoleBinding to still exist: %v", err)
	}
	// The operator-added subject MUST survive: the helper manages only its own
	// two names and never resets the subject list.
	names := map[string]bool{}
	for _, sub := range rb.Subjects {
		names[sub.Name] = true
	}
	if !names["operator-tooling"] {
		t.Errorf("operator-added subject was dropped; subjects = %+v", rb.Subjects)
	}
	if !names[GuestLauncherServiceAccountName] {
		t.Errorf("dedicated launcher SA was not bound; subjects = %+v", rb.Subjects)
	}
	// No launcher pod in team-b runs as `default`, so the legacy subject retires.
	// Leaving it bound is the vulnerability this change exists to remove.
	if names["default"] {
		t.Errorf("legacy `default` subject should have retired; subjects = %+v", rb.Subjects)
	}
}

// TestEnsureSwiftletdRBAC_PerNamespaceIsolation pins that bindings in
// one namespace don't satisfy the precondition for another. The helper
// must Create per namespace; one binding cluster-wide is NOT enough.
func TestEnsureSwiftletdRBAC_PerNamespaceIsolation(t *testing.T) {
	scheme := rbacScheme(t)
	preExistingInTeamA := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SwiftletdReporterRoleBindingName,
			Namespace: "team-a",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: SwiftletdReporterClusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: "team-a"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(preExistingInTeamA).Build()
	ctx := context.Background()

	if err := EnsureSwiftletdRBAC(ctx, c, "team-c"); err != nil {
		t.Fatalf("EnsureSwiftletdRBAC failed for team-c: %v", err)
	}

	// team-a's binding should be unchanged.
	var rbA rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: SwiftletdReporterRoleBindingName}, &rbA); err != nil {
		t.Fatalf("team-a binding lost: %v", err)
	}
	// team-c's binding should now exist with team-c subject namespace.
	var rbC rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-c", Name: SwiftletdReporterRoleBindingName}, &rbC); err != nil {
		t.Fatalf("team-c binding not created: %v", err)
	}
	if rbC.Subjects[0].Namespace != "team-c" {
		t.Errorf("team-c subject namespace wrong: got %q, want team-c", rbC.Subjects[0].Namespace)
	}
}

// TestEnsureSwiftletdRBAC_AlreadyExistsRaceIsTolerated covers the path
// where the binding gets created between our Get-not-found and our
// Create call (e.g., two concurrent SwiftGuest reconciles in the same
// namespace, both racing the bootstrap). We use a wrapper client that
// rewrites the second Create to AlreadyExists.
func TestEnsureSwiftletdRBAC_AlreadyExistsRaceIsTolerated(t *testing.T) {
	scheme := rbacScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()
	wrapped := &alreadyExistsCreateClient{Client: c}
	if err := EnsureSwiftletdRBAC(ctx, wrapped, "team-d"); err != nil {
		t.Fatalf("AlreadyExists race must be tolerated, got: %v", err)
	}
}

// alreadyExistsCreateClient wraps a fake client and rewrites every
// Create on a RoleBinding to return apierrors.NewAlreadyExists,
// modelling the parallel-reconcile race.
type alreadyExistsCreateClient struct {
	client.Client
}

func (a *alreadyExistsCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if rb, ok := obj.(*rbacv1.RoleBinding); ok {
		return apierrors.NewAlreadyExists(
			rbacv1.Resource("rolebindings"),
			rb.Name,
		)
	}
	return a.Client.Create(ctx, obj, opts...)
}

// launcherPod builds a pod that looks like a launcher to legacyLauncherPodRunning.
func launcherPod(ns, name, sa string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			ServiceAccountName: sa,
			Containers:         []corev1.Container{{Name: LauncherContainerName, Image: "x"}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// THE upgrade hazard. On an upgraded cluster the binding already exists with
// subjects [default] and a launcher pod is still running as it. If the helper
// early-returns (the pre-#443 behaviour) the new SA is never bound and every
// annotation patch 403s — the guest boots and works while
// status.network.primaryIP stays empty forever. Silent, and this codebase has
// shipped that exact shape twice (F2, W3).
func TestEnsureLauncherRBAC_UpgradeBindsBothWhileLegacyPodRuns(t *testing.T) {
	scheme := rbacScheme(t)
	legacyBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: SwiftletdReporterRoleBindingName, Namespace: "prod"},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: SwiftletdReporterClusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: "prod"},
		},
	}
	running := launcherPod("prod", "vm-1", "default", corev1.PodRunning)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacyBinding, running).Build()

	if err := EnsureSwiftletdRBAC(context.Background(), c, "prod"); err != nil {
		t.Fatalf("EnsureSwiftletdRBAC: %v", err)
	}

	var rb rbacv1.RoleBinding
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "prod", Name: SwiftletdReporterRoleBindingName}, &rb); err != nil {
		t.Fatalf("get binding: %v", err)
	}
	names := map[string]bool{}
	for _, s := range rb.Subjects {
		names[s.Name] = true
	}
	if !names[GuestLauncherServiceAccountName] {
		t.Errorf("new SA unbound on upgrade — every annotation patch would 403 silently; subjects=%+v", rb.Subjects)
	}
	if !names["default"] {
		t.Errorf("`default` dropped while a launcher pod still runs as it — that breaks the RUNNING guest; subjects=%+v", rb.Subjects)
	}
}

// A terminal pod must not pin the legacy subject: otherwise one Completed
// launcher left in a namespace keeps `default` bound forever and the
// vulnerability never actually retires.
func TestEnsureLauncherRBAC_TerminalPodDoesNotPinLegacySubject(t *testing.T) {
	scheme := rbacScheme(t)
	done := launcherPod("stale", "vm-old", "default", corev1.PodSucceeded)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(done).Build()

	if err := EnsureSwiftletdRBAC(context.Background(), c, "stale"); err != nil {
		t.Fatalf("EnsureSwiftletdRBAC: %v", err)
	}
	var rb rbacv1.RoleBinding
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "stale", Name: SwiftletdReporterRoleBindingName}, &rb); err != nil {
		t.Fatalf("get binding: %v", err)
	}
	for _, s := range rb.Subjects {
		if s.Name == "default" {
			t.Errorf("a Succeeded launcher pinned `default`; subjects=%+v", rb.Subjects)
		}
	}
}

// A pod with no explicit ServiceAccountName was defaulted by kubelet to
// `default`, so it counts as legacy. Missing this would retire the subject out
// from under a running guest.
func TestLegacyDetectionTreatsEmptySAAsDefault(t *testing.T) {
	scheme := rbacScheme(t)
	p := launcherPod("impl", "vm-implicit", "", corev1.PodRunning)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p).Build()

	inUse, err := legacyLauncherPodRunning(context.Background(), c, "impl")
	if err != nil {
		t.Fatalf("legacyLauncherPodRunning: %v", err)
	}
	if !inUse {
		t.Error("an empty ServiceAccountName means kubelet defaulted it — must count as legacy")
	}
}

// A non-launcher pod running as `default` (an ordinary workload) must NOT keep
// the legacy subject alive — that is precisely the incidental grant being removed.
func TestLegacyDetectionIgnoresNonLauncherPods(t *testing.T) {
	scheme := rbacScheme(t)
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "some-job", Namespace: "mixed"},
		Spec: corev1.PodSpec{
			ServiceAccountName: "default",
			Containers:         []corev1.Container{{Name: "worker", Image: "busybox"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(other).Build()

	inUse, err := legacyLauncherPodRunning(context.Background(), c, "mixed")
	if err != nil {
		t.Fatalf("legacyLauncherPodRunning: %v", err)
	}
	if inUse {
		t.Error("an ordinary pod on `default` must not pin the legacy subject")
	}
}

// The sandbox launcher gets its own SA and a role WITHOUT swiftguests/status.
func TestSandboxLauncherUsesItsOwnSAAndRole(t *testing.T) {
	scheme := rbacScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if err := EnsureLauncherRBAC(context.Background(), c, "sbx", SandboxLauncher); err != nil {
		t.Fatalf("EnsureLauncherRBAC(sandbox): %v", err)
	}
	var rb rbacv1.RoleBinding
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "sbx", Name: SandboxReporterRoleBindingName}, &rb); err != nil {
		t.Fatalf("get sandbox binding: %v", err)
	}
	if rb.RoleRef.Name != SandboxReporterClusterRoleName {
		t.Errorf("sandbox bound to %q — granting swiftguests/status would let an escaped sandbox forge GuestRunning", rb.RoleRef.Name)
	}
	if rb.Subjects[0].Name != SandboxLauncherServiceAccountName {
		t.Errorf("Subjects[0].Name = %q, want %q", rb.Subjects[0].Name, SandboxLauncherServiceAccountName)
	}

	var sa corev1.ServiceAccount
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "sbx", Name: SandboxLauncherServiceAccountName}, &sa); err != nil {
		t.Fatalf("sandbox launcher SA was not created: %v", err)
	}
}

// The no-op guard: a converged binding must not be rewritten, or every reconcile
// writes, the watch re-enqueues, and the controller spins.
func TestEnsureLauncherRBAC_ConvergedIsANoOp(t *testing.T) {
	scheme := rbacScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	if err := EnsureSwiftletdRBAC(ctx, c, "quiet"); err != nil {
		t.Fatalf("first: %v", err)
	}
	var first rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Namespace: "quiet", Name: SwiftletdReporterRoleBindingName}, &first); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := EnsureSwiftletdRBAC(ctx, c, "quiet"); err != nil {
		t.Fatalf("second: %v", err)
	}
	var second rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Namespace: "quiet", Name: SwiftletdReporterRoleBindingName}, &second); err != nil {
		t.Fatalf("get again: %v", err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("binding rewritten on a converged reconcile (%s -> %s) — hot-loop risk",
			first.ResourceVersion, second.ResourceVersion)
	}
}

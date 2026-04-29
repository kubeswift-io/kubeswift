package swiftguest

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/apimachinery/pkg/types"
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
	if rb.Subjects[0].Name != LauncherServiceAccountName {
		t.Errorf("Subjects[0].Name = %q, want %q", rb.Subjects[0].Name, LauncherServiceAccountName)
	}
	// Subject namespace MUST equal the binding's namespace — this is
	// the part that the kustomize-based pattern got wrong (the
	// rolebinding template hardcoded `namespace: default` and the
	// operator had to remember to patch it after every apply).
	if rb.Subjects[0].Namespace != "team-a" {
		t.Errorf("Subjects[0].Namespace = %q, want %q", rb.Subjects[0].Namespace, "team-a")
	}
}

// TestEnsureSwiftletdRBAC_IdempotentOnExisting pins the idempotency
// contract — running the helper a second time on a namespace that
// already has the binding is a no-op (no error, no mutation).
// EnsureSwiftletdRBAC runs at the start of every SwiftGuest reconcile,
// so non-idempotent behaviour would either error every reconcile or
// mutate operator-customized subjects.
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
	// The operator-added subject MUST still be there — the helper's
	// invariant is "ensure the binding exists", NOT "reset the
	// subjects to the controller-default set".
	if len(rb.Subjects) != 2 {
		t.Errorf("Subjects mutated: got len=%d, want 2 (operator-added subject preserved)", len(rb.Subjects))
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

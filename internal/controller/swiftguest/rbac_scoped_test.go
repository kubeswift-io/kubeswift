package swiftguest

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func scopedTestGuest(name string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID("uid-" + name)},
	}
}

// The grant must name the launcher's own pod and nothing else. This is the
// property the whole change exists for, so it is asserted directly rather than
// inferred from "the guest booted".
func TestScopedRules_NameExactlyTheOwnPod(t *testing.T) {
	rules := scopedRulesFor(GuestLauncher, "my-guest")
	if len(rules) != 2 {
		t.Fatalf("guest launcher wants pods + swiftguests/status; got %d rules", len(rules))
	}
	for _, r := range rules {
		if len(r.ResourceNames) != 1 || r.ResourceNames[0] != "my-guest" {
			t.Errorf("rule %v must be scoped to exactly [my-guest]; got %v", r.Resources, r.ResourceNames)
		}
		// An empty ResourceNames is namespace-wide — the thing being removed.
		// A rule that silently loses its scoping still "works" for the launcher,
		// so nothing else in the system would catch it.
		if len(r.ResourceNames) == 0 {
			t.Errorf("rule %v is namespace-wide — the scoping became a no-op", r.Resources)
		}
	}
}

// A sandbox runs untrusted code and has no SwiftGuest CR (#519). Granting it
// swiftguests/status would let an escaped sandbox forge guest status.
func TestScopedRules_SandboxGetsNoGuestStatus(t *testing.T) {
	for _, r := range scopedRulesFor(SandboxLauncher, "sbx") {
		for _, res := range r.Resources {
			if res == "swiftguests/status" {
				t.Fatalf("sandbox launcher must not be granted swiftguests/status; got %v", r)
			}
		}
	}
}

func TestEnsureScopedLauncherRBAC_CreatesOwnedPair(t *testing.T) {
	sch := scheme.Scheme
	guest := scopedTestGuest("g1")
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(guest).Build()

	if err := EnsureScopedLauncherRBAC(context.Background(), c, sch, guest, "g1", GuestLauncher); err != nil {
		t.Fatalf("EnsureScopedLauncherRBAC: %v", err)
	}

	key := types.NamespacedName{Namespace: "ns", Name: ScopedRoleNameFor("g1")}
	var role rbacv1.Role
	if err := c.Get(context.Background(), key, &role); err != nil {
		t.Fatalf("scoped Role not created: %v", err)
	}
	var rb rbacv1.RoleBinding
	if err := c.Get(context.Background(), key, &rb); err != nil {
		t.Fatalf("scoped RoleBinding not created: %v", err)
	}

	if rb.RoleRef.Kind != "Role" || rb.RoleRef.Name != key.Name {
		t.Errorf("binding must reference the scoped Role; got %s/%s", rb.RoleRef.Kind, rb.RoleRef.Name)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != GuestLauncherServiceAccountName {
		t.Errorf("binding subject must be the guest launcher SA; got %v", rb.Subjects)
	}

	// Owned by the SwiftGuest so GC reaps both when the guest goes. Owning them
	// by the POD would require the pod to exist first, reintroducing the race.
	for _, o := range []metav1.Object{&role, &rb} {
		refs := o.GetOwnerReferences()
		if len(refs) != 1 || refs[0].Kind != "SwiftGuest" || refs[0].Name != "g1" {
			t.Errorf("%T must be owned by the SwiftGuest; got %v", o, refs)
		}
	}
}

// Reconcile runs constantly. Without a no-op guard every pass writes, the watch
// re-enqueues, and the controller spins — the trap ensureConvergedBinding
// already documents for the shared binding.
func TestEnsureScopedLauncherRBAC_IdempotentNoWrite(t *testing.T) {
	sch := scheme.Scheme
	guest := scopedTestGuest("g2")
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(guest).Build()
	ctx := context.Background()

	if err := EnsureScopedLauncherRBAC(ctx, c, sch, guest, "g2", GuestLauncher); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "ns", Name: ScopedRoleNameFor("g2")}
	var first rbacv1.Role
	if err := c.Get(ctx, key, &first); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := EnsureScopedLauncherRBAC(ctx, c, sch, guest, "g2", GuestLauncher); err != nil {
			t.Fatalf("re-ensure %d: %v", i, err)
		}
	}
	var after rbacv1.Role
	if err := c.Get(ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != after.ResourceVersion {
		t.Errorf("repeat reconciles rewrote the Role (rv %s -> %s) — the no-op guard is not holding",
			first.ResourceVersion, after.ResourceVersion)
	}
}

// Drift must converge, or an operator edit silently widens the grant forever.
func TestEnsureScopedLauncherRBAC_ConvergesWidenedRules(t *testing.T) {
	sch := scheme.Scheme
	guest := scopedTestGuest("g3")
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(guest).Build()
	ctx := context.Background()

	if err := EnsureScopedLauncherRBAC(ctx, c, sch, guest, "g3", GuestLauncher); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "ns", Name: ScopedRoleNameFor("g3")}
	var role rbacv1.Role
	if err := c.Get(ctx, key, &role); err != nil {
		t.Fatal(err)
	}
	// Simulate someone dropping resourceNames — i.e. re-widening to the whole
	// namespace, exactly the state this change removes.
	role.Rules[0].ResourceNames = nil
	if err := c.Update(ctx, &role); err != nil {
		t.Fatal(err)
	}

	if err := EnsureScopedLauncherRBAC(ctx, c, sch, guest, "g3", GuestLauncher); err != nil {
		t.Fatal(err)
	}
	var reconverged rbacv1.Role
	if err := c.Get(ctx, key, &reconverged); err != nil {
		t.Fatal(err)
	}
	if len(reconverged.Rules[0].ResourceNames) != 1 || reconverged.Rules[0].ResourceNames[0] != "g3" {
		t.Errorf("a widened Role must re-converge to [g3]; got %v", reconverged.Rules[0].ResourceNames)
	}
}

func TestEnsureScopedLauncherRBAC_EmptyPodNameFails(t *testing.T) {
	sch := scheme.Scheme
	guest := scopedTestGuest("g4")
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(guest).Build()
	if err := EnsureScopedLauncherRBAC(context.Background(), c, sch, guest, "", GuestLauncher); err == nil {
		t.Error("an empty pod name must fail loudly, not create an unscoped grant")
	}
}

var _ = client.Object(&swiftv1alpha1.SwiftGuest{})

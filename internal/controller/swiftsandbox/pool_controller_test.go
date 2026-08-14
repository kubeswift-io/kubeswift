package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// The upgrade case (#515). Slots warmed BEFORE the scoped-RBAC change have no
// per-pod grant, and createWarmSlot never revisits them — it only runs for slots
// it is creating now. If the pool did not converge grants for the slots it
// already has, an operator enabling the gate would retire the shared binding out
// from under running slots and every annotation write on them would 403: warm
// slots stop reporting Ready, checkouts stop finding them, and the pool looks
// empty for no visible reason.
func TestPoolReconcile_GrantsExistingSlotsOnUpgrade(t *testing.T) {
	s := scheme.Scheme
	ctx := context.Background()
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "busybox:1", MinWarm: 0},
		// Non-empty digest so the reconcile skips the registry resolve.
		Status: sandboxv1alpha1.SwiftSandboxPoolStatus{
			Rootfs: &sandboxv1alpha1.SandboxRootfsStatus{Digest: "sha256:deadbeef"},
		},
	}
	// A slot that predates the change: no scoped Role exists for it.
	slot := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "p-slot-old01", Namespace: "ns", UID: types.UID("uid-old01"),
		Labels: map[string]string{PoolLabelKey: "p", SlotStateLabelKey: slotStateWarm},
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pool, slot).
		WithStatusSubresource(pool).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	key := types.NamespacedName{Namespace: "ns", Name: swiftguest.ScopedRoleNameFor(slot.Name)}
	var role rbacv1.Role
	if err := c.Get(ctx, key, &role); err != nil {
		t.Fatalf("pre-existing slot got no scoped grant: %v", err)
	}
	if len(role.Rules) != 1 || len(role.Rules[0].ResourceNames) != 1 ||
		role.Rules[0].ResourceNames[0] != slot.Name {
		t.Errorf("grant must name exactly the slot pod; got %v", role.Rules)
	}
	// Owned by the POD, not the pool: the checkout re-parents the pod to a
	// SwiftSandbox, and only pod ownership survives that with exact GC.
	if ref := metav1.GetControllerOf(&role); ref == nil || ref.Kind != "Pod" || ref.UID != slot.UID {
		t.Errorf("grant must be owned by the slot pod; got %v", role.OwnerReferences)
	}
	var rb rbacv1.RoleBinding
	if err := c.Get(ctx, key, &rb); err != nil {
		t.Fatalf("pre-existing slot got no scoped binding: %v", err)
	}
}

func TestSlotsToCreate(t *testing.T) {
	cases := []struct {
		name                       string
		minWarm, maxWarm, warmLive int
		want                       int
	}{
		{"cold empty pool", 3, 6, 0, 3},
		{"partially warm", 3, 6, 1, 2},
		{"at target", 3, 6, 3, 0},
		{"over target (no negative)", 3, 6, 5, 0},
		{"maxWarm below minWarm — minWarm wins", 4, 2, 0, 4},
		{"warmLive above minWarm — no create", 3, 4, 4, 0},
		{"unset maxWarm caps at minWarm", 2, 0, 0, 2},
		{"minWarm zero warms nothing", 0, 6, 0, 0},
		{"explicit large minWarm is honored (operator owns it)", 100, 0, 0, 100},
	}
	for _, c := range cases {
		if got := slotsToCreate(c.minWarm, c.maxWarm, c.warmLive); got != c.want {
			t.Errorf("%s: slotsToCreate(min=%d,max=%d,live=%d)=%d, want %d",
				c.name, c.minWarm, c.maxWarm, c.warmLive, got, c.want)
		}
	}
}

func TestSlotsToDelete(t *testing.T) {
	cases := []struct {
		name              string
		minWarm, warmLive int
		want              int
	}{
		{"at target — nothing to drain", 3, 3, 0},
		{"under target — nothing to drain (create-side handles it)", 3, 1, 0},
		{"scaled down — drain the excess", 1, 3, 2},
		{"scaled to zero — drain all", 0, 4, 4},
		{"empty pool", 0, 0, 0},
	}
	for _, c := range cases {
		if got := slotsToDelete(c.minWarm, c.warmLive); got != c.want {
			t.Errorf("%s: slotsToDelete(min=%d,live=%d)=%d, want %d",
				c.name, c.minWarm, c.warmLive, got, c.want)
		}
	}
}

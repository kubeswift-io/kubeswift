package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// After a live migration the guest runs in the renamed destination pod, whose
// scoped grant the migration created owned by itself. Deleting the migration
// (a drain migration's TTL does, after an hour) garbage-collected the grant
// and the launcher lost API access for good. The guest controller now takes
// that grant over.
func TestReconcile_MigratedLauncherGrantIsOwnedByTheGuest(t *testing.T) {
	const dst = testGuestName + "-mig-abc12345"
	ctx := context.Background()
	g := kernelGuest()
	g.UID = "guest-uid"
	g.Status.PodRef = &corev1.ObjectReference{Name: dst, Namespace: "ns"}
	mig := &migrationv1alpha1.SwiftMigration{ObjectMeta: metav1.ObjectMeta{Name: "drain-m", Namespace: "ns", UID: "mig-uid"}}
	launcher := runningLauncher("worker-2")
	launcher.Name = dst
	launcher.Labels = map[string]string{guestPodLabelKey: testGuestName}
	c := guestClientBuilder(g, mig, testGuestClass(), readyKernel(), launcher).Build()

	// The grant as the migration left it.
	if err := EnsureScopedLauncherRBAC(ctx, c, scheme.Scheme, mig, dst, GuestLauncher); err != nil {
		t.Fatal(err)
	}

	if _, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var role rbacv1.Role
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: ScopedRoleNameFor(dst)}, &role); err != nil {
		t.Fatalf("the migrated launcher's grant is gone: %v", err)
	}
	owner := metav1.GetControllerOf(&role)
	if owner == nil || owner.Kind != "SwiftGuest" || owner.Name != testGuestName {
		t.Errorf("grant controller = %+v; it must be the guest, or it dies with the migration", owner)
	}
}

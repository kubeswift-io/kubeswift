package swiftguest

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func TestSandboxLauncherServiceAccountFor(t *testing.T) {
	if got := SandboxLauncherServiceAccountFor("sb"); got != "kubeswift-sandbox-launcher-sb" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("a", 250)
	got := SandboxLauncherServiceAccountFor(long)
	if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 || !perPodServiceAccount(got) {
		t.Errorf("a long pod name must still give a valid, prefixed account: %q (%v)", got, errs)
	}
	if got == SandboxLauncherServiceAccountFor(strings.Repeat("a", 249)+"b") {
		t.Error("two long pod names sharing a prefix must not share an account")
	}
	if perPodServiceAccount(SandboxLauncherServiceAccountName) || perPodServiceAccount(GuestLauncherServiceAccountName) {
		t.Error("the shared accounts are not per-pod")
	}
}

func identityTestSandbox(name string) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID("uid-" + name)}}
}

// A sandbox launcher's own account exists, is owned like its grant, and is
// the only subject the grant binds: the grant reaches one pod.
func TestEnsureLauncherIdentity_OwnAccountIsTheOnlySubject(t *testing.T) {
	ctx := context.Background()
	sb := identityTestSandbox("sb1")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sb).Build()
	sa := SandboxLauncherServiceAccountFor("sb1")
	if err := EnsureLauncherIdentity(ctx, c, scheme.Scheme, sb, "sb1", SandboxLauncher, sa, nil); err != nil {
		t.Fatal(err)
	}
	var acct corev1.ServiceAccount
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: sa}, &acct); err != nil {
		t.Fatalf("the launcher's own account: %v", err)
	}
	if ref := metav1.GetControllerOf(&acct); ref == nil || ref.UID != sb.UID {
		t.Errorf("account owner = %v, want the sandbox", acct.OwnerReferences)
	}
	var rb rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: ScopedRoleNameFor("sb1")}, &rb); err != nil {
		t.Fatal(err)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != sa {
		t.Errorf("subjects = %+v, want exactly %s", rb.Subjects, sa)
	}
}

// A pod created before launchers had their own account keeps the shared one
// as its grant's subject, so an upgrade never cuts a running launcher off.
func TestEnsureLauncherIdentity_ExistingSharedAccountPod(t *testing.T) {
	ctx := context.Background()
	sb := identityTestSandbox("old")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sb).Build()
	if err := EnsureLauncherIdentity(ctx, c, scheme.Scheme, sb, "old", SandboxLauncher, SandboxLauncherServiceAccountName, nil); err != nil {
		t.Fatal(err)
	}
	var rb rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: ScopedRoleNameFor("old")}, &rb); err != nil {
		t.Fatal(err)
	}
	if rb.Subjects[0].Name != SandboxLauncherServiceAccountName {
		t.Errorf("subject = %s, want the shared account the pod runs as", rb.Subjects[0].Name)
	}
	var list corev1.ServiceAccountList
	_ = c.List(ctx, &list, client.InNamespace("ns"))
	if len(list.Items) != 0 {
		t.Errorf("no per-pod account for a pod that does not use one, got %d", len(list.Items))
	}
}

// A warm slot's account moves from the pool to the slot pod with its grant.
func TestEnsureLauncherIdentity_SlotAccountHandover(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.SwiftSandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid-pool"}}
	slot := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p-slot-abcde", Namespace: "ns", UID: "uid-slot"}}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool, slot).Build()
	sa := SandboxLauncherServiceAccountFor(slot.Name)

	if err := EnsureLauncherIdentity(ctx, c, scheme.Scheme, pool, slot.Name, SandboxLauncher, sa, nil); err != nil {
		t.Fatalf("phase one: %v", err)
	}
	if err := EnsureLauncherIdentity(ctx, c, scheme.Scheme, slot, slot.Name, SandboxLauncher, sa, nil); err != nil {
		t.Fatalf("phase two: %v", err)
	}
	var acct corev1.ServiceAccount
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: sa}, &acct); err != nil {
		t.Fatal(err)
	}
	if ref := metav1.GetControllerOf(&acct); ref == nil || ref.UID != slot.UID {
		t.Errorf("account owner = %v, want the slot pod", acct.OwnerReferences)
	}
}

// An account of that name that KubeSwift did not create is never used.
func TestEnsureLauncherIdentity_RefusesAForeignAccount(t *testing.T) {
	sb := identityTestSandbox("sb2")
	sa := SandboxLauncherServiceAccountFor("sb2")
	planted := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sb, planted).Build()
	err := EnsureLauncherIdentity(context.Background(), c, scheme.Scheme, sb, "sb2", SandboxLauncher, sa, nil)
	if err == nil || !strings.Contains(err.Error(), "not created by KubeSwift") {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// For a pod's own account the grant is the only access, so a failure to
// create it stops the pod even with scoped-launcher-rbac off.
func TestEnsureLauncherIdentity_OwnAccountGrantIsAlwaysFatal(t *testing.T) {
	defer func(prev bool) { ScopedOnly = prev }(ScopedOnly)
	ScopedOnly = false
	sb := identityTestSandbox("sb3")
	denied := errors.New("roles.rbac.authorization.k8s.io is forbidden")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sb).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*rbacv1.Role); ok {
				return denied
			}
			return cl.Create(ctx, obj, opts...)
		}}).Build()
	if err := EnsureLauncherIdentity(context.Background(), c, scheme.Scheme, sb, "sb3", SandboxLauncher, SandboxLauncherServiceAccountFor("sb3"), nil); err == nil {
		t.Error("the grant is the only access of a pod's own account; failing it must stop the pod")
	}
	if err := EnsureLauncherIdentity(context.Background(), c, scheme.Scheme, sb, "sb3", SandboxLauncher, SandboxLauncherServiceAccountName, nil); err != nil {
		t.Errorf("under the shared account with the gate off the shared binding still covers it; got %v", err)
	}
}

// A Secret is granted only to a pod's own account, and to exactly the names
// asked for; on the shared account the grant would reach every launcher in
// the namespace, so asking is an error.
func TestEnsureLauncherIdentity_SecretsOnlyForOwnAccount(t *testing.T) {
	ctx := context.Background()
	sb := identityTestSandbox("sb4")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sb).Build()
	if err := EnsureLauncherIdentity(ctx, c, scheme.Scheme, sb, "sb4", SandboxLauncher,
		SandboxLauncherServiceAccountName, []string{"db"}); err == nil {
		t.Error("granting a Secret to the shared launcher account must be refused")
	}
	if err := EnsureLauncherIdentity(ctx, c, scheme.Scheme, sb, "sb4", SandboxLauncher,
		SandboxLauncherServiceAccountFor("sb4"), []string{"db", "api", "db"}); err != nil {
		t.Fatal(err)
	}
	var role rbacv1.Role
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: ScopedRoleNameFor("sb4")}, &role); err != nil {
		t.Fatal(err)
	}
	last := role.Rules[len(role.Rules)-1]
	if last.Resources[0] != "secrets" || len(last.Verbs) != 1 || last.Verbs[0] != "get" ||
		strings.Join(last.ResourceNames, ",") != "api,db" {
		t.Errorf("secrets rule = %+v, want get on exactly [api db]", last)
	}
}

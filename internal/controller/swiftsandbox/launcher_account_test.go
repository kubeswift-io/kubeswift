package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

func scopedSubject(t *testing.T, c client.Client, pod string) string {
	t.Helper()
	var rb rbacv1.RoleBinding
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: swiftguest.ScopedRoleNameFor(pod)}, &rb); err != nil {
		t.Fatalf("scoped binding for %s: %v", pod, err)
	}
	if len(rb.Subjects) != 1 {
		t.Fatalf("subjects = %+v", rb.Subjects)
	}
	return rb.Subjects[0].Name
}

// A cold sandbox's launcher runs as its own account, created before the pod,
// owned by the sandbox, and the only subject of its grant.
func TestReconcile_ColdLauncherRunsAsItsOwnAccount(t *testing.T) {
	ctx := context.Background()
	sb := plainSandbox(testImage(t))
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")

	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatalf("launcher pod: %v", err)
	}
	want := swiftguest.SandboxLauncherServiceAccountFor("sb")
	if pod.Spec.ServiceAccountName != want {
		t.Errorf("launcher runs as %q, want %q", pod.Spec.ServiceAccountName, want)
	}
	var sa corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: want}, &sa); err != nil {
		t.Fatalf("own account: %v", err)
	}
	if ref := metav1.GetControllerOf(&sa); ref == nil || ref.Kind != "SwiftSandbox" {
		t.Errorf("account owner = %v", sa.OwnerReferences)
	}
	if got := scopedSubject(t, c, "sb"); got != want {
		t.Errorf("grant subject = %q, want %q", got, want)
	}
}

// A launcher created before per-pod accounts keeps the shared account as its
// grant's subject: re-pointing it would cut the running pod off.
func TestReconcile_ExistingSharedAccountLauncherKeepsItsSubject(t *testing.T) {
	sb := plainSandbox("busybox:1")
	pod := buildPod(sb, "sandbox")
	pod.Spec.ServiceAccountName = swiftguest.SandboxLauncherServiceAccountName
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: launcherName, Ready: true}}}
	r, c := sandboxReconciler(sb, pod)
	reconcileSB(t, r, "sb")
	if got := scopedSubject(t, c, "sb"); got != swiftguest.SandboxLauncherServiceAccountName {
		t.Errorf("grant subject = %q, want the shared account the pod runs as", got)
	}
}

// A warm slot runs as its own account, created (pool-owned, phase one) before
// the slot pod, and bound by the slot's grant.
func TestCreateWarmSlot_SlotRunsAsItsOwnAccount(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", UID: "uid-pool"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "busybox:1"},
	}
	r, c := poolReconciler(pool)
	if err := r.createWarmSlot(ctx, pool, defaultKernelProfile, resolvedImage{RootfsPath: "/cache/x.ext4"}, "", nil); err != nil {
		t.Fatal(err)
	}
	slots := poolSlotPods(t, c, "p")
	if len(slots) != 1 {
		t.Fatalf("want one slot, got %d", len(slots))
	}
	want := swiftguest.SandboxLauncherServiceAccountFor(slots[0].Name)
	if slots[0].Spec.ServiceAccountName != want {
		t.Errorf("slot runs as %q, want %q", slots[0].Spec.ServiceAccountName, want)
	}
	var sa corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: want}, &sa); err != nil {
		t.Fatalf("slot account: %v", err)
	}
	if got := scopedSubject(t, c, slots[0].Name); got != want {
		t.Errorf("grant subject = %q, want %q", got, want)
	}
}

// The pool's pass hands a slot's account (left pool-owned by a create that
// had no pod UID yet) to the slot pod, so it lives exactly as long as the
// slot, through checkout.
func TestPoolReconcile_HandsTheSlotAccountToThePod(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", UID: "uid-pool"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "busybox:1", MinWarm: 0},
		Status:     sandboxv1alpha1.SwiftSandboxPoolStatus{Rootfs: &sandboxv1alpha1.SandboxRootfsStatus{Digest: "sha256:deadbeef"}},
	}
	name := "p-slot-aaaaa"
	acct := swiftguest.SandboxLauncherServiceAccountFor(name)
	slot := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: "uid-slot",
			Labels: map[string]string{PoolLabelKey: "p", SlotStateLabelKey: slotStateWarm}},
		Spec: corev1.PodSpec{ServiceAccountName: acct},
	}
	r, c := poolReconciler(pool, slot)
	if err := swiftguest.EnsureLauncherIdentity(ctx, c, r.Scheme, pool, name, swiftguest.SandboxLauncher, acct); err != nil {
		t.Fatal(err)
	}
	reconcilePool(t, r, "p")
	var sa corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: acct}, &sa); err != nil {
		t.Fatal(err)
	}
	if ref := metav1.GetControllerOf(&sa); ref == nil || ref.Kind != "Pod" || ref.Name != name {
		t.Errorf("slot account owner = %v, want the slot pod", sa.OwnerReferences)
	}
}

// A launcher account name taken by an account KubeSwift did not create keeps
// the sandbox Pending and says why, instead of failing only in the log.
func TestReconcile_ForeignLauncherAccountIsReported(t *testing.T) {
	sb := plainSandbox(testImage(t))
	planted := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: swiftguest.SandboxLauncherServiceAccountFor("sb"), Namespace: "default"}}
	r, c := sandboxReconciler(sb, planted, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPending || !strings.Contains(got.Status.Message, "not created by KubeSwift") {
		t.Errorf("got %s: %q", got.Status.Phase, got.Status.Message)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err == nil {
		t.Error("no launcher may run as an account KubeSwift did not create")
	}
}

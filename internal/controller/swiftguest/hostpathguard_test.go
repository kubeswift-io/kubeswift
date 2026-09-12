package swiftguest

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// The webhook is the primary gate but defaults to disabled (it needs
// cert-manager), so the controller must refuse an unconfined host path too --
// otherwise a default install has no enforcement at all.
func TestCheckHostPaths_EnforcedWithoutTheWebhook(t *testing.T) {
	hp := "/"
	g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{
		Filesystems: []swiftv1alpha1.Filesystem{{
			Name: "share", Source: swiftv1alpha1.FilesystemSource{HostPath: &hp},
		}},
	}}
	if err := checkHostPaths(g, []string{"/srv/vm"}); err == nil {
		t.Fatal("controller accepted hostPath / — the webhook is not the only gate")
	}
	ok := "/srv/vm/share"
	g.Spec.Filesystems[0].Source.HostPath = &ok
	if err := checkHostPaths(g, []string{"/srv/vm"}); err != nil {
		t.Errorf("rejected an allowed path: %v", err)
	}
	// Empty allowlist denies, matching the webhook's posture.
	if err := checkHostPaths(g, nil); err == nil {
		t.Error("empty allowlist should deny")
	}
}

// vhost-user sockets are host paths too: the pod builder mounts the socket's
// directory into the privileged launcher.
func TestCheckHostPaths_VhostUserSockets(t *testing.T) {
	iface := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{
		Interfaces: []swiftv1alpha1.GuestInterface{{
			Name: "net0", Type: swiftv1alpha1.InterfaceTypeVhostUser, Socket: "/etc/vhost/net0.sock",
		}},
	}}
	err := checkHostPaths(iface, []string{"/run/vhost"})
	if err == nil || !strings.Contains(err.Error(), "spec.interfaces[0].socket") {
		t.Errorf("interface socket outside the allowlist: got %v", err)
	}
	dev := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{
		VhostUserDevices: []swiftv1alpha1.VhostUserDevice{{
			Name: "blk0", Type: swiftv1alpha1.VhostUserDeviceTypeBlk, Socket: "/etc/vhost/blk0.sock",
		}},
	}}
	err = checkHostPaths(dev, []string{"/run/vhost"})
	if err == nil || !strings.Contains(err.Error(), "spec.vhostUserDevices[0].socket") {
		t.Errorf("device socket outside the allowlist: got %v", err)
	}
	dev.Spec.VhostUserDevices[0].Socket = "/run/vhost/blk0.sock"
	if err := checkHostPaths(dev, []string{"/run/vhost"}); err != nil {
		t.Errorf("rejected an allowed socket: %v", err)
	}
}

// The reconcile-level tests below cover what the operator sees. The checks
// above only prove the rule; a rejection that reaches nothing but the
// controller log is not something anyone can act on.

// hostPathGuest shares path into the kernel-boot test guest.
func hostPathGuest(path string) *swiftv1alpha1.SwiftGuest {
	g := kernelGuest()
	g.Spec.Filesystems = []swiftv1alpha1.Filesystem{{
		Name: "share", Source: swiftv1alpha1.FilesystemSource{HostPath: &path},
	}}
	return g
}

func reconcileHostPathGuest(t *testing.T, allowed []string, objs ...client.Object) (*swiftv1alpha1.SwiftGuest, client.Client, ctrl.Result, error) {
	t.Helper()
	c := guestClientBuilder(objs...).Build()
	got, res, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme, AllowedHostPathPrefixes: allowed})
	return got, c, res, err
}

func resolvedCondition(t *testing.T, g *swiftv1alpha1.SwiftGuest) metav1.Condition {
	t.Helper()
	return guestCondition(t, g, ConditionResolved)
}

// The control for the rejections below: the same guest with its path allowed
// gets a launcher pod, so "no pod" there means rejected, not a harness that
// cannot build one.
func TestReconcile_AllowedHostPathCreatesTheLauncher(t *testing.T) {
	got, c, _, err := reconcileHostPathGuest(t, []string{"/srv/vm"},
		hostPathGuest("/srv/vm/share"), testGuestClass(), readyKernel())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := len(launcherPods(t, c)); n != 1 {
		t.Fatalf("allowed host path: %d launcher pods, want 1", n)
	}
	if cond := resolvedCondition(t, got); cond.Status != metav1.ConditionTrue {
		t.Errorf("allowed host path: Resolved=%s (%s)", cond.Status, cond.Message)
	}
}

// With the webhook off, a disallowed host path used to fail only inside
// buildPod: the error was logged and retried forever, and a fresh guest had no
// phase and no conditions at all. It must fail the guest, say why, and stop.
func TestReconcile_DisallowedHostPathFailsTheGuestWithTheReason(t *testing.T) {
	got, c, res, err := reconcileHostPathGuest(t, nil,
		hostPathGuest("/etc"), testGuestClass(), readyKernel())
	if err != nil {
		t.Fatalf("a policy rejection is terminal, not a retryable error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("requeued a guest that cannot run until its spec or the allowlist changes: %+v", res)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	cond := resolvedCondition(t, got)
	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "spec.filesystems[0].source.hostPath") {
		t.Errorf("Resolved = %s %q; want False naming the field", cond.Status, cond.Message)
	}
	if n := len(launcherPods(t, c)); n != 0 {
		t.Errorf("created %d launcher pods for a rejected guest", n)
	}
}

// Failed is not sticky: allowing the prefix (a controller restart with the new
// flag, which reconciles every guest) lets the same guest start.
func TestReconcile_HostPathRejectionRecoversOnceAllowed(t *testing.T) {
	_, c, _, err := reconcileHostPathGuest(t, nil,
		hostPathGuest("/srv/vm/share"), testGuestClass(), readyKernel())
	if err != nil {
		t.Fatalf("rejecting pass: %v", err)
	}
	got, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme, AllowedHostPathPrefixes: []string{"/srv/vm"}})
	if err != nil {
		t.Fatalf("pass after allowing the prefix: %v", err)
	}
	if n := len(launcherPods(t, c)); n != 1 {
		t.Fatalf("after allowing the prefix: %d launcher pods, want 1", n)
	}
	if got.Status.Phase == swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Error("guest stayed Failed after its host path was allowed")
	}
	if cond := resolvedCondition(t, got); cond.Status != metav1.ConditionTrue {
		t.Errorf("after allowing the prefix: Resolved=%s (%s)", cond.Status, cond.Message)
	}
}

// A disk-boot guest used to clone its root disk before buildPod rejected it,
// then sat in Scheduling with Resolved=True. The check has to come first.
func TestReconcile_DisallowedHostPathDoesNotCloneTheRootDisk(t *testing.T) {
	got, c, _, err := reconcileHostPathGuest(t, nil,
		asDiskBoot(hostPathGuest("/etc")), testGuestClass(), readyImage(), preparedPVC())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	var clone corev1.PersistentVolumeClaim
	cloneErr := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: RootDiskCloneName(testGuestName)}, &clone)
	if !apierrors.IsNotFound(cloneErr) {
		t.Errorf("root disk clone PVC for a rejected guest: err = %v, want NotFound", cloneErr)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("created %d Jobs for a rejected guest", len(jobs.Items))
	}
}

// A launcher that already runs predates the allowlist change. The controller
// never edits a live launcher, so it must stay up and keep being reported --
// not be marked Failed while the VM runs -- with the violation on Resolved.
func TestReconcile_DisallowedHostPathLeavesARunningLauncherAlone(t *testing.T) {
	got, c, _, err := reconcileHostPathGuest(t, nil,
		hostPathGuest("/etc"), testGuestClass(), readyKernel(), runningLauncher(""))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := len(launcherPods(t, c)); n != 1 {
		t.Fatalf("running launcher: %d pods after reconcile, want it left in place", n)
	}
	if got.Status.Phase == swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Error("marked a guest Failed while its launcher is running")
	}
	cond := resolvedCondition(t, got)
	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "spec.filesystems[0].source.hostPath") {
		t.Errorf("Resolved = %s %q; want False naming the field", cond.Status, cond.Message)
	}
}

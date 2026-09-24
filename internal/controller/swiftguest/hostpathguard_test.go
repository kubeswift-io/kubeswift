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

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
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

// The restore snapshot path arrives via annotations, not spec, and is mounted
// into the privileged restore launcher. A tenant who can patch their own
// SwiftGuest must not be able to point it at an arbitrary node path. It is
// constrained to the snapshot base + one safe segment, independent of the
// operator host-path allowlist.
func TestCheckHostPaths_RestoreSnapshotPath(t *testing.T) {
	withRestore := func(path string) *swiftv1alpha1.SwiftGuest {
		return &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationActiveRestore:       "restore-1",
				AnnotationRestoreSnapshotPath: path,
			},
		}}
	}
	// Even with a permissive allowlist, an out-of-snapshot restore path is refused.
	for _, bad := range []string{
		"/",
		"/etc/kubernetes/pki",
		"/var/lib/kubeswift/snapshots/",          // the shared root itself
		"/var/lib/kubeswift/snapshots/../../etc", // traversal
		"/var/lib/kubeswift/snapshots/a/b",       // nested
		"/var/lib/kubeswift/snapshots/a;rm",      // shell metacharacter
		"",                                       // missing
	} {
		if err := checkHostPaths(withRestore(bad), []string{"/"}); err == nil {
			t.Errorf("accepted restore snapshot path %q", bad)
		}
	}
	// The real controller-written values: clonecommon.SnapshotDir "<ns>_<name>",
	// and what earlier versions wrote (a local hostPath, "<ns>-<name>").
	for _, ok := range []string{
		"/var/lib/kubeswift/snapshots/default_snap-1",
		"/var/lib/kubeswift/snapshots/default-snap-1",
		"/var/lib/kubeswift/snapshots/ns-name",
	} {
		if err := checkHostPaths(withRestore(ok), nil); err != nil {
			t.Errorf("rejected a legitimate restore snapshot path %q: %v", ok, err)
		}
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

// The controller enforces it even with the webhook off (the default).
func TestCheckHostPaths_RejectsCHOptionInjection(t *testing.T) {
	g := kernelGuest()
	g.Spec.VhostUserDevices = []swiftv1alpha1.VhostUserDevice{{Name: "d", Type: "blk", Socket: "/srv/vm/x,path=/dev/sda"}}
	if err := checkHostPaths(g, []string{"/srv/vm"}); err == nil {
		t.Fatal("a socket that injects a CH --disk path= option passed the controller's check")
	}
}

// restoreGuest is the kernel-boot test guest (namespace "ns") marked as the
// target of a restore from path.
func restoreGuest(path string) *swiftv1alpha1.SwiftGuest {
	g := kernelGuest()
	g.Annotations = map[string]string{
		AnnotationActiveRestore:       "restore-1",
		AnnotationRestoreSnapshotPath: path,
	}
	return g
}

// capturedSnap is a SwiftSnapshot in ns that was captured into dir.
func capturedSnap(ns, name, dir string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{
			NodeName:       "n1",
			MemorySnapshot: &snapshotv1alpha1.MemorySnapshotRef{Handle: dir},
		},
	}
}

// Every snapshot directory on a node has the shape validateRestoreSnapshotPath
// accepts, so a tenant who can annotate their own guest could restore another
// namespace's memory image -- its secrets included -- into their own VM. The
// path must be a directory of the guest's own namespace.
func TestRestoreSnapshotOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		objs []client.Object
		ok   bool
	}{
		{"derived dir of the guest's namespace", "/var/lib/kubeswift/snapshots/ns_db", nil, true},
		{"derived dir of another namespace", "/var/lib/kubeswift/snapshots/other_db", nil, false},
		{"derived dir of a namespace sharing a prefix", "/var/lib/kubeswift/snapshots/ns-a_db", nil, false},
		{"older dir a snapshot here was captured into", "/var/lib/kubeswift/snapshots/ns-db",
			[]client.Object{capturedSnap("ns", "db", "/var/lib/kubeswift/snapshots/ns-db")}, true},
		{"older dir a snapshot elsewhere was captured into", "/var/lib/kubeswift/snapshots/other-db",
			[]client.Object{capturedSnap("other", "db", "/var/lib/kubeswift/snapshots/other-db")}, false},
		{"older dir nothing was captured into", "/var/lib/kubeswift/snapshots/mine", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &SwiftGuestReconciler{Client: guestClientBuilder(tc.objs...).Build(), Scheme: scheme.Scheme}
			violation, err := r.restoreSnapshotOwnerViolation(context.Background(), restoreGuest(tc.path))
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if (violation == nil) != tc.ok {
				t.Errorf("violation = %v, want ok=%v", violation, tc.ok)
			}
		})
	}
}

// Rejected like a disallowed host path: the guest fails with the reason on
// Resolved, and no restore launcher mounting the directory is built.
func TestReconcile_RestoreFromAnotherNamespaceFailsTheGuest(t *testing.T) {
	got, c, res, err := reconcileHostPathGuest(t, nil,
		restoreGuest("/var/lib/kubeswift/snapshots/other_db"), testGuestClass(), readyKernel())
	if err != nil {
		t.Fatalf("a policy rejection is terminal, not a retryable error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("requeued: %+v", res)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if cond := resolvedCondition(t, got); cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "not the directory of a SwiftSnapshot in namespace ns") {
		t.Errorf("Resolved = %s %q", cond.Status, cond.Message)
	}
	if n := len(launcherPods(t, c)); n != 0 {
		t.Errorf("created %d launcher pods", n)
	}
}

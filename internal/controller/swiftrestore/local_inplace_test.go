package swiftrestore

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	swiftguestctrl "github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

// makeLocalSnapWithBackend ensures Backend.Local is populated so the
// new wiring's hostPath sanity check passes.
func makeLocalSnapWithBackend(name, ns, sourceGuest, hostPath, hypervisorVersion string) *snapshotv1alpha1.SwiftSnapshot {
	snap := makeLocalSnap(name, ns, sourceGuest, hostPath, hypervisorVersion)
	// makeLocalSnap already sets Backend.Local.HostPath; verify here.
	if snap.Spec.Backend.Local == nil {
		snap.Spec.Backend.Local = &snapshotv1alpha1.LocalBackend{HostPath: hostPath}
	}
	return snap
}

func TestIsInPlaceRestore_TableCases(t *testing.T) {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/x", "v51.1")

	cases := []struct {
		name       string
		targetName string
		regen      []snapshotv1alpha1.IdentityRegenerationItem
		want       bool
	}{
		{"same name, no regen → in-place", "g1", nil, true},
		{"same name, empty regen → in-place", "g1", []snapshotv1alpha1.IdentityRegenerationItem{}, true},
		{"different name → clone", "g1-clone", nil, false},
		{"same name + regen → clone (operator wants divergence)", "g1",
			[]snapshotv1alpha1.IdentityRegenerationItem{snapshotv1alpha1.RegenMACAddresses}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restore := makeRestore("r1", "default", "snap1", c.targetName, true)
			if c.regen != nil {
				restore.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{Regenerate: c.regen}
			}
			if got := IsInPlaceRestore(snap, restore); got != c.want {
				t.Errorf("IsInPlaceRestore = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLocal_Pending_InPlaceRestore_StampsExistingGuestAndAdvances(t *testing.T) {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	restore := makeRestore("r1", "default", "snap1", "g1", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	source := makeSourceGuest("default", "g1", "ubuntu-noble")
	// Existing launcher pod (will be deleted by the in-place path).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"},
	}

	r, c := newReconciler(t, snap, restore, source, pod)
	r.CurrentHypervisorVersion = "v51.1"
	reconcile(t, r, "r1", "default")

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseRestoring {
		t.Fatalf("phase = %s, want Restoring", got.Status.Phase)
	}
	if got.Status.GuestRef == nil || got.Status.GuestRef.Name != "g1" {
		t.Errorf("guestRef = %+v, want g1", got.Status.GuestRef)
	}

	// Source SwiftGuest must now carry the active-restore annotations.
	var refreshed swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.Annotations[swiftguestctrl.AnnotationActiveRestore] != "r1" {
		t.Errorf("annotation %s = %q", swiftguestctrl.AnnotationActiveRestore,
			refreshed.Annotations[swiftguestctrl.AnnotationActiveRestore])
	}
	if refreshed.Annotations[swiftguestctrl.AnnotationRestoreMode] != swiftguestctrl.RestoreModeInPlace {
		t.Errorf("restore mode = %q, want %s",
			refreshed.Annotations[swiftguestctrl.AnnotationRestoreMode], swiftguestctrl.RestoreModeInPlace)
	}
	if refreshed.Annotations[swiftguestctrl.AnnotationRestoreNodeName] != "boba" {
		t.Errorf("node name annotation = %q", refreshed.Annotations[swiftguestctrl.AnnotationRestoreNodeName])
	}
	// In-place path must NOT set MAC rewrites or cmdline marker.
	if refreshed.Annotations[swiftguestctrl.AnnotationRestoreMACRewrites] != "" {
		t.Errorf("in-place must not set MAC rewrites")
	}
	if refreshed.Annotations[swiftguestctrl.AnnotationRestoreAppendCmdlineMarker] != "" {
		t.Errorf("in-place must not set cmdline marker (no clone identity divergence)")
	}

	// Existing launcher pod must have been force-deleted.
	var existingPod corev1.Pod
	err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &existingPod)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected launcher pod gone, got err=%v", err)
	}
}

func TestLocal_Pending_InPlaceRestore_RequiresOverwriteExisting(t *testing.T) {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	// OverwriteExisting NOT set — must fail with a TargetConflict-style message.
	restore := makeRestore("r1", "default", "snap1", "g1", true)
	source := makeSourceGuest("default", "g1", "ubuntu-noble")

	r, c := newReconciler(t, snap, restore, source)
	r.CurrentHypervisorVersion = "v51.1"
	reconcile(t, r, "r1", "default")

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	cond := findReady(got)
	if cond == nil || !contains(cond.Message, "overwriteExisting") {
		t.Errorf("expected overwriteExisting message, got %q", reasonOrEmpty(cond))
	}
}

func TestLocal_Pending_CloneRequiresMacAddressesRegen(t *testing.T) {
	// Different target name + no regen → clone restore that omits the
	// macAddresses regen. The wiring must reject this with a clear
	// message; without a unique MAC, two clones L2-collide.
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	restore := makeRestore("r1", "default", "snap1", "g1-clone", true)
	source := makeSourceGuest("default", "g1", "ubuntu-noble")

	r, c := newReconciler(t, snap, restore, source)
	r.CurrentHypervisorVersion = "v51.1"
	reconcile(t, r, "r1", "default")

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	cond := findReady(got)
	if cond == nil || !contains(cond.Message, "macAddresses") {
		t.Errorf("message must mention macAddresses; got %q", reasonOrEmpty(cond))
	}
}

func TestLocal_Pending_CloneCreatesTargetGuestWithMACRewrites(t *testing.T) {
	// Different target name + macAddresses regen → controller creates
	// the target SwiftGuest with the AnnotationRestoreMACRewrites set.
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	restore := makeRestore("r1", "default", "snap1", "g1-clone-a", true)
	restore.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{
		Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{
			snapshotv1alpha1.RegenMACAddresses,
			snapshotv1alpha1.RegenMachineID,
		},
	}
	source := makeSourceGuest("default", "g1", "ubuntu-noble")

	r, c := newReconciler(t, snap, restore, source)
	r.CurrentHypervisorVersion = "v51.1"
	reconcile(t, r, "r1", "default")

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseRestoring {
		t.Fatalf("phase = %s, want Restoring", got.Status.Phase)
	}
	var clone swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1-clone-a", Namespace: "default"}, &clone); err != nil {
		t.Fatalf("get clone target: %v", err)
	}
	if clone.Annotations[swiftguestctrl.AnnotationActiveRestore] != "r1" {
		t.Errorf("clone missing active-restore annotation")
	}
	if clone.Annotations[swiftguestctrl.AnnotationRestoreMode] != swiftguestctrl.RestoreModeClone {
		t.Errorf("clone mode = %q, want clone", clone.Annotations[swiftguestctrl.AnnotationRestoreMode])
	}
	if clone.Annotations[swiftguestctrl.AnnotationRestoreMACRewrites] == "" {
		t.Errorf("clone must have MAC rewrites annotation set")
	}
	// MachineID regen → cmdline marker MUST be set.
	if clone.Annotations[swiftguestctrl.AnnotationRestoreAppendCmdlineMarker] != "true" {
		t.Errorf("clone with machineId regen must set cmdline marker")
	}
	// Two clones from the same snapshot should produce different
	// MACs (deterministic from target name).
	restore2 := makeRestore("r2", "default", "snap1", "g1-clone-b", true)
	restore2.Spec.Identity = restore.Spec.Identity
	r2, c2 := newReconciler(t, snap, restore2, source)
	r2.CurrentHypervisorVersion = "v51.1"
	reconcile(t, r2, "r2", "default")
	var clone2 swiftv1alpha1.SwiftGuest
	if err := c2.Get(context.Background(), client.ObjectKey{Name: "g1-clone-b", Namespace: "default"}, &clone2); err != nil {
		t.Fatalf("get clone-b: %v", err)
	}
	macsA := clone.Annotations[swiftguestctrl.AnnotationRestoreMACRewrites]
	macsB := clone2.Annotations[swiftguestctrl.AnnotationRestoreMACRewrites]
	if macsA == macsB || macsA == "" {
		t.Errorf("two clones must get different MAC rewrite lists; A=%q B=%q", macsA, macsB)
	}
}

func TestLocal_Pending_RejectsSnapshotMissingNodeName(t *testing.T) {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/x", "v51.1")
	snap.Status.NodeName = ""
	restore := makeRestore("r1", "default", "snap1", "g1", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	source := makeSourceGuest("default", "g1", "ubuntu-noble")

	r, c := newReconciler(t, snap, restore, source)
	reconcile(t, r, "r1", "default")

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	cond := findReady(got)
	if cond == nil || !contains(cond.Message, "nodeName") {
		t.Errorf("expected nodeName-required message, got %q", reasonOrEmpty(cond))
	}
}

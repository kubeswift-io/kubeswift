package swiftrestore

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

// -------- Pure version-comparison tests --------

func TestCompareHypervisorVersions_ExactMatch(t *testing.T) {
	if got := CompareHypervisorVersions("v51.1", "v51.1"); got != VersionExactMatch {
		t.Errorf("got %v, want VersionExactMatch", got)
	}
}

func TestCompareHypervisorVersions_PatchDriftSameMinor(t *testing.T) {
	if got := CompareHypervisorVersions("v51.1.0", "v51.1.2"); got != VersionPatchDrift {
		t.Errorf("got %v, want VersionPatchDrift", got)
	}
}

func TestCompareHypervisorVersions_MinorMismatch(t *testing.T) {
	if got := CompareHypervisorVersions("v51.1", "v51.2"); got != VersionMinorMismatch {
		t.Errorf("got %v, want VersionMinorMismatch", got)
	}
}

func TestCompareHypervisorVersions_MajorMismatch(t *testing.T) {
	if got := CompareHypervisorVersions("v51.1", "v52.0"); got != VersionMajorMismatch {
		t.Errorf("got %v, want VersionMajorMismatch", got)
	}
}

func TestCompareHypervisorVersions_EmptyEitherSide(t *testing.T) {
	if got := CompareHypervisorVersions("", "v51.1"); got != VersionUnknown {
		t.Errorf("empty snap version: got %v, want VersionUnknown", got)
	}
	if got := CompareHypervisorVersions("v51.1", ""); got != VersionUnknown {
		t.Errorf("empty current version: got %v, want VersionUnknown", got)
	}
}

func TestCompareHypervisorVersions_GarbageInputsAreUnknown(t *testing.T) {
	if got := CompareHypervisorVersions("not a version", "v51.1"); got != VersionUnknown {
		t.Errorf("got %v, want VersionUnknown", got)
	}
}

func TestCompareHypervisorVersions_HandlesPrefixVariations(t *testing.T) {
	// Both with v
	if got := CompareHypervisorVersions("v51.1", "v51.1"); got != VersionExactMatch {
		t.Errorf("got %v, want VersionExactMatch", got)
	}
	// One with v, one without — semantically equivalent. parseVersion
	// strips "v" so this should behave as patch-drift-or-better; with
	// no patch on either side, that's exact-match.
	if got := CompareHypervisorVersions("51.1", "v51.1"); got != VersionExactMatch {
		t.Errorf("got %v, want VersionExactMatch", got)
	}
}

// -------- Tier B Pending dispatch tests --------

func makeLocalSnap(name, ns, sourceGuest, hostPath, hypervisorVersion string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: sourceGuest},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type:  snapshotv1alpha1.SnapshotBackendLocal,
				Local: &snapshotv1alpha1.LocalBackend{HostPath: hostPath},
			},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{
			Phase:             snapshotv1alpha1.SwiftSnapshotPhaseReady,
			HypervisorVersion: hypervisorVersion,
			NodeName:          "boba",
		},
	}
}

func TestLocal_Pending_MajorVersionMismatch_FailsWithVersionReason(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	restore := makeRestore("restore1", "default", "snap1", "g1-restored", true)

	r, c := newReconciler(t, snap, restore)
	r.CurrentHypervisorVersion = "v52.0"
	reconcile(t, r, "restore1", "default")

	got := get(t, c, "restore1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonRestoreFailed {
		t.Errorf("reason = %q, want RestoreFailed", reasonOrEmpty(cond))
	} else if !contains(cond.Message, "major-version mismatch") {
		t.Errorf("message %q missing 'major-version mismatch'", cond.Message)
	}
}

func TestLocal_Pending_MinorVersionMismatch_FailsWithVersionReason(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	restore := makeRestore("restore1", "default", "snap1", "g1-restored", true)

	r, c := newReconciler(t, snap, restore)
	r.CurrentHypervisorVersion = "v51.2"
	reconcile(t, r, "restore1", "default")

	got := get(t, c, "restore1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || !contains(cond.Message, "minor-version mismatch") {
		t.Errorf("missing minor-version mismatch reason; cond=%v", cond)
	}
}

func TestLocal_Pending_PatchDrift_DoesNotBlockRestore(t *testing.T) {
	// Patch-only difference is allowed (architect risk #3 mitigation:
	// don't block routine upgrades). With 10b wiring in, the restore
	// proceeds past the version check; we use a same-name target to
	// avoid the macAddresses-required gate (in-place fast path).
	snap := makeLocalSnap("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1.0")
	restore := makeRestore("restore1", "default", "snap1", "g1", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	source := makeSourceGuest("default", "g1", "ubuntu-noble")

	r, c := newReconciler(t, snap, restore, source)
	r.CurrentHypervisorVersion = "v51.1.2"
	reconcile(t, r, "restore1", "default")

	got := get(t, c, "restore1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseRestoring {
		t.Fatalf("phase = %s, want Restoring", got.Status.Phase)
	}
	cond := findReady(got)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if contains(cond.Message, "version mismatch") {
		t.Errorf("patch drift should not surface as version mismatch; got %q", cond.Message)
	}
}

func TestLocal_Pending_SkipAnnotationBypassesCheck(t *testing.T) {
	// Operator override for disaster-recovery: even a major mismatch
	// passes the version gate when the skip annotation is set. With
	// the in-place target name + OverwriteExisting we sail through
	// to Restoring rather than failing on either gate.
	snap := makeLocalSnap("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v50.0")
	restore := makeRestore("restore1", "default", "snap1", "g1", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	restore.Annotations = map[string]string{SkipHypervisorVersionCheckAnnotation: "true"}
	source := makeSourceGuest("default", "g1", "ubuntu-noble")

	r, c := newReconciler(t, snap, restore, source)
	r.CurrentHypervisorVersion = "v52.5"
	reconcile(t, r, "restore1", "default")

	got := get(t, c, "restore1", "default")
	cond := findReady(got)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if contains(cond.Message, "version mismatch") {
		t.Errorf("skip annotation should bypass version check; got %q", cond.Message)
	}
}

func TestLocal_Pending_NoCurrentVersionConfigured_AllowsRestore(t *testing.T) {
	// Empty CurrentHypervisorVersion → version check is disabled (a
	// controller deployed without the env wiring). Restores proceed
	// rather than blocking.
	snap := makeLocalSnap("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	restore := makeRestore("restore1", "default", "snap1", "g1", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	source := makeSourceGuest("default", "g1", "ubuntu-noble")

	r, c := newReconciler(t, snap, restore, source)
	r.CurrentHypervisorVersion = ""
	reconcile(t, r, "restore1", "default")

	got := get(t, c, "restore1", "default")
	cond := findReady(got)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if contains(cond.Message, "version mismatch") {
		t.Errorf("empty CurrentHypervisorVersion should not run version check; got %q", cond.Message)
	}
}

func TestIsTierBRestore(t *testing.T) {
	csi := &snapshotv1alpha1.SwiftSnapshot{
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot,
			},
		},
	}
	if IsTierBRestore(csi) {
		t.Errorf("CSI snapshot should not be Tier B")
	}
	local := &snapshotv1alpha1.SwiftSnapshot{
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendLocal,
			},
		},
	}
	if !IsTierBRestore(local) {
		t.Errorf("local snapshot should be Tier B")
	}
}

// -------- Helpers --------

// contains is a tiny wrapper around strings.Contains kept around so
// the test code reads naturally; Go's strings.Contains works fine
// here but the call site reads better with the local name.
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

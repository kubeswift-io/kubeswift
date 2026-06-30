package swiftrestore

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	swiftguestctrl "github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
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

// -------- handlePendingLocal: clone re-entrancy guard --------

func TestLocal_Pending_Clone_ReentrantReconcileDoesNotConflict(t *testing.T) {
	// Reproduces the failure mode that surfaced in the f41a310 e2e:
	// a prior reconcile of this same SwiftRestore created the clone
	// target and queued a status update to phase=Restoring, but the
	// controller cache served stale phase=Pending while the new
	// SwiftGuest was already visible. handlePendingLocal must
	// recognize "this is our own work" via the swift-restore label
	// and not fail with TargetConflict.
	snap := makeLocalSnap("snap1", "default", "src", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseReady
	snap.Status.NodeName = "boba"
	restore := makeRestore("restore-clone", "default", "snap1", "clone-a", true)
	restore.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{
		Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{
			snapshotv1alpha1.RegenMACAddresses,
		},
	}
	source := makeSourceGuest("default", "src", "ubuntu-noble")

	// Pre-existing clone-a SwiftGuest from the prior reconcile.
	preExisting := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "clone-a",
			Namespace: "default",
			Labels: map[string]string{
				swiftRestoreOwnerLabel: "restore-clone", // owned by THIS SwiftRestore
			},
		},
		Spec: source.Spec,
	}

	r, c := newReconciler(t, snap, restore, source, preExisting)
	r.CurrentHypervisorVersion = "v51.1"
	reconcile(t, r, "restore-clone", "default")

	got := get(t, c, "restore-clone", "default")
	if got.Status.Phase == snapshotv1alpha1.SwiftRestorePhaseFailed {
		cond := findReady(got)
		t.Fatalf("re-entrant reconcile must not Fail; got phase=Failed reason=%s msg=%q",
			reasonOrEmpty(cond), msgOrEmpty(cond))
	}
}

func TestLocal_Pending_Clone_ExistingSwiftGuestNotOwnedByUs_StillConflicts(t *testing.T) {
	// Counter-case: the user (or another controller) created a
	// SwiftGuest with the target name. Without OverwriteExisting,
	// this MUST still trip TargetConflict — we can't silently take
	// over someone else's resource.
	snap := makeLocalSnap("snap1", "default", "src", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseReady
	snap.Status.NodeName = "boba"
	restore := makeRestore("restore-clone", "default", "snap1", "clone-a", true)
	restore.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{
		Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{
			snapshotv1alpha1.RegenMACAddresses,
		},
	}
	source := makeSourceGuest("default", "src", "ubuntu-noble")

	// Clone-a exists but with a DIFFERENT label value (owned by
	// another SwiftRestore, or no label at all because user-created).
	foreign := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "clone-a",
			Namespace: "default",
			Labels: map[string]string{
				swiftRestoreOwnerLabel: "different-restore",
			},
		},
		Spec: source.Spec,
	}

	r, c := newReconciler(t, snap, restore, source, foreign)
	r.CurrentHypervisorVersion = "v51.1"
	reconcile(t, r, "restore-clone", "default")

	got := get(t, c, "restore-clone", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Fatalf("phase = %s, want Failed (foreign SwiftGuest)", got.Status.Phase)
	}
	cond := findReady(got)
	if cond == nil || cond.Reason != ReasonTargetConflict {
		t.Errorf("reason = %q, want TargetConflict", reasonOrEmpty(cond))
	}
}

// -------- restoreAnnotations: clone-mode wiring --------

func TestRestoreAnnotations_Clone_SetsRuntimeDirPrefixAndNullifyHostMAC(t *testing.T) {
	r := &SwiftRestoreReconciler{}
	restore := &snapshotv1alpha1.SwiftRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "rst1", Namespace: "default"},
		Spec: snapshotv1alpha1.SwiftRestoreSpec{
			TargetGuest: snapshotv1alpha1.SwiftRestoreTarget{Name: "clone-a"},
			Identity: &snapshotv1alpha1.IdentityRegeneration{
				Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{
					snapshotv1alpha1.RegenMACAddresses,
					snapshotv1alpha1.RegenMachineID,
					snapshotv1alpha1.RegenSSHHostKeys,
					snapshotv1alpha1.RegenHostname,
				},
			},
		},
	}
	snap := &snapshotv1alpha1.SwiftSnapshot{
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type:  snapshotv1alpha1.SnapshotBackendLocal,
				Local: &snapshotv1alpha1.LocalBackend{HostPath: "/var/lib/kubeswift/snapshots/default-snap1"},
			},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{NodeName: "boba"},
	}
	source := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "default"}}

	annos := r.restoreAnnotations(restore, snap, source, false /* inPlace */, snap.Spec.Backend.Local.HostPath, snap.Status.NodeName)

	wantFrom := "/var/lib/kubeswift/run/default-src/"
	wantTo := "/var/lib/kubeswift/run/default-clone-a/"
	if got := annos[swiftguestctrl.AnnotationRestoreRuntimeDirFromPrefix]; got != wantFrom {
		t.Errorf("from prefix = %q, want %q", got, wantFrom)
	}
	if got := annos[swiftguestctrl.AnnotationRestoreRuntimeDirToPrefix]; got != wantTo {
		t.Errorf("to prefix = %q, want %q", got, wantTo)
	}
	if got := annos[swiftguestctrl.AnnotationRestoreNullifyHostMAC]; got != "true" {
		t.Errorf("nullify-host-mac = %q, want \"true\"", got)
	}
	if got := annos[swiftguestctrl.AnnotationRestoreMode]; got != swiftguestctrl.RestoreModeClone {
		t.Errorf("mode = %q, want clone", got)
	}
}

func TestRestoreAnnotations_InPlace_DoesNotSetCloneOnlyAnnotations(t *testing.T) {
	// In-place restore reuses the source's runtime_dir name and does
	// not need any of the clone-only patches. The fast path bypasses
	// the stager entirely; setting these annotations would still
	// trigger a stager pass on a future pod recreation, so they must
	// be omitted.
	r := &SwiftRestoreReconciler{}
	restore := &snapshotv1alpha1.SwiftRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "rst1", Namespace: "default"},
		Spec: snapshotv1alpha1.SwiftRestoreSpec{
			TargetGuest: snapshotv1alpha1.SwiftRestoreTarget{Name: "src"},
		},
	}
	snap := &snapshotv1alpha1.SwiftSnapshot{
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type:  snapshotv1alpha1.SnapshotBackendLocal,
				Local: &snapshotv1alpha1.LocalBackend{HostPath: "/var/lib/kubeswift/snapshots/default-snap1"},
			},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{NodeName: "boba"},
	}
	source := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "default"}}

	annos := r.restoreAnnotations(restore, snap, source, true /* inPlace */, snap.Spec.Backend.Local.HostPath, snap.Status.NodeName)

	for _, key := range []string{
		swiftguestctrl.AnnotationRestoreRuntimeDirFromPrefix,
		swiftguestctrl.AnnotationRestoreRuntimeDirToPrefix,
		swiftguestctrl.AnnotationRestoreNullifyHostMAC,
		swiftguestctrl.AnnotationRestoreMACRewrites,
		swiftguestctrl.AnnotationRestoreAppendCmdlineMarker,
	} {
		if _, set := annos[key]; set {
			t.Errorf("in-place restore set clone-only annotation %s = %q", key, annos[key])
		}
	}
	if got := annos[swiftguestctrl.AnnotationRestoreMode]; got != swiftguestctrl.RestoreModeInPlace {
		t.Errorf("mode = %q, want in-place", got)
	}
}

func TestRuntimeDirPrefix_FormatMatchesSwiftRuntime(t *testing.T) {
	// Format must match swift-runtime's create_runtime_dir naming
	// (rust/swift-runtime/src/runtime_dir.rs:48): "<ns>-<name>" under
	// /var/lib/kubeswift/run, with a trailing "/" so the patcher's
	// prefix match doesn't clip a longer name.
	got := runtimeDirPrefix("default", "guest1")
	want := "/var/lib/kubeswift/run/default-guest1/"
	if got != want {
		t.Errorf("runtimeDirPrefix(\"default\", \"guest1\") = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, "/") {
		t.Errorf("runtimeDirPrefix must end in '/' — patcher requires it")
	}
}

// -------- Helpers --------

// contains is a tiny wrapper around strings.Contains kept around so
// the test code reads naturally; Go's strings.Contains works fine
// here but the call site reads better with the local name.
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

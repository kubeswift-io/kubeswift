package swiftmigration

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

// migWithPhase produces a minimal SwiftMigration with phase set, used
// across the table-driven cases.
func migWithPhase(phase migrationv1alpha1.SwiftMigrationPhase) *migrationv1alpha1.SwiftMigration {
	return &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Phase: phase},
	}
}

func TestShouldCheckSourcePodUID_PreCutoverPhases_ReturnsTrue(t *testing.T) {
	cases := []migrationv1alpha1.SwiftMigrationPhase{
		migrationv1alpha1.SwiftMigrationPhasePending,
		migrationv1alpha1.SwiftMigrationPhaseValidating,
		migrationv1alpha1.SwiftMigrationPhasePreparing,
	}
	for _, phase := range cases {
		t.Run(string(phase), func(t *testing.T) {
			if !shouldCheckSourcePodUID(migWithPhase(phase)) {
				t.Errorf("phase %q: want true, got false", phase)
			}
		})
	}
}

func TestShouldCheckSourcePodUID_PostCutoverPhases_ReturnsFalse(t *testing.T) {
	// Resuming, Completed, Failed, Cancelled — the pod-replacement
	// signal is irrelevant once the cutover has crossed (Resuming) or
	// the migration is terminal.
	cases := []migrationv1alpha1.SwiftMigrationPhase{
		migrationv1alpha1.SwiftMigrationPhaseResuming,
		migrationv1alpha1.SwiftMigrationPhaseCompleted,
		migrationv1alpha1.SwiftMigrationPhaseFailed,
		migrationv1alpha1.SwiftMigrationPhaseCancelled,
	}
	for _, phase := range cases {
		t.Run(string(phase), func(t *testing.T) {
			if shouldCheckSourcePodUID(migWithPhase(phase)) {
				t.Errorf("phase %q: want false, got true", phase)
			}
		})
	}
}

func TestShouldCheckSourcePodUID_StopAndCopy_PreCutoverDetail_ReturnsTrue(t *testing.T) {
	// During StopAndCopy live substates BEFORE cutover starts, the
	// check must fire — these are exactly the phases where F4.2
	// pod-replacement detection is needed.
	preCutover := []string{
		migrationv1alpha1.PhaseDetailLiveIssuingRecv,
		migrationv1alpha1.PhaseDetailLiveDestReceiving,
		migrationv1alpha1.PhaseDetailLiveIssuingSend,
		migrationv1alpha1.PhaseDetailLiveTransferring,
		migrationv1alpha1.PhaseDetailLiveSrcCompleted,
	}
	for _, detail := range preCutover {
		t.Run(detail, func(t *testing.T) {
			mig := migWithPhase(migrationv1alpha1.SwiftMigrationPhaseStopAndCopy)
			mig.Status.PhaseDetail = detail
			if !shouldCheckSourcePodUID(mig) {
				t.Errorf("phaseDetail %q: want true, got false", detail)
			}
		})
	}
}

func TestShouldCheckSourcePodUID_StopAndCopy_CutoverDetails_ReturnsFalse(t *testing.T) {
	// Once phaseDetail crosses into cutover-step-1 or cutover-step-2
	// territory, source-pod-replacement detection MUST be off — the
	// source pod is intentionally being retired by the cutover.
	cutover := []string{
		migrationv1alpha1.PhaseDetailLiveCutoverPodRef,
		migrationv1alpha1.PhaseDetailLiveCutoverDeleteSrc,
	}
	for _, detail := range cutover {
		t.Run(detail, func(t *testing.T) {
			mig := migWithPhase(migrationv1alpha1.SwiftMigrationPhaseStopAndCopy)
			mig.Status.PhaseDetail = detail
			if shouldCheckSourcePodUID(mig) {
				t.Errorf("phaseDetail %q: want false (cutover-in-progress), got true", detail)
			}
		})
	}
}

func TestShouldCheckSourcePodUID_StopAndCopy_PodRefSwappedTrue_ReturnsFalse(t *testing.T) {
	// The triple-gate (architect C1 mitigation): even with a pre-
	// cutover-looking phaseDetail, if PodRefSwapped is True the
	// check is OFF. PodRefSwapped is the cutover-commit-point signal
	// derived from cluster state per Q3.3 (c) mitigation.
	mig := migWithPhase(migrationv1alpha1.SwiftMigrationPhaseStopAndCopy)
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveTransferring
	mig.Status.Conditions = []metav1.Condition{{
		Type:   migrationv1alpha1.SwiftMigrationConditionPodRefSwapped,
		Status: metav1.ConditionTrue,
	}}
	if shouldCheckSourcePodUID(mig) {
		t.Errorf("PodRefSwapped=True must gate the check off; got true")
	}
}

func TestShouldCheckSourcePodUID_StopAndCopy_PodRefSwappedFalseCondition_ReturnsTrue(t *testing.T) {
	// PodRefSwapped present but with Status=False (theoretical case;
	// the controller never writes it False in practice but the helper
	// must handle it). False means "not crossed yet"; check fires.
	mig := migWithPhase(migrationv1alpha1.SwiftMigrationPhaseStopAndCopy)
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveTransferring
	mig.Status.Conditions = []metav1.Condition{{
		Type:   migrationv1alpha1.SwiftMigrationConditionPodRefSwapped,
		Status: metav1.ConditionFalse,
	}}
	if !shouldCheckSourcePodUID(mig) {
		t.Errorf("PodRefSwapped=False must NOT gate; want true, got false")
	}
}

func TestShouldCheckSourcePodUID_StopAndCopy_OfflinePhaseDetail_ReturnsFalse(t *testing.T) {
	// Phase 1 offline mode never calls this helper, but if it did,
	// offline's phaseDetail strings ("awaiting destination pod
	// creation", etc.) would not match the live vocabulary. The
	// helper returns false; this is documented as expected behavior
	// since offline does not use this code path.
	mig := migWithPhase(migrationv1alpha1.SwiftMigrationPhaseStopAndCopy)
	mig.Status.PhaseDetail = "awaiting destination pod creation"
	if shouldCheckSourcePodUID(mig) {
		t.Errorf("offline phaseDetail must fall through to false")
	}
}

func TestIsPostCutover_True_WhenPodRefSwappedConditionTrue(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{
			Conditions: []metav1.Condition{{
				Type:   migrationv1alpha1.SwiftMigrationConditionPodRefSwapped,
				Status: metav1.ConditionTrue,
			}},
		},
	}
	if !isPostCutover(mig) {
		t.Errorf("PodRefSwapped=True must yield isPostCutover=true")
	}
}

func TestIsPostCutover_False_WhenConditionAbsent(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{}
	if isPostCutover(mig) {
		t.Errorf("absent condition must yield isPostCutover=false")
	}
}

func TestIsPostCutover_False_WhenConditionFalse(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{
			Conditions: []metav1.Condition{{
				Type:   migrationv1alpha1.SwiftMigrationConditionPodRefSwapped,
				Status: metav1.ConditionFalse,
			}},
		},
	}
	if isPostCutover(mig) {
		t.Errorf("PodRefSwapped=False must yield isPostCutover=false")
	}
}

func TestIsLiveMode_StatusModeLive_ReturnsTrue(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeLive},
	}
	if !isLiveMode(mig) {
		t.Errorf("status.Mode=live must yield isLiveMode=true")
	}
}

func TestIsLiveMode_StatusEmptySpecLive_ReturnsTrue(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{
		Spec: migrationv1alpha1.SwiftMigrationSpec{Mode: migrationv1alpha1.SwiftMigrationModeLive},
	}
	if !isLiveMode(mig) {
		t.Errorf("status empty + spec.Mode=live (initial entry) must yield true")
	}
}

func TestIsLiveMode_StatusOfflineSpecLive_ReturnsFalse(t *testing.T) {
	// Defensive: if status was already resolved to offline, spec.Mode
	// is irrelevant. The status is the source of truth post-Validating.
	mig := &migrationv1alpha1.SwiftMigration{
		Spec:   migrationv1alpha1.SwiftMigrationSpec{Mode: migrationv1alpha1.SwiftMigrationModeLive},
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	if isLiveMode(mig) {
		t.Errorf("status.Mode=offline must override spec.Mode=live; want false")
	}
}

func TestIsLiveMode_AutoMode_ReturnsFalse(t *testing.T) {
	// mode=auto stays in offline path until B2 implements resolution.
	mig := &migrationv1alpha1.SwiftMigration{
		Spec: migrationv1alpha1.SwiftMigrationSpec{Mode: migrationv1alpha1.SwiftMigrationModeAuto},
	}
	if isLiveMode(mig) {
		t.Errorf("spec.Mode=auto must NOT dispatch to live in B1; want false")
	}
}

func TestIsLiveMode_OfflineMode_ReturnsFalse(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{
		Spec: migrationv1alpha1.SwiftMigrationSpec{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	if isLiveMode(mig) {
		t.Errorf("spec.Mode=offline must yield isLiveMode=false")
	}
}

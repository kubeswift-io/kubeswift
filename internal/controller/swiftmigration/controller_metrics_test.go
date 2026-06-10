package swiftmigration

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/metrics"
)

func TestRecordMigrationTerminal_CounterByModeAndResult(t *testing.T) {
	cases := []struct {
		mode   migrationv1alpha1.SwiftMigrationMode
		phase  migrationv1alpha1.SwiftMigrationPhase
		result string
	}{
		{migrationv1alpha1.SwiftMigrationModeLive, migrationv1alpha1.SwiftMigrationPhaseCompleted, "completed"},
		{migrationv1alpha1.SwiftMigrationModeOffline, migrationv1alpha1.SwiftMigrationPhaseFailed, "failed"},
		{migrationv1alpha1.SwiftMigrationModeOffline, migrationv1alpha1.SwiftMigrationPhaseCancelled, "cancelled"},
	}
	for _, tc := range cases {
		before := testutil.ToFloat64(metrics.MigrationTotal.WithLabelValues(string(tc.mode), tc.result))
		recordMigrationTerminal(&migrationv1alpha1.SwiftMigrationStatus{Mode: tc.mode, Phase: tc.phase})
		if got := testutil.ToFloat64(metrics.MigrationTotal.WithLabelValues(string(tc.mode), tc.result)); got != before+1 {
			t.Errorf("MigrationTotal{%s,%s} = %v, want %v", tc.mode, tc.result, got, before+1)
		}
	}
}

func TestRecordMigrationTerminal_FailuresByReason(t *testing.T) {
	// A failed migration breaks down by the bounded failureReason enum.
	before := testutil.ToFloat64(metrics.MigrationFailuresTotal.WithLabelValues("live", "DstNeverReady"))
	recordMigrationTerminal(&migrationv1alpha1.SwiftMigrationStatus{
		Mode:          migrationv1alpha1.SwiftMigrationModeLive,
		Phase:         migrationv1alpha1.SwiftMigrationPhaseFailed,
		FailureReason: migrationv1alpha1.FailureReasonDstNeverReady,
	})
	if got := testutil.ToFloat64(metrics.MigrationFailuresTotal.WithLabelValues("live", "DstNeverReady")); got != before+1 {
		t.Errorf("MigrationFailuresTotal{live,DstNeverReady} = %v, want %v", got, before+1)
	}

	// Offline mode does not populate failureReason -> "Unknown".
	ub := testutil.ToFloat64(metrics.MigrationFailuresTotal.WithLabelValues("offline", "Unknown"))
	recordMigrationTerminal(&migrationv1alpha1.SwiftMigrationStatus{
		Mode:  migrationv1alpha1.SwiftMigrationModeOffline,
		Phase: migrationv1alpha1.SwiftMigrationPhaseFailed,
	})
	if got := testutil.ToFloat64(metrics.MigrationFailuresTotal.WithLabelValues("offline", "Unknown")); got != ub+1 {
		t.Errorf("MigrationFailuresTotal{offline,Unknown} = %v, want %v", got, ub+1)
	}

	// A completed migration must NOT touch the failures counter.
	cb := testutil.ToFloat64(metrics.MigrationFailuresTotal.WithLabelValues("live", "Unknown"))
	recordMigrationTerminal(&migrationv1alpha1.SwiftMigrationStatus{
		Mode:  migrationv1alpha1.SwiftMigrationModeLive,
		Phase: migrationv1alpha1.SwiftMigrationPhaseCompleted,
	})
	if got := testutil.ToFloat64(metrics.MigrationFailuresTotal.WithLabelValues("live", "Unknown")); got != cb {
		t.Errorf("a completed migration must not increment failures; %v -> %v", cb, got)
	}
}

func TestRecordMigrationTerminal_DowntimeOnlyOnCompleted(t *testing.T) {
	before := testutil.CollectAndCount(metrics.MigrationDowntimeSeconds)
	// A failed migration must NOT observe downtime.
	recordMigrationTerminal(&migrationv1alpha1.SwiftMigrationStatus{
		Mode:  migrationv1alpha1.SwiftMigrationModeLive,
		Phase: migrationv1alpha1.SwiftMigrationPhaseFailed,
	})
	// A completed migration with downtime observes it.
	d := metav1.Duration{Duration: 2500 * time.Millisecond}
	recordMigrationTerminal(&migrationv1alpha1.SwiftMigrationStatus{
		Mode:             migrationv1alpha1.SwiftMigrationModeLive,
		Phase:            migrationv1alpha1.SwiftMigrationPhaseCompleted,
		ObservedDowntime: &d,
	})
	if after := testutil.CollectAndCount(metrics.MigrationDowntimeSeconds); after < 1 {
		t.Errorf("downtime histogram should carry an observation after a completed migration; series=%d (before=%d)", after, before)
	}
}

func TestRecordMigrationTerminal_TransferOnlyOnCompletedWithDuration(t *testing.T) {
	before := testutil.CollectAndCount(metrics.MigrationTransferSeconds)
	// Failed migration: no transfer observation even if a duration is set.
	td := metav1.Duration{Duration: 38 * time.Second}
	recordMigrationTerminal(&migrationv1alpha1.SwiftMigrationStatus{
		Mode:                     migrationv1alpha1.SwiftMigrationModeLive,
		Phase:                    migrationv1alpha1.SwiftMigrationPhaseFailed,
		ObservedTransferDuration: &td,
	})
	// Completed migration without a transfer duration (offline): no observation.
	recordMigrationTerminal(&migrationv1alpha1.SwiftMigrationStatus{
		Mode:  migrationv1alpha1.SwiftMigrationModeOffline,
		Phase: migrationv1alpha1.SwiftMigrationPhaseCompleted,
	})
	// Completed live migration with a transfer duration: observed.
	recordMigrationTerminal(&migrationv1alpha1.SwiftMigrationStatus{
		Mode:                     migrationv1alpha1.SwiftMigrationModeLive,
		Phase:                    migrationv1alpha1.SwiftMigrationPhaseCompleted,
		ObservedTransferDuration: &td,
	})
	if after := testutil.CollectAndCount(metrics.MigrationTransferSeconds); after < 1 {
		t.Errorf("transfer histogram should carry an observation after a completed live migration; series=%d (before=%d)", after, before)
	}
}

// TestPersist_RecordsTerminalMetricOnce verifies the wiring: persist records the
// terminal metric exactly once on the non-terminal -> terminal transition, and
// a subsequent no-op persist (status unchanged) does not double-count.
func TestPersist_RecordsTerminalMetricOnce(t *testing.T) {
	scheme := preparingScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming // non-terminal
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	before := testutil.ToFloat64(metrics.MigrationTotal.WithLabelValues("offline", "completed"))

	status := mig.Status.DeepCopy()
	status.Mode = migrationv1alpha1.SwiftMigrationModeOffline
	status.Phase = migrationv1alpha1.SwiftMigrationPhaseCompleted
	if err := r.persist(context.Background(), mig, status); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := testutil.ToFloat64(metrics.MigrationTotal.WithLabelValues("offline", "completed")); got != before+1 {
		t.Fatalf("after terminal persist, counter = %v, want %v", got, before+1)
	}

	// Re-persist the same terminal status: DeepEqual -> no-op -> no double count.
	if err := r.persist(context.Background(), mig, mig.Status.DeepCopy()); err != nil {
		t.Fatalf("second persist: %v", err)
	}
	if got := testutil.ToFloat64(metrics.MigrationTotal.WithLabelValues("offline", "completed")); got != before+1 {
		t.Errorf("no-op persist must not double-count; counter = %v, want %v", got, before+1)
	}
}

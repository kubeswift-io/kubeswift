package swiftrestore

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

func histCount(t *testing.T, m prometheus.Metric) uint64 {
	t.Helper()
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return d.GetHistogram().GetSampleCount()
}

func restore(phase snapshotv1alpha1.SwiftRestorePhase, started, completed *metav1.Time) *snapshotv1alpha1.SwiftRestore {
	return &snapshotv1alpha1.SwiftRestore{
		Status: snapshotv1alpha1.SwiftRestoreStatus{Phase: phase, StartedAt: started, CompletedAt: completed},
	}
}

func TestRecordRestoreTerminal_CounterByResult(t *testing.T) {
	for _, tc := range []struct {
		phase  snapshotv1alpha1.SwiftRestorePhase
		result string
	}{
		{snapshotv1alpha1.SwiftRestorePhaseReady, "ready"},
		{snapshotv1alpha1.SwiftRestorePhaseFailed, "failed"},
	} {
		before := testutil.ToFloat64(metrics.RestoreTotal.WithLabelValues(tc.result))
		recordRestoreTerminal(restore(tc.phase, nil, nil))
		if got := testutil.ToFloat64(metrics.RestoreTotal.WithLabelValues(tc.result)); got != before+1 {
			t.Errorf("RestoreTotal{%s} = %v, want %v", tc.result, got, before+1)
		}
	}
	// A non-terminal phase records nothing.
	before := testutil.CollectAndCount(metrics.RestoreTotal)
	recordRestoreTerminal(restore(snapshotv1alpha1.SwiftRestorePhaseRestoring, nil, nil))
	if after := testutil.CollectAndCount(metrics.RestoreTotal); after != before {
		t.Errorf("a non-terminal restore must not record a counter; series %d -> %d", before, after)
	}
}

func TestRecordRestoreTerminal_SecondsOnlyOnReady(t *testing.T) {
	base := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	started := &metav1.Time{Time: base}
	completed := &metav1.Time{Time: base.Add(30 * time.Second)}

	// A FAILED restore — even with both timestamps — does not observe latency.
	before := histCount(t, metrics.RestoreSeconds)
	recordRestoreTerminal(restore(snapshotv1alpha1.SwiftRestorePhaseFailed, started, completed))
	if got := histCount(t, metrics.RestoreSeconds); got != before {
		t.Errorf("failed restore must not observe latency; %d -> %d", before, got)
	}
	// A READY restore observes start->complete latency.
	recordRestoreTerminal(restore(snapshotv1alpha1.SwiftRestorePhaseReady, started, completed))
	if got := histCount(t, metrics.RestoreSeconds); got != before+1 {
		t.Errorf("ready restore should observe latency; %d -> %d", before, got)
	}
}

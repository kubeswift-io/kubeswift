package swiftsnapshot

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

// histCount reads a histogram's accumulated sample count via the dto wire form.
func histCount(t *testing.T, m prometheus.Metric) uint64 {
	t.Helper()
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return d.GetHistogram().GetSampleCount()
}

func snap(backend snapshotv1alpha1.SnapshotBackendType, phase snapshotv1alpha1.SwiftSnapshotPhase) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		Spec:   snapshotv1alpha1.SwiftSnapshotSpec{Backend: snapshotv1alpha1.SwiftSnapshotBackend{Type: backend}},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{Phase: phase},
	}
}

func TestRecordSnapshotTerminal_CounterByBackendResult(t *testing.T) {
	cases := []struct {
		backend snapshotv1alpha1.SnapshotBackendType
		phase   snapshotv1alpha1.SwiftSnapshotPhase
		result  string
	}{
		{snapshotv1alpha1.SnapshotBackendLocal, snapshotv1alpha1.SwiftSnapshotPhaseReady, "ready"},
		{snapshotv1alpha1.SnapshotBackendS3, snapshotv1alpha1.SwiftSnapshotPhaseFailed, "failed"},
		{snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot, snapshotv1alpha1.SwiftSnapshotPhaseReady, "ready"},
	}
	for _, tc := range cases {
		before := testutil.ToFloat64(metrics.SnapshotTotal.WithLabelValues(string(tc.backend), tc.result))
		recordSnapshotTerminal(snap(tc.backend, tc.phase))
		if got := testutil.ToFloat64(metrics.SnapshotTotal.WithLabelValues(string(tc.backend), tc.result)); got != before+1 {
			t.Errorf("SnapshotTotal{%s,%s} = %v, want %v", tc.backend, tc.result, got, before+1)
		}
	}
	// A non-terminal phase records nothing.
	before := testutil.CollectAndCount(metrics.SnapshotTotal)
	recordSnapshotTerminal(snap(snapshotv1alpha1.SnapshotBackendLocal, snapshotv1alpha1.SwiftSnapshotPhaseCapturing))
	if after := testutil.CollectAndCount(metrics.SnapshotTotal); after != before {
		t.Errorf("a non-terminal snapshot must not record a counter; series %d -> %d", before, after)
	}
}

func TestRecordSnapshotTerminal_LatenciesOnlyOnReady(t *testing.T) {
	base := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	capH := metrics.SnapshotCaptureSeconds.WithLabelValues("s3").(prometheus.Metric)
	sizeH := metrics.SnapshotSizeBytes.WithLabelValues("s3").(prometheus.Metric)
	pauseH := metrics.SnapshotPauseWindowSeconds.WithLabelValues("s3").(prometheus.Metric)

	// A FAILED snapshot records the counter but observes no latency/size.
	capBefore, sizeBefore, pauseBefore := histCount(t, capH), histCount(t, sizeH), histCount(t, pauseH)
	uploadBefore := histCount(t, metrics.SnapshotUploadSeconds)
	failed := snap(snapshotv1alpha1.SnapshotBackendS3, snapshotv1alpha1.SwiftSnapshotPhaseFailed)
	failed.Status.CapturedAt = &metav1.Time{Time: base.Add(10 * time.Second)}
	failed.Status.TotalSizeBytes = 1 << 30
	recordSnapshotTerminal(failed)
	if histCount(t, capH) != capBefore || histCount(t, sizeH) != sizeBefore {
		t.Error("a failed snapshot must not observe capture latency or size")
	}

	// A READY snapshot observes capture latency, size, pause window, and upload.
	ready := snap(snapshotv1alpha1.SnapshotBackendS3, snapshotv1alpha1.SwiftSnapshotPhaseReady)
	ready.CreationTimestamp = metav1.NewTime(base)
	ready.Status.CapturedAt = &metav1.Time{Time: base.Add(10 * time.Second)}
	ready.Status.TotalSizeBytes = 2 << 30
	ready.Status.ObservedPauseWindowMs = 2500
	ready.Status.S3 = &snapshotv1alpha1.S3SnapshotStatus{UploadedAt: &metav1.Time{Time: base.Add(40 * time.Second)}}
	recordSnapshotTerminal(ready)
	if got := histCount(t, capH); got != capBefore+1 {
		t.Errorf("ready snapshot should observe capture latency; %d -> %d", capBefore, got)
	}
	if got := histCount(t, sizeH); got != sizeBefore+1 {
		t.Errorf("ready snapshot should observe size; %d -> %d", sizeBefore, got)
	}
	if got := histCount(t, pauseH); got != pauseBefore+1 {
		t.Errorf("ready snapshot should observe pause window; %d -> %d", pauseBefore, got)
	}
	if got := histCount(t, metrics.SnapshotUploadSeconds); got != uploadBefore+1 {
		t.Errorf("ready s3 snapshot should observe upload latency; %d -> %d", uploadBefore, got)
	}
}

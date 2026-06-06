package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// vmBootObserved tracks guests we've already observed for VMBootSeconds to avoid double-counting.
var vmBootObserved sync.Map

// MarkVMBootObserved returns true if this is the first observation for the key (avoids double-counting).
func MarkVMBootObserved(key string) bool {
	_, loaded := vmBootObserved.LoadOrStore(key, struct{}{})
	return !loaded
}

// UnmarkVMBootObserved clears the key when guest leaves Running so a future boot can be observed.
func UnmarkVMBootObserved(key string) {
	vmBootObserved.Delete(key)
}

var (
	GuestRunningTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kubeswift_guest_running_total",
			Help: "Number of SwiftGuest instances currently in Running phase",
		},
		[]string{"namespace"},
	)

	VMBootSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kubeswift_vm_boot_seconds",
			Help:    "Time in seconds from pod creation to GuestRunning=True",
			Buckets: []float64{5, 10, 20, 30, 60, 90, 120, 180},
		},
		[]string{"namespace"},
	)

	VMFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kubeswift_vm_failures_total",
			Help: "Total number of SwiftGuest VM failures",
		},
		[]string{"namespace", "reason"},
	)

	ImageImportSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kubeswift_image_import_seconds",
			Help:    "Time in seconds for SwiftImage import to reach Ready",
			Buckets: []float64{30, 60, 120, 300, 600, 900},
		},
		[]string{"namespace"},
	)

	// MigrationTotal counts SwiftMigrations that reached a terminal phase,
	// labelled by resolved mode (live/offline) and result
	// (completed/failed/cancelled). Recorded once per migration on the
	// non-terminal -> terminal transition.
	MigrationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kubeswift_migration_total",
			Help: "Total SwiftMigrations that reached a terminal phase, by mode and result",
		},
		[]string{"mode", "result"},
	)

	// MigrationDowntimeSeconds observes status.observedDowntime for completed
	// migrations (the operator-visible guest-unavailable window), by mode.
	// Live migrations sit near the low buckets (~1-3s); offline span the
	// tens-of-seconds range.
	MigrationDowntimeSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kubeswift_migration_downtime_seconds",
			Help:    "Observed guest downtime for completed SwiftMigrations, by mode",
			Buckets: []float64{0.5, 1, 2, 3, 5, 10, 20, 30, 45, 60, 90, 120},
		},
		[]string{"mode"},
	)

	// --- Snapshot / restore / clone metrics (Phase 5) ---
	// Recorded once per resource on the non-terminal -> terminal transition,
	// mirroring recordMigrationTerminal. Labels stay low-cardinality
	// (backend x result); no per-namespace label on the result-bearing series.

	// SnapshotTotal counts SwiftSnapshots that reached a terminal phase, by
	// backend (csi-volume-snapshot/local/s3) and result (ready/failed).
	SnapshotTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kubeswift_snapshot_total",
			Help: "Total SwiftSnapshots that reached a terminal phase, by backend and result",
		},
		[]string{"backend", "result"},
	)

	// SnapshotCaptureSeconds observes capture latency (capturedAt -
	// creationTimestamp) for successful snapshots, by backend.
	SnapshotCaptureSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kubeswift_snapshot_capture_seconds",
			Help:    "Time from SwiftSnapshot creation to capturedAt, by backend",
			Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 900},
		},
		[]string{"backend"},
	)

	// SnapshotPauseWindowSeconds observes the source-VM pause window during
	// capture (status.observedPauseWindowMs), by backend. Set for local/s3
	// (memory) captures; absent for csi-volume-snapshot (no pause).
	SnapshotPauseWindowSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kubeswift_snapshot_pause_window_seconds",
			Help:    "Source VM pause window during capture, by backend",
			Buckets: []float64{0.1, 0.5, 1, 2, 3, 5, 10, 20, 30, 45, 60},
		},
		[]string{"backend"},
	)

	// SnapshotUploadSeconds observes Tier C upload latency (s3.uploadedAt -
	// capturedAt). Unlabelled: s3 is the only backend that uploads.
	SnapshotUploadSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kubeswift_snapshot_upload_seconds",
			Help:    "Time from capturedAt to s3 uploadedAt for Tier C snapshots",
			Buckets: []float64{1, 5, 10, 20, 30, 60, 120, 300, 600, 900},
		},
	)

	// SnapshotSizeBytes observes total snapshot size (status.totalSizeBytes) at
	// Ready, by backend. Exponential buckets 64MiB .. ~128GiB.
	SnapshotSizeBytes = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kubeswift_snapshot_size_bytes",
			Help:    "Total SwiftSnapshot size at Ready, by backend",
			Buckets: prometheus.ExponentialBuckets(64*1024*1024, 2, 12),
		},
		[]string{"backend"},
	)

	// RestoreTotal counts SwiftRestores that reached a terminal phase, by
	// result (ready/failed). No backend label: a SwiftRestore does not carry
	// its source snapshot's backend, and a status-path Get to recover it isn't
	// worth the cost.
	RestoreTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kubeswift_restore_total",
			Help: "Total SwiftRestores that reached a terminal phase, by result",
		},
		[]string{"result"},
	)

	// RestoreSeconds observes restore latency (completedAt - startedAt) for
	// successful restores.
	RestoreSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kubeswift_restore_seconds",
			Help:    "Time from SwiftRestore startedAt to completedAt for successful restores",
			Buckets: []float64{5, 10, 20, 30, 60, 90, 120, 180, 300},
		},
	)

	// CloneTotal counts cloneFromSnapshot SwiftGuests by result, incremented on
	// the transition into Running ("running") or Failed ("failed") — riding the
	// SwiftGuest controller's existing phase-transition detection.
	CloneTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kubeswift_clone_total",
			Help: "cloneFromSnapshot SwiftGuests by result (running/failed)",
		},
		[]string{"result"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		GuestRunningTotal,
		VMBootSeconds,
		VMFailuresTotal,
		ImageImportSeconds,
		MigrationTotal,
		MigrationDowntimeSeconds,
		SnapshotTotal,
		SnapshotCaptureSeconds,
		SnapshotPauseWindowSeconds,
		SnapshotUploadSeconds,
		SnapshotSizeBytes,
		RestoreTotal,
		RestoreSeconds,
		CloneTotal,
	)
}

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

// kubeswift_guest_running_total moved to the StateCollector
// (state_collector.go): as an event-accumulated Inc/Dec gauge it silently
// drifted to 0 (or negative) across controller restarts. It is now emitted
// from cluster state at scrape time, DEPRECATED in favor of
// kubeswift_guests{phase="Running"}, and removed after one release.

var (
	VMBootSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kubeswift_vm_boot_seconds",
			Help:    "Time in seconds from pod creation to GuestRunning=True",
			Buckets: []float64{5, 10, 20, 30, 60, 90, 120, 180},
		},
		[]string{"namespace"},
	)

	// VMFailuresTotal counts transitions into Failed by condition Reason.
	// The reason label MUST carry the bounded machine token (condition
	// .Reason), never the free-text .Message — messages embed pod/node
	// names and error strings, which is unbounded series cardinality.
	VMFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kubeswift_vm_failures_total",
			Help: "Total number of SwiftGuest VM failures, by condition reason",
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

	// MigrationTransferSeconds observes status.observedTransferDuration for
	// completed live migrations (the swiftletd-reported send-migration RPC:
	// pre-copy iterations + final stop-and-copy + finalize), by mode. This is
	// the data-movement window — distinct from the operator-visible downtime
	// (MigrationDowntimeSeconds). Empirically ~38s for a 4Gi guest.
	MigrationTransferSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kubeswift_migration_transfer_seconds",
			Help:    "Observed state-transfer duration for completed SwiftMigrations, by mode",
			Buckets: []float64{5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600},
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

	// SnapshotUploadBytesTotal counts artifact bytes actually pushed to S3
	// (Tier C uploads), excluding resume-skipped objects — the wire-traffic
	// counter (vs status.s3.uploadedBytes, which is the snapshot's S3 footprint).
	SnapshotUploadBytesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "kubeswift_snapshot_upload_bytes_total",
			Help: "Artifact bytes pushed to S3 by Tier C snapshot uploads (excludes resume-skipped)",
		},
	)

	// RestoreDownloadBytesTotal counts artifact bytes actually pulled from S3
	// (Tier C restores + cloneFromSnapshot downloads), excluding skipped.
	RestoreDownloadBytesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "kubeswift_restore_download_bytes_total",
			Help: "Artifact bytes pulled from S3 by Tier C restore/clone downloads (excludes skipped)",
		},
	)

	// SnapshotSchedulePrunedTotal counts snapshots a SwiftSnapshotSchedule
	// deleted by keep-N retention (excludes those skipped because still
	// referenced).
	SnapshotSchedulePrunedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "kubeswift_snapshot_schedule_pruned_total",
			Help: "Snapshots deleted by SwiftSnapshotSchedule keep-N retention",
		},
	)
)

// cloneDownloadObserved dedupes the per-(node,snapshot) clone download byte
// report so it fires once per shared download Job (the SwiftGuest controller
// re-reads the completed Job every reconcile). In-memory, mirroring
// MarkVMBootObserved; a controller restart may re-count once (acceptable for a
// bandwidth counter).
var cloneDownloadObserved sync.Map

// MarkCloneDownloadObserved returns true the first time key is seen.
func MarkCloneDownloadObserved(key string) bool {
	_, loaded := cloneDownloadObserved.LoadOrStore(key, struct{}{})
	return !loaded
}

func init() {
	metrics.Registry.MustRegister(
		VMBootSeconds,
		VMFailuresTotal,
		ImageImportSeconds,
		MigrationTotal,
		MigrationDowntimeSeconds,
		MigrationTransferSeconds,
		SnapshotTotal,
		SnapshotCaptureSeconds,
		SnapshotPauseWindowSeconds,
		SnapshotUploadSeconds,
		SnapshotSizeBytes,
		RestoreTotal,
		RestoreSeconds,
		CloneTotal,
		SnapshotUploadBytesTotal,
		RestoreDownloadBytesTotal,
		SnapshotSchedulePrunedTotal,
	)
}

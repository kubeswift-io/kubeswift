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
)

func init() {
	metrics.Registry.MustRegister(
		GuestRunningTotal,
		VMBootSeconds,
		VMFailuresTotal,
		ImageImportSeconds,
		MigrationTotal,
		MigrationDowntimeSeconds,
	)
}

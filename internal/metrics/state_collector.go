package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// StateCollector exports CR-state gauges computed from the informer cache at
// scrape time (observability design doc D1). State gauges must NEVER be
// event-accumulated (Inc/Dec on phase transitions): after a controller
// restart the accumulator resets to zero while the cluster state does not,
// so the gauge silently drifts to 0 — or negative once a pre-restart guest
// stops. Computing from a List at scrape time is immune by construction.
//
// All series are aggregates with bounded labels (namespace/phase/node/...).
// Per-CR-name series are deliberately NOT exported (cardinality); operators
// who want them can layer kube-state-metrics CustomResourceStateMetrics on
// top (see config/samples/monitoring/).
//
// The reader is the manager's cached client: every listed type already has
// a running informer (each has a controller watching it), so Collect is an
// in-memory scan — no apiserver round-trips on the scrape path.
type StateCollector struct {
	reader client.Reader
}

// NewStateCollector builds a collector reading from the given cache-backed
// reader.
func NewStateCollector(reader client.Reader) *StateCollector {
	return &StateCollector{reader: reader}
}

// RegisterStateCollector registers the collector on the controller-runtime
// metrics registry. Call once from main after the manager is constructed.
func RegisterStateCollector(reader client.Reader) {
	metrics.Registry.MustRegister(NewStateCollector(reader))
}

// collectTimeout bounds the scrape-side cache scan. Cache reads are
// in-memory; this only guards the not-yet-synced window during startup.
const collectTimeout = 5 * time.Second

var (
	guestsDesc = prometheus.NewDesc(
		"kubeswift_guests",
		"SwiftGuests by namespace and phase, computed from cluster state at scrape time",
		[]string{"namespace", "phase"}, nil)
	guestsByNodeDesc = prometheus.NewDesc(
		"kubeswift_guests_by_node",
		"Scheduled SwiftGuests by node and hypervisor",
		[]string{"node", "hypervisor"}, nil)
	guestsByBootSourceDesc = prometheus.NewDesc(
		"kubeswift_guests_by_boot_source",
		"SwiftGuests by boot source (image|kernel|clone)",
		[]string{"source"}, nil)
	// Deprecated alias of sum(kubeswift_guests{phase="Running"}) by namespace.
	// Pre-O2 this was an event-accumulated gauge that drifted across
	// controller restarts; it is now emitted from cluster state with correct
	// semantics. Kept one release for dashboard compatibility — prefer
	// kubeswift_guests.
	guestRunningDesc = prometheus.NewDesc(
		"kubeswift_guest_running_total",
		"DEPRECATED (use kubeswift_guests{phase=\"Running\"}): Running SwiftGuests by namespace",
		[]string{"namespace"}, nil)
	poolReplicasDesc = prometheus.NewDesc(
		"kubeswift_pool_replicas",
		"SwiftGuestPool replica counts by state (desired|ready|available|failed|updated)",
		[]string{"namespace", "pool", "state"}, nil)
	imagesDesc = prometheus.NewDesc(
		"kubeswift_images",
		"SwiftImages by namespace and phase",
		[]string{"namespace", "phase"}, nil)
	kernelsDesc = prometheus.NewDesc(
		"kubeswift_kernels",
		"SwiftKernels by phase (cluster-wide)",
		[]string{"phase"}, nil)
	snapshotsDesc = prometheus.NewDesc(
		"kubeswift_snapshots",
		"SwiftSnapshots by namespace and phase",
		[]string{"namespace", "phase"}, nil)
	migrationsActiveDesc = prometheus.NewDesc(
		"kubeswift_migrations_active",
		"In-flight (non-terminal) SwiftMigrations by mode",
		[]string{"mode"}, nil)
	gpuNodeGPUsDesc = prometheus.NewDesc(
		"kubeswift_gpu_node_gpus",
		"GPUs per SwiftGPUNode by state (total|free)",
		[]string{"node", "state"}, nil)
	gpuNodeInfoDesc = prometheus.NewDesc(
		"kubeswift_gpu_node_info",
		"SwiftGPUNode inventory info (value is always 1)",
		[]string{"node", "model", "vfio_ready"}, nil)
	gpuNodeLastDiscoveryDesc = prometheus.NewDesc(
		"kubeswift_gpu_node_last_discovery_timestamp_seconds",
		"Unix time of the last successful GPU discovery per node",
		[]string{"node"}, nil)
	scheduleLastSuccessDesc = prometheus.NewDesc(
		"kubeswift_snapshot_schedule_last_success_timestamp_seconds",
		"Unix time a SwiftSnapshotSchedule's snapshot last reached Ready",
		[]string{"namespace", "schedule"}, nil)
)

var (
	guestPhases = []swiftv1alpha1.SwiftGuestPhase{
		swiftv1alpha1.SwiftGuestPhasePending,
		swiftv1alpha1.SwiftGuestPhaseScheduling,
		swiftv1alpha1.SwiftGuestPhaseRunning,
		swiftv1alpha1.SwiftGuestPhaseStopped,
		swiftv1alpha1.SwiftGuestPhaseFailed,
	}
	imagePhases = []imagev1alpha1.SwiftImagePhase{
		imagev1alpha1.SwiftImagePhasePending,
		imagev1alpha1.SwiftImagePhaseImporting,
		imagev1alpha1.SwiftImagePhaseValidating,
		imagev1alpha1.SwiftImagePhasePreparing,
		imagev1alpha1.SwiftImagePhaseSnapshotting,
		imagev1alpha1.SwiftImagePhaseReady,
		imagev1alpha1.SwiftImagePhaseFailed,
	}
	kernelPhases = []kernelv1alpha1.SwiftKernelPhase{
		kernelv1alpha1.SwiftKernelPhasePending,
		kernelv1alpha1.SwiftKernelPhasePulling,
		kernelv1alpha1.SwiftKernelPhaseReady,
		kernelv1alpha1.SwiftKernelPhaseFailed,
	}
	snapshotPhases = []snapshotv1alpha1.SwiftSnapshotPhase{
		snapshotv1alpha1.SwiftSnapshotPhasePending,
		snapshotv1alpha1.SwiftSnapshotPhaseCapturing,
		snapshotv1alpha1.SwiftSnapshotPhaseUploading,
		snapshotv1alpha1.SwiftSnapshotPhaseReady,
		snapshotv1alpha1.SwiftSnapshotPhaseFailed,
	}
)

// Describe implements prometheus.Collector.
func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- guestsDesc
	ch <- guestsByNodeDesc
	ch <- guestsByBootSourceDesc
	ch <- guestRunningDesc
	ch <- poolReplicasDesc
	ch <- imagesDesc
	ch <- kernelsDesc
	ch <- snapshotsDesc
	ch <- migrationsActiveDesc
	ch <- gpuNodeGPUsDesc
	ch <- gpuNodeInfoDesc
	ch <- gpuNodeLastDiscoveryDesc
	ch <- scheduleLastSuccessDesc
}

// Collect implements prometheus.Collector. Each type is collected
// independently: a List error (cache not synced yet during startup) skips
// that type's series for this scrape rather than failing the whole scrape.
func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	c.collectGuests(ctx, ch)
	c.collectPools(ctx, ch)
	c.collectImages(ctx, ch)
	c.collectKernels(ctx, ch)
	c.collectSnapshots(ctx, ch)
	c.collectMigrations(ctx, ch)
	c.collectGPUNodes(ctx, ch)
	c.collectSchedules(ctx, ch)
}

func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, labels...)
}

func (c *StateCollector) collectGuests(ctx context.Context, ch chan<- prometheus.Metric) {
	var list swiftv1alpha1.SwiftGuestList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	// Zero-fill every phase for every namespace that has at least one guest,
	// so `kubeswift_guests{phase="Failed"} > 0`-style alerts see an explicit
	// 0 instead of an absent series.
	byNSPhase := map[string]map[swiftv1alpha1.SwiftGuestPhase]int{}
	byNode := map[[2]string]int{}
	bySource := map[string]int{"image": 0, "kernel": 0, "clone": 0}
	for i := range list.Items {
		g := &list.Items[i]
		if byNSPhase[g.Namespace] == nil {
			byNSPhase[g.Namespace] = map[swiftv1alpha1.SwiftGuestPhase]int{}
		}
		byNSPhase[g.Namespace][g.Status.Phase]++

		if g.Status.NodeName != "" {
			hv := "unknown"
			if g.Status.Runtime != nil && g.Status.Runtime.Hypervisor != "" {
				hv = g.Status.Runtime.Hypervisor
			}
			byNode[[2]string{g.Status.NodeName, hv}]++
		}

		switch {
		case g.UsesCloneFromSnapshot():
			bySource["clone"]++
		case g.Spec.KernelRef != nil:
			bySource["kernel"]++
		case g.Spec.ImageRef != nil:
			bySource["image"]++
		}
	}
	for ns, phases := range byNSPhase {
		for _, p := range guestPhases {
			gauge(ch, guestsDesc, float64(phases[p]), ns, string(p))
		}
		gauge(ch, guestRunningDesc, float64(phases[swiftv1alpha1.SwiftGuestPhaseRunning]), ns)
	}
	for k, n := range byNode {
		gauge(ch, guestsByNodeDesc, float64(n), k[0], k[1])
	}
	for src, n := range bySource {
		gauge(ch, guestsByBootSourceDesc, float64(n), src)
	}
}

func (c *StateCollector) collectPools(ctx context.Context, ch chan<- prometheus.Metric) {
	var list swiftv1alpha1.SwiftGuestPoolList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		p := &list.Items[i]
		for state, v := range map[string]int32{
			"desired":   p.Spec.Replicas,
			"ready":     p.Status.ReadyReplicas,
			"available": p.Status.AvailableReplicas,
			"failed":    p.Status.FailedReplicas,
			"updated":   p.Status.UpdatedReplicas,
		} {
			gauge(ch, poolReplicasDesc, float64(v), p.Namespace, p.Name, state)
		}
	}
}

func (c *StateCollector) collectImages(ctx context.Context, ch chan<- prometheus.Metric) {
	var list imagev1alpha1.SwiftImageList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	byNSPhase := map[string]map[imagev1alpha1.SwiftImagePhase]int{}
	for i := range list.Items {
		img := &list.Items[i]
		if byNSPhase[img.Namespace] == nil {
			byNSPhase[img.Namespace] = map[imagev1alpha1.SwiftImagePhase]int{}
		}
		byNSPhase[img.Namespace][img.Status.Phase]++
	}
	for ns, phases := range byNSPhase {
		for _, p := range imagePhases {
			gauge(ch, imagesDesc, float64(phases[p]), ns, string(p))
		}
	}
}

func (c *StateCollector) collectKernels(ctx context.Context, ch chan<- prometheus.Metric) {
	var list kernelv1alpha1.SwiftKernelList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	byPhase := map[kernelv1alpha1.SwiftKernelPhase]int{}
	for i := range list.Items {
		byPhase[list.Items[i].Status.Phase]++
	}
	for _, p := range kernelPhases {
		gauge(ch, kernelsDesc, float64(byPhase[p]), string(p))
	}
}

func (c *StateCollector) collectSnapshots(ctx context.Context, ch chan<- prometheus.Metric) {
	var list snapshotv1alpha1.SwiftSnapshotList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	byNSPhase := map[string]map[snapshotv1alpha1.SwiftSnapshotPhase]int{}
	for i := range list.Items {
		s := &list.Items[i]
		if byNSPhase[s.Namespace] == nil {
			byNSPhase[s.Namespace] = map[snapshotv1alpha1.SwiftSnapshotPhase]int{}
		}
		byNSPhase[s.Namespace][s.Status.Phase]++
	}
	for ns, phases := range byNSPhase {
		for _, p := range snapshotPhases {
			gauge(ch, snapshotsDesc, float64(phases[p]), ns, string(p))
		}
	}
}

func (c *StateCollector) collectMigrations(ctx context.Context, ch chan<- prometheus.Metric) {
	var list migrationv1alpha1.SwiftMigrationList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	// live/offline are always emitted (0 when idle); "auto" appears only for
	// migrations the controller has not yet resolved (status.Mode empty).
	byMode := map[string]int{"live": 0, "offline": 0}
	for i := range list.Items {
		m := &list.Items[i]
		switch m.Status.Phase {
		case migrationv1alpha1.SwiftMigrationPhaseCompleted,
			migrationv1alpha1.SwiftMigrationPhaseFailed,
			migrationv1alpha1.SwiftMigrationPhaseCancelled:
			continue
		}
		mode := string(m.Status.Mode)
		if mode == "" {
			mode = string(m.Spec.Mode)
		}
		if mode == "" {
			mode = string(migrationv1alpha1.SwiftMigrationModeAuto)
		}
		byMode[mode]++
	}
	for mode, n := range byMode {
		gauge(ch, migrationsActiveDesc, float64(n), mode)
	}
}

func (c *StateCollector) collectGPUNodes(ctx context.Context, ch chan<- prometheus.Metric) {
	var list gpuv1alpha1.SwiftGPUNodeList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		n := &list.Items[i]
		gauge(ch, gpuNodeGPUsDesc, float64(n.Status.GPUCount), n.Name, "total")
		gauge(ch, gpuNodeGPUsDesc, float64(n.Status.FreeGPUs), n.Name, "free")
		vfio := "false"
		if n.Status.VfioReady {
			vfio = "true"
		}
		gauge(ch, gpuNodeInfoDesc, 1, n.Name, n.Status.GPUModel, vfio)
		if n.Status.LastDiscovery != nil {
			gauge(ch, gpuNodeLastDiscoveryDesc, float64(n.Status.LastDiscovery.Unix()), n.Name)
		}
	}
}

func (c *StateCollector) collectSchedules(ctx context.Context, ch chan<- prometheus.Metric) {
	var list snapshotv1alpha1.SwiftSnapshotScheduleList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		s := &list.Items[i]
		if s.Status.LastSuccessfulTime != nil {
			gauge(ch, scheduleLastSuccessDesc, float64(s.Status.LastSuccessfulTime.Unix()), s.Namespace, s.Name)
		}
	}
}

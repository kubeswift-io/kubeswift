package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func guest(ns, name string, phase swiftv1alpha1.SwiftGuestPhase, node, hypervisor string) *swiftv1alpha1.SwiftGuest {
	g := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef: &corev1.LocalObjectReference{Name: "img"},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{Phase: phase, NodeName: node},
	}
	if hypervisor != "" {
		g.Status.Runtime = &swiftv1alpha1.GuestRuntimeStatus{Hypervisor: hypervisor}
	}
	return g
}

func newStateReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
}

// TestStateCollector_GuestGauges proves the collector computes guest counts
// from cluster state at scrape time — including the deprecated
// kubeswift_guest_running_total alias — with per-namespace zero-fill for
// every phase.
func TestStateCollector_GuestGauges(t *testing.T) {
	c := NewStateCollector(newStateReader(t,
		guest("a", "g1", swiftv1alpha1.SwiftGuestPhaseRunning, "miles", "cloud-hypervisor"),
		guest("a", "g2", swiftv1alpha1.SwiftGuestPhaseRunning, "boba", "qemu"),
		guest("a", "g3", swiftv1alpha1.SwiftGuestPhaseFailed, "", ""),
		guest("b", "g4", swiftv1alpha1.SwiftGuestPhasePending, "", ""),
	))

	expected := `
# HELP kubeswift_guest_running_total DEPRECATED (use kubeswift_guests{phase="Running"}): Running SwiftGuests by namespace
# TYPE kubeswift_guest_running_total gauge
kubeswift_guest_running_total{namespace="a"} 2
kubeswift_guest_running_total{namespace="b"} 0
# HELP kubeswift_guests SwiftGuests by namespace and phase, computed from cluster state at scrape time
# TYPE kubeswift_guests gauge
kubeswift_guests{namespace="a",phase="Failed"} 1
kubeswift_guests{namespace="a",phase="Pending"} 0
kubeswift_guests{namespace="a",phase="Running"} 2
kubeswift_guests{namespace="a",phase="Scheduling"} 0
kubeswift_guests{namespace="a",phase="Stopped"} 0
kubeswift_guests{namespace="b",phase="Failed"} 0
kubeswift_guests{namespace="b",phase="Pending"} 1
kubeswift_guests{namespace="b",phase="Running"} 0
kubeswift_guests{namespace="b",phase="Scheduling"} 0
kubeswift_guests{namespace="b",phase="Stopped"} 0
# HELP kubeswift_guests_by_node Scheduled SwiftGuests by node and hypervisor
# TYPE kubeswift_guests_by_node gauge
kubeswift_guests_by_node{hypervisor="cloud-hypervisor",node="miles"} 1
kubeswift_guests_by_node{hypervisor="qemu",node="boba"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kubeswift_guests", "kubeswift_guests_by_node", "kubeswift_guest_running_total"); err != nil {
		t.Error(err)
	}
}

// TestStateCollector_SurvivesRestart is the restart-drift contract test. The
// pre-O2 implementation accumulated kubeswift_guest_running_total with
// Inc/Dec on phase transitions; a controller restart reset the accumulator
// to 0 while the cluster still had Running guests — the gauge silently
// reported 0 (Design Principle #6 violation). The collector computes from
// state, so a brand-new instance (= restarted controller) over the same
// cluster reports identical values with NO transitions ever observed.
func TestStateCollector_SurvivesRestart(t *testing.T) {
	reader := newStateReader(t,
		guest("prod", "g1", swiftv1alpha1.SwiftGuestPhaseRunning, "miles", "cloud-hypervisor"),
		guest("prod", "g2", swiftv1alpha1.SwiftGuestPhaseRunning, "boba", "cloud-hypervisor"),
	)

	first := NewStateCollector(reader)
	if got := testutil.CollectAndCount(first, "kubeswift_guest_running_total"); got != 1 {
		t.Fatalf("first collector: %d running_total series, want 1", got)
	}

	// "Restart": a fresh collector with zero observed history.
	restarted := NewStateCollector(reader)
	expected := `
# HELP kubeswift_guest_running_total DEPRECATED (use kubeswift_guests{phase="Running"}): Running SwiftGuests by namespace
# TYPE kubeswift_guest_running_total gauge
kubeswift_guest_running_total{namespace="prod"} 2
`
	if err := testutil.CollectAndCompare(restarted, strings.NewReader(expected),
		"kubeswift_guest_running_total"); err != nil {
		t.Errorf("post-restart collector must report cluster state, not accumulated transitions: %v", err)
	}
}

func TestStateCollector_PoolsImagesMigrationsGPU(t *testing.T) {
	lastDiscovery := metav1.Unix(1700000000, 0)
	lastSuccess := metav1.Unix(1700000100, 0)
	c := NewStateCollector(newStateReader(t,
		&swiftv1alpha1.SwiftGuestPool{
			ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "pool1"},
			Spec:       swiftv1alpha1.SwiftGuestPoolSpec{Replicas: 3},
			Status:     swiftv1alpha1.SwiftGuestPoolStatus{ReadyReplicas: 2, AvailableReplicas: 2, FailedReplicas: 1, UpdatedReplicas: 3},
		},
		&imagev1alpha1.SwiftImage{
			ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "img1"},
			Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseImporting},
		},
		&migrationv1alpha1.SwiftMigration{
			ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "m-live"},
			Status:     migrationv1alpha1.SwiftMigrationStatus{Phase: migrationv1alpha1.SwiftMigrationPhaseStopAndCopy, Mode: migrationv1alpha1.SwiftMigrationModeLive},
		},
		&migrationv1alpha1.SwiftMigration{
			ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "m-done"},
			Status:     migrationv1alpha1.SwiftMigrationStatus{Phase: migrationv1alpha1.SwiftMigrationPhaseCompleted, Mode: migrationv1alpha1.SwiftMigrationModeLive},
		},
		&gpuv1alpha1.SwiftGPUNode{
			ObjectMeta: metav1.ObjectMeta{Name: "boba"},
			Status: gpuv1alpha1.SwiftGPUNodeStatus{
				GPUCount: 1, FreeGPUs: 1, GPUModel: "GTX 1080", VfioReady: true,
				LastDiscovery: &lastDiscovery,
			},
		},
		&snapshotv1alpha1.SwiftSnapshotSchedule{
			ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "nightly"},
			Status:     snapshotv1alpha1.SwiftSnapshotScheduleStatus{LastSuccessfulTime: &lastSuccess},
		},
	))

	expected := `
# HELP kubeswift_gpu_node_gpus GPUs per SwiftGPUNode by state (total|free)
# TYPE kubeswift_gpu_node_gpus gauge
kubeswift_gpu_node_gpus{node="boba",state="free"} 1
kubeswift_gpu_node_gpus{node="boba",state="total"} 1
# HELP kubeswift_gpu_node_info SwiftGPUNode inventory info (value is always 1)
# TYPE kubeswift_gpu_node_info gauge
kubeswift_gpu_node_info{model="GTX 1080",node="boba",vfio_ready="true"} 1
# HELP kubeswift_gpu_node_last_discovery_timestamp_seconds Unix time of the last successful GPU discovery per node
# TYPE kubeswift_gpu_node_last_discovery_timestamp_seconds gauge
kubeswift_gpu_node_last_discovery_timestamp_seconds{node="boba"} 1.7e+09
# HELP kubeswift_migrations_active In-flight (non-terminal) SwiftMigrations by mode
# TYPE kubeswift_migrations_active gauge
kubeswift_migrations_active{mode="live"} 1
kubeswift_migrations_active{mode="offline"} 0
# HELP kubeswift_pool_replicas SwiftGuestPool replica counts by state (desired|ready|available|failed|updated)
# TYPE kubeswift_pool_replicas gauge
kubeswift_pool_replicas{namespace="a",pool="pool1",state="available"} 2
kubeswift_pool_replicas{namespace="a",pool="pool1",state="desired"} 3
kubeswift_pool_replicas{namespace="a",pool="pool1",state="failed"} 1
kubeswift_pool_replicas{namespace="a",pool="pool1",state="ready"} 2
kubeswift_pool_replicas{namespace="a",pool="pool1",state="updated"} 3
# HELP kubeswift_snapshot_schedule_last_success_timestamp_seconds Unix time a SwiftSnapshotSchedule's snapshot last reached Ready
# TYPE kubeswift_snapshot_schedule_last_success_timestamp_seconds gauge
kubeswift_snapshot_schedule_last_success_timestamp_seconds{namespace="a",schedule="nightly"} 1.7000001e+09
# HELP kubeswift_images SwiftImages by namespace and phase
# TYPE kubeswift_images gauge
kubeswift_images{namespace="a",phase="Failed"} 0
kubeswift_images{namespace="a",phase="Importing"} 1
kubeswift_images{namespace="a",phase="Pending"} 0
kubeswift_images{namespace="a",phase="Preparing"} 0
kubeswift_images{namespace="a",phase="Ready"} 0
kubeswift_images{namespace="a",phase="Snapshotting"} 0
kubeswift_images{namespace="a",phase="Validating"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"kubeswift_pool_replicas", "kubeswift_images", "kubeswift_migrations_active",
		"kubeswift_gpu_node_gpus", "kubeswift_gpu_node_info",
		"kubeswift_gpu_node_last_discovery_timestamp_seconds",
		"kubeswift_snapshot_schedule_last_success_timestamp_seconds"); err != nil {
		t.Error(err)
	}
}

// TestStateCollector_Lint keeps the exported series within Prometheus
// naming/label conventions.
func TestStateCollector_Lint(t *testing.T) {
	c := NewStateCollector(newStateReader(t,
		guest("a", "g1", swiftv1alpha1.SwiftGuestPhaseRunning, "miles", "cloud-hypervisor"),
	))
	problems, err := testutil.CollectAndLint(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		// The deprecated alias keeps its historical (lint-violating) name —
		// a gauge with a _total suffix — until removal; everything else
		// must lint clean. Drop this carve-out together with the alias.
		if p.Metric == "kubeswift_guest_running_total" {
			continue
		}
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}

package swiftdrain

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

const ns = "default"

// --- builders ---

func guest(name string, opts ...func(*swiftv1alpha1.SwiftGuest)) *swiftv1alpha1.SwiftGuest {
	g := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name + "-uid")},
		Spec:       swiftv1alpha1.SwiftGuestSpec{GuestClassRef: corev1.LocalObjectReference{Name: "small"}},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

func drain(node string) func(*swiftv1alpha1.SwiftGuest) {
	return func(g *swiftv1alpha1.SwiftGuest) {
		if g.Annotations == nil {
			g.Annotations = map[string]string{}
		}
		g.Annotations[swiftv1alpha1.AnnotationDrainRequested] = node
	}
}

func statusNode(node string) func(*swiftv1alpha1.SwiftGuest) {
	return func(g *swiftv1alpha1.SwiftGuest) { g.Status.NodeName = node }
}

func policy(p string) func(*swiftv1alpha1.SwiftGuest) {
	return func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Migration = &swiftv1alpha1.MigrationSpec{DrainPolicy: p}
	}
}

func vfio() func(*swiftv1alpha1.SwiftGuest) {
	return func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
	}
}

func inProgress(migName string) func(*swiftv1alpha1.SwiftGuest) {
	return func(g *swiftv1alpha1.SwiftGuest) {
		if g.Annotations == nil {
			g.Annotations = map[string]string{}
		}
		g.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] = migName
	}
}

func node(name string, opts ...func(*corev1.Node)) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	n.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("8"),
		corev1.ResourceMemory: resource.MustParse("16Gi"),
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

func cordoned(n *corev1.Node) { n.Spec.Unschedulable = true }
func notReady(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionFalse }
func smallClass() *swiftv1alpha1.SwiftGuestClass {
	return &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "small"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi")},
	}
}

func drainMig(name string, phase migrationv1alpha1.SwiftMigrationPhase) *migrationv1alpha1.SwiftMigration {
	return &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     migrationv1alpha1.SwiftMigrationStatus{Phase: phase},
	}
}

func newR(objs ...client.Object) (*Reconciler, client.Client) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	return &Reconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(32)}, c
}

func reconcileGuest(t *testing.T, r *Reconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func listMigs(t *testing.T, c client.Client) []migrationv1alpha1.SwiftMigration {
	t.Helper()
	var l migrationv1alpha1.SwiftMigrationList
	if err := c.List(context.Background(), &l); err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	return l.Items
}

func getGuest(t *testing.T, c client.Client, name string) *swiftv1alpha1.SwiftGuest {
	t.Helper()
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &g); err != nil {
		t.Fatalf("get guest: %v", err)
	}
	return &g
}

// --- tests ---

func TestReconcile_NoMarker_NoOp(t *testing.T) {
	r, c := newR(guest("g"), node("miles"), node("boba"), smallClass())
	reconcileGuest(t, r, "g")
	if migs := listMigs(t, c); len(migs) != 0 {
		t.Errorf("no marker → no migration; got %d", len(migs))
	}
}

func TestReconcile_GuestMovedOff_ClearsMarker(t *testing.T) {
	r, c := newR(guest("g", drain("miles"), statusNode("boba")), node("miles"), node("boba"), smallClass())
	reconcileGuest(t, r, "g")
	g := getGuest(t, c, "g")
	if _, ok := g.Annotations[swiftv1alpha1.AnnotationDrainRequested]; ok {
		t.Errorf("marker should be cleared once guest is off the draining node")
	}
	if migs := listMigs(t, c); len(migs) != 0 {
		t.Errorf("moved-off guest must not get a new migration; got %d", len(migs))
	}
}

func gpuProfileFixture() *gpuv1alpha1.SwiftGPUProfile {
	return &gpuv1alpha1.SwiftGPUProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns},
		Spec:       gpuv1alpha1.SwiftGPUProfileSpec{Count: 1, PartitionMode: "isolated"},
	}
}

func swiftGPUNode(name string, vfioReady bool, free int) *gpuv1alpha1.SwiftGPUNode {
	return &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     gpuv1alpha1.SwiftGPUNodeStatus{VfioReady: vfioReady, FreeGPUs: free, GPUModel: "GeForce GTX 1080"},
	}
}

func TestReconcile_GPUGuest_NoGPUTarget_NoMigration(t *testing.T) {
	// GPU guest, profile exists, but boba is not a GPU node (no SwiftGPUNode)
	// → no schedulable GPU target → no migration; marker stays.
	r, c := newR(guest("g", drain("miles"), statusNode("miles"), vfio()),
		node("miles"), node("boba"), smallClass(), gpuProfileFixture())
	res := reconcileGuest(t, r, "g")
	if migs := listMigs(t, c); len(migs) != 0 {
		t.Errorf("no GPU target → no migration; got %d", len(migs))
	}
	if res.RequeueAfter == 0 {
		t.Errorf("no GPU target should requeue")
	}
	if getGuest(t, c, "g").Annotations[swiftv1alpha1.AnnotationDrainRequested] != "miles" {
		t.Errorf("marker must stay when there is no GPU target")
	}
}

func TestReconcile_GPUGuest_CreatesOfflineMigration(t *testing.T) {
	// GPU guest with a vfio-ready GPU target (boba) → creates an offline
	// (auto→offline) migration via release-and-reallocate.
	r, c := newR(guest("g", drain("miles"), statusNode("miles"), vfio()),
		node("miles"), node("boba"), smallClass(), gpuProfileFixture(),
		swiftGPUNode("boba", true, 1))
	reconcileGuest(t, r, "g")
	migs := listMigs(t, c)
	if len(migs) != 1 {
		t.Fatalf("GPU guest with a vfio-ready GPU target should create 1 migration; got %d", len(migs))
	}
	if migs[0].Spec.Target.NodeName != "boba" {
		t.Errorf("target = %q, want boba (the vfio-ready GPU node)", migs[0].Spec.Target.NodeName)
	}
	if migs[0].Spec.Mode != migrationv1alpha1.SwiftMigrationModeAuto {
		t.Errorf("mode = %q, want auto (resolves to offline for VFIO)", migs[0].Spec.Mode)
	}
}

func TestReconcile_GPUGuest_NonVfioReadyTarget_NoMigration(t *testing.T) {
	// boba is a GPU node but NOT vfio-ready → GPUNodeHasCapacity rejects it →
	// no GPU target → no migration.
	r, c := newR(guest("g", drain("miles"), statusNode("miles"), vfio()),
		node("miles"), node("boba"), smallClass(), gpuProfileFixture(),
		swiftGPUNode("boba", false, 1))
	reconcileGuest(t, r, "g")
	if migs := listMigs(t, c); len(migs) != 0 {
		t.Errorf("a non-vfio-ready GPU node is not a valid target; got %d migrations", len(migs))
	}
}

func TestReconcile_CreatesMigration(t *testing.T) {
	r, c := newR(guest("g", drain("miles"), statusNode("miles")), node("miles"), node("boba"), smallClass())
	reconcileGuest(t, r, "g")

	migs := listMigs(t, c)
	if len(migs) != 1 {
		t.Fatalf("expected 1 migration created; got %d", len(migs))
	}
	m := migs[0]
	if want := drainMigrationName("g", "miles"); m.Name != want {
		t.Errorf("migration name = %q, want %q", m.Name, want)
	}
	if m.Spec.GuestRef.Name != "g" {
		t.Errorf("guestRef = %q, want g", m.Spec.GuestRef.Name)
	}
	if m.Spec.Target.NodeName != "boba" {
		t.Errorf("target = %q, want boba (the non-draining peer)", m.Spec.Target.NodeName)
	}
	if m.Spec.Mode != migrationv1alpha1.SwiftMigrationModeAuto {
		t.Errorf("mode = %q, want auto (default Migrate policy)", m.Spec.Mode)
	}
	if m.Spec.Reason != "node-drain" {
		t.Errorf("reason = %q, want node-drain", m.Spec.Reason)
	}
	if !m.Spec.AllowIPChange {
		t.Errorf("drain migration must set allowIPChange=true to avoid stalling on default networking")
	}
	if m.Spec.TTL == nil || m.Spec.TTL.Duration != drainMigrationTTL {
		t.Errorf("drain migration must set ttl=%s for auto-cleanup; got %v", drainMigrationTTL, m.Spec.TTL)
	}
	if len(m.OwnerReferences) != 1 || m.OwnerReferences[0].Name != "g" {
		t.Errorf("migration must be guest-owned; got %+v", m.OwnerReferences)
	}
}

func TestReconcile_LiveMigratePolicy_LiveMode(t *testing.T) {
	r, c := newR(guest("g", drain("miles"), statusNode("miles"), policy(swiftv1alpha1.DrainPolicyLiveMigrate)),
		node("miles"), node("boba"), smallClass())
	reconcileGuest(t, r, "g")
	migs := listMigs(t, c)
	if len(migs) != 1 {
		t.Fatalf("expected 1 migration; got %d", len(migs))
	}
	if migs[0].Spec.Mode != migrationv1alpha1.SwiftMigrationModeLive {
		t.Errorf("LiveMigrate policy → mode=live; got %q", migs[0].Spec.Mode)
	}
}

func TestReconcile_MigrationInFlight_NoDuplicate(t *testing.T) {
	existing := drainMig(drainMigrationName("g", "miles"), migrationv1alpha1.SwiftMigrationPhaseValidating)
	r, c := newR(guest("g", drain("miles"), statusNode("miles")), existing, node("miles"), node("boba"), smallClass())
	reconcileGuest(t, r, "g")
	if migs := listMigs(t, c); len(migs) != 1 {
		t.Errorf("in-flight migration must not be duplicated; got %d", len(migs))
	}
}

func TestReconcile_MigrationFailed_MarkerStays_NoNew(t *testing.T) {
	failed := drainMig(drainMigrationName("g", "miles"), migrationv1alpha1.SwiftMigrationPhaseFailed)
	failed.Status.FailureMessage = "boom"
	r, c := newR(guest("g", drain("miles"), statusNode("miles")), failed, node("miles"), node("boba"), smallClass())
	reconcileGuest(t, r, "g")
	if migs := listMigs(t, c); len(migs) != 1 {
		t.Errorf("failed migration must not trigger a new one (no retry storm); got %d", len(migs))
	}
	g := getGuest(t, c, "g")
	if g.Annotations[swiftv1alpha1.AnnotationDrainRequested] != "miles" {
		t.Errorf("marker must stay after a failed migration (VM stays protected)")
	}
}

func TestReconcile_NoTarget_Requeue(t *testing.T) {
	// Only the draining node exists → no schedulable peer.
	r, c := newR(guest("g", drain("miles"), statusNode("miles")), node("miles"), smallClass())
	res := reconcileGuest(t, r, "g")
	if migs := listMigs(t, c); len(migs) != 0 {
		t.Errorf("no target → no migration; got %d", len(migs))
	}
	if res.RequeueAfter == 0 {
		t.Errorf("no-target should requeue to retry when capacity frees up")
	}
}

func TestReconcile_CordonedPeer_NoTarget(t *testing.T) {
	r, c := newR(guest("g", drain("miles"), statusNode("miles")), node("miles"), node("boba", cordoned), smallClass())
	reconcileGuest(t, r, "g")
	if migs := listMigs(t, c); len(migs) != 0 {
		t.Errorf("cordoned peer is not a valid target; got %d migrations", len(migs))
	}
}

func TestReconcile_NotReadyPeer_NoTarget(t *testing.T) {
	r, c := newR(guest("g", drain("miles"), statusNode("miles")), node("miles"), node("boba", notReady), smallClass())
	reconcileGuest(t, r, "g")
	if migs := listMigs(t, c); len(migs) != 0 {
		t.Errorf("not-Ready peer is not a valid target; got %d migrations", len(migs))
	}
}

func TestReconcile_OtherMigrationInProgress_Defers(t *testing.T) {
	r, c := newR(guest("g", drain("miles"), statusNode("miles"), inProgress("some-other-mig")),
		node("miles"), node("boba"), smallClass())
	res := reconcileGuest(t, r, "g")
	if migs := listMigs(t, c); len(migs) != 0 {
		t.Errorf("must not create a second migration while one is in flight; got %d", len(migs))
	}
	if res.RequeueAfter == 0 {
		t.Errorf("should requeue while deferring to the in-flight migration")
	}
}

func TestDrainMigrationName_DeterministicAndBounded(t *testing.T) {
	a := drainMigrationName("guest", "miles")
	if a != drainMigrationName("guest", "miles") {
		t.Errorf("name must be deterministic for the same (guest, node)")
	}
	if a == drainMigrationName("guest", "boba") {
		t.Errorf("name must differ across draining nodes")
	}
	long := ""
	for i := 0; i < 80; i++ {
		long += "x"
	}
	n := drainMigrationName(long, "miles")
	if len(n) > maxMigrationNameLen {
		t.Errorf("name %q (len %d) exceeds the %d-char label-value bound", n, len(n), maxMigrationNameLen)
	}
}

func TestDrainMode(t *testing.T) {
	if drainMode(swiftv1alpha1.DrainPolicyLiveMigrate) != migrationv1alpha1.SwiftMigrationModeLive {
		t.Errorf("LiveMigrate → live")
	}
	if drainMode(swiftv1alpha1.DrainPolicyMigrate) != migrationv1alpha1.SwiftMigrationModeAuto {
		t.Errorf("Migrate → auto")
	}
	if drainMode("") != migrationv1alpha1.SwiftMigrationModeAuto {
		t.Errorf("empty → auto (default)")
	}
}

// guard: a SwiftMigration get error other than NotFound is surfaced.
func TestObserveMigration_InProgress_NoOp(t *testing.T) {
	r, _ := newR()
	g := guest("g", drain("miles"))
	m := drainMig("x", migrationv1alpha1.SwiftMigrationPhasePreparing)
	res, err := r.observeMigration(context.Background(), g, m, "miles")
	if err != nil {
		t.Fatalf("observeMigration: %v", err)
	}
	if res.RequeueAfter != 0 || res.Requeue {
		t.Errorf("in-progress migration: rely on the Owns watch, no explicit requeue; got %+v", res)
	}
}

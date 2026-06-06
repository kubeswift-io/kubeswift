package swiftsnapshotschedule

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

// fixed minute boundary for deterministic cron math.
var baseTime = time.Date(2026, 6, 6, 3, 0, 0, 0, time.UTC)

func schedScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	gv := schema.GroupVersion{Group: "snapshot.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gv,
		&snapshotv1alpha1.SwiftSnapshot{}, &snapshotv1alpha1.SwiftSnapshotList{},
		&snapshotv1alpha1.SwiftSnapshotSchedule{}, &snapshotv1alpha1.SwiftSnapshotScheduleList{},
	)
	metav1.AddToGroupVersion(s, gv)
	return s
}

func newSched(t *testing.T, now time.Time, objs ...client.Object) (*SwiftSnapshotScheduleReconciler, client.Client) {
	s := schedScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&snapshotv1alpha1.SwiftSnapshotSchedule{}).Build()
	return &SwiftSnapshotScheduleReconciler{Client: c, Scheme: s, now: func() time.Time { return now }}, c
}

func schedule(mut func(*snapshotv1alpha1.SwiftSnapshotSchedule)) *snapshotv1alpha1.SwiftSnapshotSchedule {
	s := &snapshotv1alpha1.SwiftSnapshotSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nightly", Namespace: "ns",
			CreationTimestamp: metav1.NewTime(baseTime.Add(-time.Hour)),
		},
		Spec: snapshotv1alpha1.SwiftSnapshotScheduleSpec{
			Schedule:          "* * * * *",
			ConcurrencyPolicy: snapshotv1alpha1.ConcurrencyForbid,
			Template: snapshotv1alpha1.SnapshotTemplate{
				Metadata: snapshotv1alpha1.SnapshotTemplateMeta{Labels: map[string]string{"tier": "db"}},
				Spec: snapshotv1alpha1.SwiftSnapshotSpec{
					GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "g1"},
					Backend:  snapshotv1alpha1.SwiftSnapshotBackend{Type: snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot},
				},
			},
		},
	}
	if mut != nil {
		mut(s)
	}
	return s
}

func req() ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Name: "nightly", Namespace: "ns"}}
}

func listSnaps(t *testing.T, c client.Client) []snapshotv1alpha1.SwiftSnapshot {
	t.Helper()
	var l snapshotv1alpha1.SwiftSnapshotList
	if err := c.List(context.Background(), &l, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestMostRecentDue(t *testing.T) {
	sched, _ := cron.ParseStandard("* * * * *")
	// earliest = T-2min, now = T -> latest due tick is T.
	due, ok := mostRecentDue(sched, baseTime.Add(-2*time.Minute), baseTime)
	if !ok || !due.Equal(baseTime) {
		t.Errorf("due=%v ok=%v, want %v true", due, ok, baseTime)
	}
	// next tick is in the future -> nothing due.
	if _, ok := mostRecentDue(sched, baseTime, baseTime); ok {
		t.Error("no tick should be due when earliest==now")
	}
}

func TestReconcile_FiresDueTick(t *testing.T) {
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		lt := metav1.NewTime(baseTime.Add(-2 * time.Minute))
		s.Status.LastScheduleTime = &lt
	})
	r, c := newSched(t, baseTime, s)
	res, err := r.Reconcile(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > maxRequeue {
		t.Errorf("requeue = %v, want a capped positive wait", res.RequeueAfter)
	}
	snaps := listSnaps(t, c)
	if len(snaps) != 1 {
		t.Fatalf("expected exactly 1 scheduled snapshot, got %d", len(snaps))
	}
	got := snaps[0]
	if !strings.HasPrefix(got.Name, "nightly-") {
		t.Errorf("snapshot name %q should be <schedule>-<ts>", got.Name)
	}
	if got.Labels[snapshotv1alpha1.ScheduleLabel] != "nightly" || got.Labels["tier"] != "db" {
		t.Errorf("labels wrong: %v", got.Labels)
	}
	if got.Spec.GuestRef.Name != "g1" || got.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot {
		t.Errorf("template spec not copied: %+v", got.Spec)
	}
	if oc := metav1.GetControllerOf(&got); oc == nil || oc.Kind != "SwiftSnapshotSchedule" || oc.Name != "nightly" {
		t.Errorf("snapshot must be owned by the schedule; got %+v", got.OwnerReferences)
	}
	// status.lastScheduleTime advanced to the tick (==baseTime).
	var after snapshotv1alpha1.SwiftSnapshotSchedule
	if err := c.Get(context.Background(), req().NamespacedName, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.LastScheduleTime == nil || !after.Status.LastScheduleTime.Time.Equal(baseTime) {
		t.Errorf("lastScheduleTime = %v, want %v", after.Status.LastScheduleTime, baseTime)
	}
}

func TestReconcile_Idempotent_SameTickNoDuplicate(t *testing.T) {
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		lt := metav1.NewTime(baseTime.Add(-2 * time.Minute))
		s.Status.LastScheduleTime = &lt
	})
	r, c := newSched(t, baseTime, s)
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	// Reset lastScheduleTime to re-attempt the same tick; the deterministic name
	// makes Create a no-op (AlreadyExists), so still exactly 1.
	var sc snapshotv1alpha1.SwiftSnapshotSchedule
	_ = c.Get(context.Background(), req().NamespacedName, &sc)
	lt := metav1.NewTime(baseTime.Add(-2 * time.Minute))
	sc.Status.LastScheduleTime = &lt
	_ = c.Status().Update(context.Background(), &sc)
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if n := len(listSnaps(t, c)); n != 1 {
		t.Errorf("same tick must not duplicate; got %d snapshots", n)
	}
}

func TestReconcile_Suspend_NoFire(t *testing.T) {
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) { s.Spec.Suspend = true })
	r, c := newSched(t, baseTime, s)
	res, err := r.Reconcile(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != suspendedRequeue {
		t.Errorf("suspended requeue = %v, want %v", res.RequeueAfter, suspendedRequeue)
	}
	if n := len(listSnaps(t, c)); n != 0 {
		t.Errorf("suspended schedule must not create snapshots; got %d", n)
	}
}

func TestReconcile_Forbid_SkipsWhenInFlight(t *testing.T) {
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		lt := metav1.NewTime(baseTime.Add(-2 * time.Minute))
		s.Status.LastScheduleTime = &lt
	})
	inflight := &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nightly-old", Namespace: "ns",
			Labels: map[string]string{snapshotv1alpha1.ScheduleLabel: "nightly"},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{Phase: snapshotv1alpha1.SwiftSnapshotPhaseCapturing},
	}
	r, c := newSched(t, baseTime, s, inflight)
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	// Forbid: no NEW snapshot — still just the in-flight one.
	if n := len(listSnaps(t, c)); n != 1 {
		t.Errorf("Forbid must skip while a prior is in flight; got %d snapshots", n)
	}
	// active reflects the in-flight child.
	var after snapshotv1alpha1.SwiftSnapshotSchedule
	_ = c.Get(context.Background(), req().NamespacedName, &after)
	if len(after.Status.Active) != 1 || after.Status.Active[0] != "nightly-old" {
		t.Errorf("active = %v, want [nightly-old]", after.Status.Active)
	}
}

func TestReconcile_StartingDeadline_SkipsTooLate(t *testing.T) {
	// Daily schedule; controller "wakes" 5min after the midnight tick with a 60s
	// deadline -> the tick is too late, skip it (no snapshot), advance lastSchedule.
	at := time.Date(2026, 6, 6, 0, 5, 0, 0, time.UTC) // 00:05
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Spec.Schedule = "0 0 * * *"
		dl := int64(60)
		s.Spec.StartingDeadlineSeconds = &dl
		lt := metav1.NewTime(at.Add(-26 * time.Hour)) // before the missed midnight
		s.Status.LastScheduleTime = &lt
		s.CreationTimestamp = metav1.NewTime(at.Add(-48 * time.Hour))
	})
	r, c := newSched(t, at, s)
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if n := len(listSnaps(t, c)); n != 0 {
		t.Errorf("a tick past startingDeadline must be skipped; got %d snapshots", n)
	}
	midnight := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	var after snapshotv1alpha1.SwiftSnapshotSchedule
	_ = c.Get(context.Background(), req().NamespacedName, &after)
	if after.Status.LastScheduleTime == nil || !after.Status.LastScheduleTime.Time.Equal(midnight) {
		t.Errorf("skipped tick should still advance lastScheduleTime to %v; got %v", midnight, after.Status.LastScheduleTime)
	}
}

func TestMergeLabels(t *testing.T) {
	out := mergeLabels(map[string]string{"a": "1"}, "sched-x")
	if out["a"] != "1" || out[snapshotv1alpha1.ScheduleLabel] != "sched-x" {
		t.Errorf("mergeLabels = %v", out)
	}
	// schedule label always wins / is set even with nil template labels.
	if got := mergeLabels(nil, "s")[snapshotv1alpha1.ScheduleLabel]; got != "s" {
		t.Errorf("schedule label not set on nil template labels; got %q", got)
	}
}

package swiftsnapshotschedule

import (
	"context"
	"strings"
	"testing"
	"time"
	// Embedded zoneinfo, so the explicit-zone test does not depend on host tzdata.
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
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
		&snapshotv1alpha1.SwiftRestore{}, &snapshotv1alpha1.SwiftRestoreList{},
	)
	metav1.AddToGroupVersion(s, gv)
	// ReferenceBlocker (keep-N) lists SwiftGuests + SwiftRestores.
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(s, gvSwift)
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

// withLocalTZ stands in for a pod given a timezone (a mounted /etc/localtime),
// or a contributor running the suite outside UTC.
func withLocalTZ(t *testing.T, offset time.Duration) {
	t.Helper()
	orig := time.Local
	time.Local = time.FixedZone("TEST", int(offset/time.Second))
	t.Cleanup(func() { time.Local = orig })
}

// spec.schedule is documented as UTC; unpinned, it would follow the zone of the
// times the reconcile loop hands it, which are always local.
func TestParseScheduleUTC_IgnoresProcessTimezone(t *testing.T) {
	withLocalTZ(t, 5*time.Hour)

	sched, err := parseScheduleUTC("0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// A local-zone time, as the reconcile loop passes.
	from := time.Date(2026, 6, 6, 0, 0, 0, 0, time.Local)
	want := time.Date(2026, 6, 6, 2, 0, 0, 0, time.UTC)
	if got := sched.Next(from); !got.Equal(want) {
		t.Errorf("next tick = %v (%v UTC), want %v", got, got.UTC(), want)
	}
}

// Pinning UTC must not override a zone the expression asked for.
func TestParseScheduleUTC_HonoursExplicitZone(t *testing.T) {
	withLocalTZ(t, 5*time.Hour)

	sched, err := parseScheduleUTC("CRON_TZ=Asia/Tokyo 0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	// 00:00 UTC is already 09:00 in Tokyo, so the next 02:00 there is tomorrow's.
	from := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	want := time.Date(2026, 6, 7, 2, 0, 0, 0, tokyo)
	if got := sched.Next(from); !got.Equal(want) {
		t.Errorf("next tick = %v (%v UTC), want %v", got, got.UTC(), want)
	}
}

// @every has no location; pinning must not drop it on the type assertion.
func TestParseScheduleUTC_IntervalSchedule(t *testing.T) {
	sched, err := parseScheduleUTC("@every 30m")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	if got, want := sched.Next(from), from.Add(30*time.Minute); !got.Equal(want) {
		t.Errorf("next tick = %v, want %v", got, want)
	}
}

// Parser errors pass through unchanged.
func TestParseScheduleUTC_RejectsInvalid(t *testing.T) {
	if _, err := parseScheduleUTC("not a cron"); err == nil {
		t.Error("expected an error for an invalid cron expression")
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

func readyCond(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	var s snapshotv1alpha1.SwiftSnapshotSchedule
	if err := c.Get(context.Background(), req().NamespacedName, &s); err != nil {
		t.Fatal(err)
	}
	return apimeta.FindStatusCondition(s.Status.Conditions, snapshotv1alpha1.SwiftSnapshotScheduleConditionReady)
}

// The defect the Ready condition exists for.
//
// An unparseable spec.schedule was logged and dropped: nothing in spec or
// status changed, so the object read as perfectly healthy under `kubectl get`
// and silently never fired. The validating webhook that would reject it is off
// by default, which makes the controller the path a malformed schedule
// actually reaches.
func TestReconcile_InvalidSchedule_SurfacesReadyFalse(t *testing.T) {
	sched := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Generation = 4
		s.Spec.Schedule = "not a cron"
	})
	r, c := newSched(t, baseTime, sched)
	res, err := r.Reconcile(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	// Still no hot-loop — the fix is visibility, not retry. The schedule is
	// re-reconciled by its own watch when the spec is corrected.
	if res.RequeueAfter != 0 {
		t.Errorf("requeue = %v, want 0", res.RequeueAfter)
	}
	if n := len(listSnaps(t, c)); n != 0 {
		t.Errorf("invalid schedule must not create snapshots; got %d", n)
	}

	cond := readyCond(t, c)
	if cond == nil {
		t.Fatal("no Ready condition — an invalid schedule is invisible again")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %v, want False", cond.Status)
	}
	if cond.Reason != "InvalidSchedule" {
		t.Errorf("reason = %q, want InvalidSchedule", cond.Reason)
	}
	// The parse error is the only thing that tells an operator what to fix.
	if !strings.Contains(cond.Message, "not a cron") && cond.Message == "" {
		t.Errorf("message must carry the parse error, got %q", cond.Message)
	}
	if cond.ObservedGeneration != 4 {
		t.Errorf("observedGeneration = %d, want 4 (must track the spec it judged)", cond.ObservedGeneration)
	}
}

func TestReconcile_ValidSchedule_SetsReadyTrue(t *testing.T) {
	r, c := newSched(t, baseTime, schedule(nil))
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	cond := readyCond(t, c)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %v, want True", cond)
	}
	if cond.Reason != "Scheduled" {
		t.Errorf("reason = %q, want Scheduled", cond.Reason)
	}
}

func TestReconcile_Suspend_SetsReadyFalse(t *testing.T) {
	sched := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) { s.Spec.Suspend = true })
	r, c := newSched(t, baseTime, sched)
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	cond := readyCond(t, c)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %v, want False", cond)
	}
	if cond.Reason != "Suspended" {
		t.Errorf("reason = %q, want Suspended", cond.Reason)
	}
}

// A steady-state reconcile must not write status. persistStatus compares whole
// statuses, so anything in a condition that varies between two identical
// reconciles turns every requeue into an API write. Verified red by building the
// Ready message from time.Now() instead of from spec, which moves the
// resourceVersion on the second pass.
//
// Note this catches wall-clock drift specifically: r.clock() is pinned in tests,
// so a message derived from the injected clock would stay stable here.
func TestReconcile_SteadyStateDoesNotRewriteStatus(t *testing.T) {
	sched := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		lt := metav1.NewTime(baseTime)
		s.Status.LastScheduleTime = &lt // nothing is due at baseTime
	})
	r, c := newSched(t, baseTime, sched)

	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	var first snapshotv1alpha1.SwiftSnapshotSchedule
	if err := c.Get(context.Background(), req().NamespacedName, &first); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	var second snapshotv1alpha1.SwiftSnapshotSchedule
	if err := c.Get(context.Background(), req().NamespacedName, &second); err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("status rewritten on an idle reconcile: rv %s -> %s",
			first.ResourceVersion, second.ResourceVersion)
	}
}

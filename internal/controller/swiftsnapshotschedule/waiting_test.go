package swiftsnapshotschedule

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

// pendingChild is a snapshot of the schedule, Pending with the given reason.
func pendingChild(name string, created time.Time, reason, msg string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns", CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{snapshotv1alpha1.ScheduleLabel: "nightly"},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{
			Phase: snapshotv1alpha1.SwiftSnapshotPhasePending,
			Conditions: []metav1.Condition{{
				Type: snapshotv1alpha1.SwiftSnapshotConditionReady, Status: metav1.ConditionFalse,
				Reason: reason, Message: msg, LastTransitionTime: metav1.NewTime(created),
			}},
		},
	}
}

func scheduleAfterReconcile(t *testing.T, s *snapshotv1alpha1.SwiftSnapshotSchedule, children ...*snapshotv1alpha1.SwiftSnapshot) (*metav1.Condition, int) {
	t.Helper()
	objs := []client.Object{s}
	for _, c := range children {
		objs = append(objs, c)
	}
	r, c := newSched(t, baseTime, objs...)
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	var got snapshotv1alpha1.SwiftSnapshotSchedule
	if err := c.Get(context.Background(), req().NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if errs := metav1validation.ValidateConditions(got.Status.Conditions, field.NewPath("status", "conditions")); len(errs) > 0 {
		t.Fatalf("the apiserver would refuse these conditions: %v", errs.ToAggregate())
	}
	return readyCond(t, c), len(listSnaps(t, c))
}

// A snapshot of the schedule waits for its storage: with Forbid every tick
// is skipped, and the schedule's own Ready says why, naming the snapshot and
// what it waits for.
func TestReconcile_WaitingChildIsMirrored(t *testing.T) {
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		lt := metav1.NewTime(baseTime.Add(-2 * time.Minute))
		s.Status.LastScheduleTime = &lt
	})
	child := pendingChild("nightly-1", baseTime.Add(-3*time.Minute), storagelocation.ReasonCredentialsMissing,
		"the registry credentials Secret kubeswift-registry does not exist in namespace ns: create it")
	cond, n := scheduleAfterReconcile(t, s, child)
	if n != 1 {
		t.Errorf("Forbid must skip the tick while the child waits; %d snapshots", n)
	}
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != storagelocation.ReasonCredentialsMissing {
		t.Fatalf("Ready = %+v, want False/%s", cond, storagelocation.ReasonCredentialsMissing)
	}
	for _, want := range []string{"SwiftSnapshot nightly-1 is waiting", "kubeswift-registry", "Forbid"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q should mention %q", cond.Message, want)
		}
	}
}

// Several waiting: the oldest is named, so the message does not change with
// list order. With Allow the message says new snapshots wait too.
func TestReconcile_WaitingChild_OldestAndAllow(t *testing.T) {
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Spec.ConcurrencyPolicy = snapshotv1alpha1.ConcurrencyAllow
		lt := metav1.NewTime(baseTime)
		s.Status.LastScheduleTime = &lt
	})
	newer := pendingChild("nightly-b", baseTime.Add(-time.Minute), storagelocation.ReasonNone, "no storage location")
	older := pendingChild("nightly-a", baseTime.Add(-2*time.Minute), storagelocation.ReasonNone, "no storage location")
	cond, _ := scheduleAfterReconcile(t, s, newer, older)
	if cond == nil || cond.Reason != storagelocation.ReasonNone || !strings.Contains(cond.Message, "nightly-a") || !strings.Contains(cond.Message, "wait the same way") {
		t.Errorf("Ready = %+v", cond)
	}
}

// A child that is Pending for another reason (its guest is still starting)
// is not the schedule's to report.
func TestReconcile_OtherPendingReasonsNotMirrored(t *testing.T) {
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		lt := metav1.NewTime(baseTime)
		s.Status.LastScheduleTime = &lt
	})
	cond, _ := scheduleAfterReconcile(t, s, pendingChild("nightly-1", baseTime.Add(-time.Minute), "GuestNotReady", "still provisioning"))
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Scheduled" {
		t.Errorf("Ready = %+v, want True/Scheduled", cond)
	}
}

// Once the child goes on, the schedule is Scheduled again.
func TestReconcile_WaitingChildClears(t *testing.T) {
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		lt := metav1.NewTime(baseTime)
		s.Status.LastScheduleTime = &lt
	})
	child := pendingChild("nightly-1", baseTime.Add(-time.Minute), storagelocation.ReasonNone, "no storage location")
	r, c := newSched(t, baseTime, s, child)
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if cond := readyCond(t, c); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False while the child waits", cond)
	}
	var cur snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(child), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	cur.Status.Conditions[0].Reason = "Capturing"
	if err := c.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if cond := readyCond(t, c); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Scheduled" {
		t.Errorf("Ready = %+v, want True/Scheduled once the child goes on", cond)
	}
}

// A template may leave the registry to a storage location: each snapshot
// carries the template's locationRef (or nothing) and resolves at its own
// creation, so it follows a changed default.
func TestReconcile_TemplateWithoutBackendOCI(t *testing.T) {
	s := schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Spec.Template.Spec.Backend = snapshotv1alpha1.SwiftSnapshotBackend{
			Type:        snapshotv1alpha1.SnapshotBackendOCI,
			LocationRef: &storagev1alpha1.StorageLocationRef{Name: "team"},
		}
	})
	r, c := newSched(t, baseTime, s)
	if _, err := r.Reconcile(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	snaps := listSnaps(t, c)
	if len(snaps) != 1 {
		t.Fatalf("%d snapshots, want 1", len(snaps))
	}
	b := snaps[0].Spec.Backend
	if b.OCI != nil || b.LocationRef == nil || b.LocationRef.Name != "team" {
		t.Errorf("created snapshot backend = %+v, want the template's locationRef and no oci block", b)
	}
}

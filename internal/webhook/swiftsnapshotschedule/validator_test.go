package swiftsnapshotschedule

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func sched(mut func(*snapshotv1alpha1.SwiftSnapshotSchedule)) *snapshotv1alpha1.SwiftSnapshotSchedule {
	s := &snapshotv1alpha1.SwiftSnapshotSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "ns"},
		Spec: snapshotv1alpha1.SwiftSnapshotScheduleSpec{
			Schedule: "0 2 * * *",
			Template: snapshotv1alpha1.SnapshotTemplate{
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

func errHas(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want containing %q", err, want)
	}
}

func TestValidate_OK(t *testing.T) {
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), sched(nil)); err != nil {
		t.Errorf("valid schedule rejected: %v", err)
	}
}

func TestValidate_BadCron(t *testing.T) {
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), sched(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Spec.Schedule = "not a cron"
	}))
	errHas(t, err, "not a valid")
}

func TestValidate_NameTooLong(t *testing.T) {
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), sched(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Name = strings.Repeat("x", maxScheduleNameLen+1)
	}))
	errHas(t, err, "at most")
}

func TestValidate_KeepLastAndDeadlineBounds(t *testing.T) {
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), sched(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Spec.Retention = &snapshotv1alpha1.SnapshotScheduleRetention{KeepLast: ptr.To(int32(0))}
	}))
	errHas(t, err, "keepLast must be >= 1")

	_, err = v.ValidateCreate(context.Background(), sched(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Spec.StartingDeadlineSeconds = ptr.To(int64(-5))
	}))
	errHas(t, err, "startingDeadlineSeconds must be >= 0")
}

func TestValidate_BadTemplate(t *testing.T) {
	v := &Validator{}
	// Missing guestRef → caught by the reused SwiftSnapshot shape validation.
	_, err := v.ValidateCreate(context.Background(), sched(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Spec.Template.Spec.GuestRef.Name = ""
	}))
	errHas(t, err, "template.spec is invalid")

	// local backend missing hostPath → also a template shape error.
	_, err = v.ValidateCreate(context.Background(), sched(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Spec.Template.Spec.Backend = snapshotv1alpha1.SwiftSnapshotBackend{Type: snapshotv1alpha1.SnapshotBackendLocal}
	}))
	errHas(t, err, "template.spec is invalid")
}

func TestValidate_UpdateRunsSameRules(t *testing.T) {
	v := &Validator{}
	bad := sched(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) { s.Spec.Schedule = "bogus" })
	if _, err := v.ValidateUpdate(context.Background(), sched(nil), bad); err == nil {
		t.Error("ValidateUpdate must re-validate the (mutable) spec")
	}
	// ValidateDelete is a pass-through.
	if _, err := v.ValidateDelete(context.Background(), sched(nil)); err != nil {
		t.Errorf("ValidateDelete should pass through; got %v", err)
	}
}

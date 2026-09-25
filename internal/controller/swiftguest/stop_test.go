package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// stopFixture is a reconciler over a guest and its launcher that records every
// pod it is asked to delete.
type stopFixture struct {
	r       *SwiftGuestReconciler
	rec     *record.FakeRecorder
	deleted []string
}

func newStopFixture(g *swiftv1alpha1.SwiftGuest, objs ...client.Object) *stopFixture {
	f := &stopFixture{rec: record.NewFakeRecorder(10)}
	c := guestClientBuilder(append([]client.Object{g, testGuestClass(), readyKernel()}, objs...)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod {
					f.deleted = append(f.deleted, obj.GetName())
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	f.r = &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme, Recorder: f.rec}
	return f
}

// stoppedGuest is the test guest set to runPolicy Stopped while its launcher
// (pod UID pod-1) still runs.
func stoppedGuest() *swiftv1alpha1.SwiftGuest {
	g := kernelGuest()
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	g.Status = *ranStatus("pod-1")
	return g
}

func liveLauncher() *corev1.Pod {
	p := runningLauncher("worker-1")
	p.UID = "pod-1"
	return p
}

func wantEvent(t *testing.T, rec *record.FakeRecorder, want string) {
	t.Helper()
	select {
	case got := <-rec.Events:
		if got != want {
			t.Errorf("event:\n got  %q\n want %q", got, want)
		}
	default:
		t.Errorf("no event; want %q", want)
	}
}

func noEvent(t *testing.T, rec *record.FakeRecorder) {
	t.Helper()
	select {
	case got := <-rec.Events:
		t.Errorf("unexpected event %q", got)
	default:
	}
}

// THE reported bug: runPolicy set to Stopped with kubectl left the guest
// running. The controller now shuts its launcher down, as swiftctl stop does,
// and then records the guest Stopped.
func TestReconcile_StoppedGuestWithRunningLauncherIsShutDown(t *testing.T) {
	f := newStopFixture(stoppedGuest(), liveLauncher())

	_, res, err := reconcileGuest(t, f.r)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != testGuestName {
		t.Fatalf("deleted pods %v, want [%s]", f.deleted, testGuestName)
	}
	if !res.Requeue && res.RequeueAfter == 0 {
		t.Error("no requeue while the launcher shuts down")
	}
	wantEvent(t, f.rec, "Normal Stopping runPolicy is Stopped; deleted launcher pod g, the guest shuts down within its termination grace period")

	got, _, err := reconcileGuest(t, f.r) // the launcher is gone
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseStopped {
		t.Errorf("phase = %q, want Stopped", got.Status.Phase)
	}
	if len(f.deleted) != 1 {
		t.Errorf("deleted pods %v; the launcher was deleted once", f.deleted)
	}
	noEvent(t, f.rec)
}

// After a live migration the launcher is <guest>-mig-<uid>, named by
// status.podRef; that is the pod to shut down.
func TestReconcile_StoppedGuestShutsDownItsMigratedLauncher(t *testing.T) {
	g := stoppedGuest()
	g.Status.PodRef.Name = testGuestName + "-mig-abcd"
	pod := liveLauncher()
	pod.Name = testGuestName + "-mig-abcd"
	f := newStopFixture(g, pod)

	if _, _, err := reconcileGuest(t, f.r); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != pod.Name {
		t.Errorf("deleted pods %v, want [%s]", f.deleted, pod.Name)
	}
}

// A launcher already on its way out is waited for, not deleted again.
func TestReconcile_StoppedGuestDoesNotDeleteATerminatingLauncherAgain(t *testing.T) {
	pod := liveLauncher()
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"test.kubeswift.io/hold"} // the fake client needs one to keep a deleted pod
	f := newStopFixture(stoppedGuest(), pod)

	_, res, err := reconcileGuest(t, f.r)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted pods %v; the launcher was already terminating", f.deleted)
	}
	if res.RequeueAfter == 0 {
		t.Error("no requeue while the launcher shuts down")
	}
	noEvent(t, f.rec)
}

// Another operation that owns the launcher is not interrupted: the stop waits
// for it, says what it waits for once, and looks again. An operation that set
// runPolicy Stopped itself deletes the launcher on its own; nothing is waiting
// on it then, and nothing is said.
func TestReconcile_StoppedGuestWaitsForTheOperationThatOwnsIt(t *testing.T) {
	offlineClaim := map[string]string{migrationv1alpha1.AnnotationMigrationInProgress: "m"}
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		op          client.Object
		event       string // "" when the operation stops the guest itself
	}{
		{
			name: "migration in flight",
			op: &migrationv1alpha1.SwiftMigration{
				ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
				Spec:       migrationv1alpha1.SwiftMigrationSpec{GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: testGuestName}},
				Status:     migrationv1alpha1.SwiftMigrationStatus{Phase: migrationv1alpha1.SwiftMigrationPhaseStopAndCopy},
			},
			event: "SwiftMigration m",
		},
		{
			name: "migration not yet picked up",
			op: &migrationv1alpha1.SwiftMigration{
				ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
				Spec:       migrationv1alpha1.SwiftMigrationSpec{GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: testGuestName}},
			},
			event: "SwiftMigration m",
		},
		{
			name:        "offline migration that stopped the guest itself",
			annotations: offlineClaim,
			op: &migrationv1alpha1.SwiftMigration{
				ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
				Spec:       migrationv1alpha1.SwiftMigrationSpec{GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: testGuestName}},
				Status:     migrationv1alpha1.SwiftMigrationStatus{Phase: migrationv1alpha1.SwiftMigrationPhasePreparing},
			},
		},
		{
			name: "restore replacing the launcher",
			op: &snapshotv1alpha1.SwiftRestore{
				ObjectMeta: metav1.ObjectMeta{Name: "rst", Namespace: "ns"},
				Spec:       snapshotv1alpha1.SwiftRestoreSpec{TargetGuest: snapshotv1alpha1.SwiftRestoreTarget{Name: testGuestName}},
				Status:     snapshotv1alpha1.SwiftRestoreStatus{Phase: snapshotv1alpha1.SwiftRestorePhaseRestoring},
			},
			event: "SwiftRestore rst",
		},
		{
			name: "snapshot capturing",
			op: &snapshotv1alpha1.SwiftSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns"},
				Spec:       snapshotv1alpha1.SwiftSnapshotSpec{GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: testGuestName}},
				Status:     snapshotv1alpha1.SwiftSnapshotStatus{Phase: snapshotv1alpha1.SwiftSnapshotPhaseCapturing},
			},
			event: "SwiftSnapshot snap",
		},
		{
			name: "full-state snapshot terminating the source",
			op: &snapshotv1alpha1.SwiftSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns"},
				Spec: snapshotv1alpha1.SwiftSnapshotSpec{
					GuestRef:    snapshotv1alpha1.SwiftSnapshotGuestRef{Name: testGuestName},
					IncludeDisk: true,
				},
				Status: snapshotv1alpha1.SwiftSnapshotStatus{Phase: snapshotv1alpha1.SwiftSnapshotPhaseUploading},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := stoppedGuest()
			g.Annotations = tc.annotations
			f := newStopFixture(g, liveLauncher(), tc.op)

			for pass := 0; pass < 2; pass++ {
				got, res, err := reconcileGuest(t, f.r)
				if err != nil {
					t.Fatal(err)
				}
				if len(f.deleted) != 0 {
					t.Fatalf("deleted pods %v under %s", f.deleted, tc.event)
				}
				if res.RequeueAfter == 0 {
					t.Errorf("pass %d: no requeue while the stop waits", pass)
				}
				if got.Status.Phase == swiftv1alpha1.SwiftGuestPhaseStopped {
					t.Errorf("pass %d: phase Stopped while the launcher runs", pass)
				}
			}
			if pods := launcherPods(t, f.r.Client); len(pods) != 1 {
				t.Errorf("%d launcher pods, want the one still running", len(pods))
			}
			if tc.event != "" {
				wantEvent(t, f.rec, "Normal StopDeferred runPolicy is Stopped; waiting for "+tc.event+" to finish before stopping the guest")
			}
			noEvent(t, f.rec) // said once, not on every pass
		})
	}
}

// Operations that are over, have not reached the launcher, or do not need it
// hold nothing up.
func TestReconcile_StoppedGuestIsNotHeldByOperationsThatDoNotOwnIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   client.Object
	}{
		{"completed migration", &migrationv1alpha1.SwiftMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
			Spec:       migrationv1alpha1.SwiftMigrationSpec{GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: testGuestName}},
			Status:     migrationv1alpha1.SwiftMigrationStatus{Phase: migrationv1alpha1.SwiftMigrationPhaseCompleted},
		}},
		{"another guest's migration", &migrationv1alpha1.SwiftMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
			Spec:       migrationv1alpha1.SwiftMigrationSpec{GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: "other"}},
			Status:     migrationv1alpha1.SwiftMigrationStatus{Phase: migrationv1alpha1.SwiftMigrationPhaseStopAndCopy},
		}},
		{"restore waiting for its snapshot", &snapshotv1alpha1.SwiftRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "rst", Namespace: "ns"},
			Spec:       snapshotv1alpha1.SwiftRestoreSpec{TargetGuest: snapshotv1alpha1.SwiftRestoreTarget{Name: testGuestName}},
			Status:     snapshotv1alpha1.SwiftRestoreStatus{Phase: snapshotv1alpha1.SwiftRestorePhasePending},
		}},
		{"pending snapshot", &snapshotv1alpha1.SwiftSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns"},
			Spec:       snapshotv1alpha1.SwiftSnapshotSpec{GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: testGuestName}},
			Status:     snapshotv1alpha1.SwiftSnapshotStatus{Phase: snapshotv1alpha1.SwiftSnapshotPhasePending},
		}},
		{"memory snapshot uploading", &snapshotv1alpha1.SwiftSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns"},
			Spec:       snapshotv1alpha1.SwiftSnapshotSpec{GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: testGuestName}},
			Status:     snapshotv1alpha1.SwiftSnapshotStatus{Phase: snapshotv1alpha1.SwiftSnapshotPhaseUploading},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStopFixture(stoppedGuest(), liveLauncher(), tc.op)
			if _, _, err := reconcileGuest(t, f.r); err != nil {
				t.Fatal(err)
			}
			if len(f.deleted) != 1 {
				t.Errorf("deleted pods %v, want the launcher", f.deleted)
			}
		})
	}
}

// A launcher that handed its VM to a live migration has exited, but the guest
// runs on in the destination: while the migration is in flight the guest is
// neither reported Stopped nor touched.
func TestReconcile_StoppedGuestIsNotStoppedByItsHandedOffLauncher(t *testing.T) {
	pod := liveLauncher()
	pod.Status.Phase = corev1.PodSucceeded
	pod.Annotations = map[string]string{PodAnnotationMigrationStatus: "complete"}
	mig := &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec:       migrationv1alpha1.SwiftMigrationSpec{GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: testGuestName}},
		Status:     migrationv1alpha1.SwiftMigrationStatus{Phase: migrationv1alpha1.SwiftMigrationPhaseStopAndCopy},
	}
	f := newStopFixture(stoppedGuest(), pod, mig)

	got, _, err := reconcileGuest(t, f.r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == swiftv1alpha1.SwiftGuestPhaseStopped {
		t.Error("phase Stopped while the VM runs in the migration's destination")
	}
	if c := guestCondition(t, got, "GuestRunning"); c.Status != metav1.ConditionTrue {
		t.Errorf("GuestRunning = %s (%s); the destination's report was cleared", c.Status, c.Reason)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted pods %v during the migration", f.deleted)
	}
}

// No launcher: the guest is Stopped, as before, with nothing to delete.
func TestReconcile_StoppedGuestWithNoLauncherIsStopped(t *testing.T) {
	f := newStopFixture(stoppedGuest())

	got, res, err := reconcileGuest(t, f.r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseStopped {
		t.Errorf("phase = %q, want Stopped", got.Status.Phase)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("result = %+v; a stopped guest has nothing to wait for", res)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted pods %v", f.deleted)
	}
	noEvent(t, f.rec)
}

// A running guest's launcher is left alone.
func TestReconcile_RunningGuestLauncherIsLeftAlone(t *testing.T) {
	g := stoppedGuest()
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning
	f := newStopFixture(g, liveLauncher())

	got, _, err := reconcileGuest(t, f.r)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted pods %v of a running guest", f.deleted)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	noEvent(t, f.rec)
}

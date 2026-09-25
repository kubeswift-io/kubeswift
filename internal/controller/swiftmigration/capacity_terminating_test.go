package swiftmigration

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// t4Message is the failure lab validation of v0.15.0 (round 3, T4) saw: an
// 8-CPU target running 5710m of pods plus a cancelled migration's 2-CPU
// destination pod, still terminating.
const t4Message = `target node "worker-2" has insufficient CPU headroom: need 2, have 290m (allocatable 8, used 7710m)`

// podOnNode is a running pod on node requesting cpu and mem.
func podOnNode(name, node, cpu, mem string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(cpu),
						corev1.ResourceMemory: resource.MustParse(mem),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// terminatingPodOnNode is podOnNode with a graceful delete in progress. The
// fake client keeps an object with a deletionTimestamp only while a finalizer
// holds it; releaseTerminatingPod lets it go.
func terminatingPodOnNode(name, node, cpu, mem string) *corev1.Pod {
	p := podOnNode(name, node, cpu, mem)
	now := metav1.Now()
	p.DeletionTimestamp = &now
	p.Finalizers = []string{"test.kubeswift.io/hold"}
	return p
}

// releaseTerminatingPod finishes a terminating pod's deletion.
func releaseTerminatingPod(t *testing.T, c client.Client, name string) {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &p); err != nil {
		t.Fatalf("get pod %q: %v", name, err)
	}
	p.Finalizers = nil
	if err := c.Update(context.Background(), &p); err != nil {
		t.Fatalf("release pod %q: %v", name, err)
	}
}

func compatibleCondition(status *migrationv1alpha1.SwiftMigrationStatus) *metav1.Condition {
	return apimeta.FindStatusCondition(status.Conditions, migrationv1alpha1.SwiftMigrationConditionCompatible)
}

// NodeHasCapacity tells a node that fits once pods being deleted there are
// gone (TerminatingPodsError) from one that does not fit, with the same
// message either way.
func TestNodeHasCapacity_TerminatingPods(t *testing.T) {
	cases := []struct {
		name            string
		pods            []client.Object
		wantErr         string // "" = fits
		wantTerminating bool
	}{
		{
			name: "fits",
			pods: []client.Object{podOnNode("busy", "worker-2", "4", "1Gi")},
		},
		{
			name:    "short, running pods only",
			pods:    []client.Object{podOnNode("busy", "worker-2", "5710m", "1Gi"), podOnNode("prev-dst", "worker-2", "2", "1Gi")},
			wantErr: t4Message,
		},
		{
			name:            "short only by a terminating pod's CPU",
			pods:            []client.Object{podOnNode("busy", "worker-2", "5710m", "1Gi"), terminatingPodOnNode("prev-dst", "worker-2", "2", "1Gi")},
			wantErr:         t4Message,
			wantTerminating: true,
		},
		{
			name: "short even without the terminating pod",
			pods: []client.Object{podOnNode("busy", "worker-2", "7", "1Gi"), terminatingPodOnNode("prev-dst", "worker-2", "500m", "1Gi")},
			wantErr: `target node "worker-2" has insufficient CPU headroom: need 2, have 500m ` +
				`(allocatable 8, used 7500m)`,
		},
		{
			name: "short only by a terminating pod's memory",
			pods: []client.Object{podOnNode("busy", "worker-2", "1", "60Gi"), terminatingPodOnNode("prev-dst", "worker-2", "1", "3Gi")},
			wantErr: `target node "worker-2" has insufficient memory headroom: need 2560Mi, have 1Gi ` +
				`(allocatable 64Gi, used 63Gi)`,
			wantTerminating: true,
		},
		{
			// CPU would fit without the terminating pod, memory would not:
			// the shortfall is not only the terminating pod's.
			name:    "CPU freed by a terminating pod, memory short regardless",
			pods:    []client.Object{podOnNode("busy", "worker-2", "5710m", "63Gi"), terminatingPodOnNode("prev-dst", "worker-2", "2", "512Mi")},
			wantErr: t4Message,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{newSpaciousNode("worker-2", 8, 65536)}, tc.pods...)
			c := fake.NewClientBuilder().WithScheme(validatingScheme(t)).WithObjects(objs...).Build()
			node := newSpaciousNode("worker-2", 8, 65536)
			class := newGuestClass("class-default", 2, 2048)

			err := NodeHasCapacity(context.Background(), c, node, class)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("NodeHasCapacity = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("NodeHasCapacity = %v, want %q", err, tc.wantErr)
			}
			var terminating *TerminatingPodsError
			if got := errors.As(err, &terminating); got != tc.wantTerminating {
				t.Errorf("errors.As(TerminatingPodsError) = %v, want %v", got, tc.wantTerminating)
			}
		})
	}
}

// validatingT4 builds the T4 cluster: target worker-2 fits the guest's 2 CPUs
// only once the cancelled migration's destination pod, still terminating, is
// gone.
func validatingT4(t *testing.T, mig *migrationv1alpha1.SwiftMigration, extra ...client.Object) (*SwiftMigrationReconciler, client.Client) {
	t.Helper()
	scheme := validatingScheme(t)
	objs := append([]client.Object{
		mig,
		newGuestForValidating("guest", "default", "class-default"),
		newGuestClass("class-default", 2, 2048),
		newSpaciousNode("worker-2", 8, 65536),
		podOnNode("busy", "worker-2", "5710m", "1Gi"),
		terminatingPodOnNode("guest-mig-prev", "worker-2", "2", "1Gi"),
	}, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(mig).Build()
	return &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}, c
}

// A target short only by a terminating pod's requests keeps the migration in
// Validating, waiting, instead of failing it; once the pod is gone the
// migration goes on.
func TestValidating_TerminatingPodOnTarget_WaitsThenAdvances(t *testing.T) {
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
	r, c := validatingT4(t, mig)

	status := mig.Status.DeepCopy()
	result := r.handleValidating(context.Background(), mig, status)
	if result.Err != nil || result.FailureMsg != "" || result.Advanced {
		t.Fatalf("result = %+v, want a requeue in Validating", result)
	}
	if result.Requeue != terminatingPodsPollInterval {
		t.Errorf("requeue = %s, want %s", result.Requeue, terminatingPodsPollInterval)
	}
	if status.PhaseDetail != phaseDetailAwaitingTerminatingPods {
		t.Errorf("phaseDetail = %q, want %q", status.PhaseDetail, phaseDetailAwaitingTerminatingPods)
	}
	cond := compatibleCondition(status)
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != ReasonAwaitingTerminatingPods {
		t.Fatalf("Compatible = %+v, want Unknown/%s", cond, ReasonAwaitingTerminatingPods)
	}
	started := cond.LastTransitionTime

	// A later pass still waits, and keeps the wait's start.
	result = r.handleValidating(context.Background(), mig, status)
	if result.FailureMsg != "" || result.Advanced {
		t.Fatalf("second pass result = %+v, want a requeue", result)
	}
	if got := compatibleCondition(status).LastTransitionTime; !got.Equal(&started) {
		t.Errorf("wait start moved from %s to %s", started, got)
	}

	releaseTerminatingPod(t, c, "guest-mig-prev")
	// Once the node fits, the wait's Unknown condition goes, so a later
	// Validating check that fails does not leave it behind.
	var node corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "worker-2"}, &node); err != nil {
		t.Fatal(err)
	}
	var class swiftv1alpha1.SwiftGuestClass
	if err := c.Get(context.Background(), client.ObjectKey{Name: "class-default"}, &class); err != nil {
		t.Fatal(err)
	}
	if res := r.checkNodeCapacity(context.Background(), status, &node, &class, ""); res != nil {
		t.Fatalf("checkNodeCapacity = %+v, want nil once the pod is gone", res)
	}
	if cond := compatibleCondition(status); cond != nil {
		t.Errorf("Compatible = %+v, want the wait's condition removed", cond)
	}
	result = r.handleValidating(context.Background(), mig, status)
	if !result.Advanced {
		t.Fatalf("result = %+v, want Advanced once the terminating pod is gone", result)
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhasePreparing {
		t.Errorf("phase = %q, want Preparing", status.Phase)
	}
	if cond := compatibleCondition(status); cond.Status != metav1.ConditionTrue {
		t.Errorf("Compatible = %s, want True", cond.Status)
	}
}

// Past terminatingPodsWait, the wait ends and the migration fails with the
// message it would have failed with at once.
func TestValidating_TerminatingPodWaitExpired_Fails(t *testing.T) {
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
	mig.Status.Conditions = []metav1.Condition{{
		Type:               migrationv1alpha1.SwiftMigrationConditionCompatible,
		Status:             metav1.ConditionUnknown,
		Reason:             ReasonAwaitingTerminatingPods,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-terminatingPodsWait - time.Second)),
	}}
	r, _ := validatingT4(t, mig)

	status := mig.Status.DeepCopy()
	result := r.handleValidating(context.Background(), mig, status)
	if result.FailureMsg != t4Message {
		t.Fatalf("FailureMsg = %q, want %q", result.FailureMsg, t4Message)
	}
	if cond := compatibleCondition(status); cond.Status != metav1.ConditionFalse || cond.Reason != ReasonValidationFailed {
		t.Errorf("Compatible = %s/%s, want False/%s", cond.Status, cond.Reason, ReasonValidationFailed)
	}
}

// A target that would be short even without its terminating pods fails at
// once, as before, and starts no wait.
func TestValidating_ShortEvenWithoutTerminatingPods_FailsAtOnce(t *testing.T) {
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
	r, _ := validatingT4(t, mig, podOnNode("more", "worker-2", "1", "1Gi"))

	status := mig.Status.DeepCopy()
	result := r.handleValidating(context.Background(), mig, status)
	want := `target node "worker-2" has insufficient CPU headroom: need 2, have -710m (allocatable 8, used 8710m)`
	if result.FailureMsg != want {
		t.Fatalf("FailureMsg = %q, want %q", result.FailureMsg, want)
	}
	if cond := compatibleCondition(status); cond != nil {
		t.Errorf("Compatible = %+v, want no condition (no wait started)", cond)
	}
}

// The live path, which T4 took, waits too.
func TestValidatingLive_TerminatingPodOnTarget_Waits(t *testing.T) {
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * time.Minute}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
	r, _ := validatingT4(t, mig, newSourcePod("guest", "default", "src-uid"))

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if result.Err != nil || result.FailureMsg != "" || result.Advanced {
		t.Fatalf("result = %+v, want a requeue in Validating", result)
	}
	if status.PhaseDetail != phaseDetailAwaitingTerminatingPods {
		t.Errorf("phaseDetail = %q, want %q", status.PhaseDetail, phaseDetailAwaitingTerminatingPods)
	}
}

// Through Reconcile the wait is persisted: the migration stays Validating with
// the phaseDetail and requeues.
func TestReconcile_TerminatingPodOnTarget_StaysValidating(t *testing.T) {
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
	r, c := validatingT4(t, mig)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != terminatingPodsPollInterval {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, terminatingPodsPollInterval)
	}
	var got migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(mig), &got); err != nil {
		t.Fatalf("get migration: %v", err)
	}
	if got.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseValidating {
		t.Errorf("phase = %q, want Validating", got.Status.Phase)
	}
	if got.Status.PhaseDetail != phaseDetailAwaitingTerminatingPods {
		t.Errorf("phaseDetail = %q, want %q", got.Status.PhaseDetail, phaseDetailAwaitingTerminatingPods)
	}
	if cond := compatibleCondition(&got.Status); cond == nil || cond.Status != metav1.ConditionUnknown {
		t.Errorf("persisted Compatible = %+v, want Unknown", cond)
	}
}

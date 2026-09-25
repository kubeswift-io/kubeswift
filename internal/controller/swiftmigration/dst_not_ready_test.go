package swiftmigration

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

// withEventIndex registers the field index the fake client needs to serve the
// involvedObject.name selector podWarningEvents lists Events with.
func withEventIndex(b *fake.ClientBuilder) *fake.ClientBuilder {
	return b.WithIndex(&corev1.Event{}, eventInvolvedObjectNameField, func(o client.Object) []string {
		return []string{o.(*corev1.Event).InvolvedObject.Name}
	})
}

// podEvent builds an Event about pod, last seen at when.
func podEvent(name string, pod *corev1.Pod, eventType, reason, message string, when time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pod.Namespace},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
		},
		Type:          eventType,
		Reason:        reason,
		Message:       message,
		LastTimestamp: metav1.NewTime(when),
	}
}

func waiting(name, reason, message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message},
	}}
}

func TestPodStatusNotReadyCause(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	cases := []struct {
		name string
		pod  corev1.Pod
		want []string
	}{
		{
			name: "ready pod says nothing",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "launcher", Ready: true, State: running}},
			}},
		},
		{
			name: "pod with no status yet says nothing",
			pod:  corev1.Pod{},
		},
		{
			name: "container config error",
			pod: corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				waiting("launcher", "CreateContainerConfigError", `secret "guest-seed" not found`),
			}}},
			want: []string{`container "launcher" waiting: CreateContainerConfigError: secret "guest-seed" not found`},
		},
		{
			name: "image pull back-off",
			pod: corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				waiting("launcher", "ImagePullBackOff", `Back-off pulling image "ghcr.io/x/launcher:v9"`),
			}}},
			want: []string{`container "launcher" waiting: ImagePullBackOff: Back-off pulling image "ghcr.io/x/launcher:v9"`},
		},
		{
			name: "failed init container holds up the containers",
			pod: corev1.Pod{Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{{Name: "network-init", State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
				}}},
				ContainerStatuses: []corev1.ContainerStatus{waiting("launcher", "PodInitializing", "")},
			}},
			want: []string{`init container "network-init" exited 1: Error`},
		},
		{
			name: "running init container holds up the containers",
			pod: corev1.Pod{Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{
					{Name: "seed", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
					{Name: "network-init", State: running},
				},
				ContainerStatuses: []corev1.ContainerStatus{waiting("launcher", "PodInitializing", "")},
			}},
			want: []string{`init container "network-init" still running`},
		},
		{
			name: "running native sidecar does not",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "proxy", RestartPolicy: &always}}},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{{Name: "proxy", State: running}},
					ContainerStatuses:     []corev1.ContainerStatus{waiting("launcher", "ContainerCreating", "")},
				},
			},
			want: []string{`container "launcher" waiting: ContainerCreating`},
		},
		{
			name: "kubelet rejection",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodFailed, Reason: "OutOfcpu",
				Message: "Pod was rejected: Node didn't have enough resource: cpu",
			}},
			want: []string{"pod Failed: OutOfcpu: Pod was rejected: Node didn't have enough resource: cpu"},
		},
		{
			name: "unschedulable",
			pod: corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
				Message: "0/3 nodes are available: 3 Insufficient devices.kubevirt.io/kvm.",
			}}}},
			want: []string{"not scheduled: Unschedulable: 0/3 nodes are available: 3 Insufficient devices.kubevirt.io/kvm."},
		},
		{
			name: "running but not ready, and a container that exited",
			pod: corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "launcher", State: running},
				{Name: "stunnel", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 2, Reason: "Error", Message: "cannot load\ncertificate",
				}}},
			}}},
			want: []string{
				`container "launcher" running but not ready`,
				`container "stunnel" exited 2: Error: cannot load certificate`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := podStatusNotReadyCause(&tc.pod); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Events are quoted latest first, one per reason, at most two, each message
// bounded; equal timestamps are broken by name so the same events always give
// the same message. Normal events, other pods' events and the events of an
// earlier pod with the same name are left out.
func TestPodWarningEvents_LatestPerReasonBoundedAndDeterministic(t *testing.T) {
	scheme := testScheme(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "guest-mig-abcdef", Namespace: "default", UID: "dst-uid"}}
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "src-uid"}}
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	long := "AttachVolume.Attach failed for volume \"pvc-1\" : " + strings.Repeat("x", 400)

	// Repeating: lastTimestamp is old, series.lastObservedTime is the latest.
	mount := podEvent("e-mount", pod, corev1.EventTypeWarning, "FailedMount", "timed out waiting for the condition", t0)
	mount.Series = &corev1.EventSeries{Count: 3, LastObservedTime: metav1.NewMicroTime(t0.Add(30 * time.Second))}
	// Two of one reason at the same instant: "e-attach-a" wins on name.
	attachA := podEvent("e-attach-a", pod, corev1.EventTypeWarning, "FailedAttachVolume", long, t0.Add(20*time.Second))
	attachB := podEvent("e-attach-b", pod, corev1.EventTypeWarning, "FailedAttachVolume", "second", t0.Add(20*time.Second))
	// events.k8s.io recorder: eventTime only. A third reason, past the bound.
	sched := podEvent("e-sched", pod, corev1.EventTypeWarning, "FailedScheduling", "0/3 nodes", time.Time{})
	sched.EventTime = metav1.NewMicroTime(t0.Add(10 * time.Second))
	normal := podEvent("e-normal", pod, corev1.EventTypeNormal, "Scheduled", "assigned", t0.Add(time.Minute))
	stale := podEvent("e-stale", pod, corev1.EventTypeWarning, "FailedKillPod", "earlier pod", t0.Add(time.Minute))
	stale.InvolvedObject.UID = "earlier-uid"
	elsewhere := podEvent("e-other", other, corev1.EventTypeWarning, "Unhealthy", "probe failed", t0.Add(time.Minute))

	c := withEventIndex(fake.NewClientBuilder().WithScheme(scheme)).
		WithObjects(attachB, sched, mount, normal, stale, elsewhere, attachA).
		Build()
	r := &SwiftMigrationReconciler{Client: c, APIReader: c, Scheme: scheme}

	want := []string{
		"Warning FailedMount: timed out waiting for the condition",
		"Warning FailedAttachVolume: " + long[:notReadyDetailMaxLen] + "...",
	}
	for i := 0; i < 3; i++ {
		if got := r.podWarningEvents(context.Background(), pod); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %q\nwant %q", got, want)
		}
	}
}

// Without an APIReader, or when the list fails (a role without events list),
// there are no events and the pod's status is reported alone.
func TestPodWarningEvents_NoReaderOrListErrorGivesNone(t *testing.T) {
	scheme := testScheme(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	ev := podEvent("e", pod, corev1.EventTypeWarning, "FailedMount", "m", time.Now())

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ev).Build()
	if got := (&SwiftMigrationReconciler{Client: c}).podWarningEvents(context.Background(), pod); got != nil {
		t.Errorf("no APIReader: got %q, want none", got)
	}
	// No field index registered: the fake client fails the list, as a
	// Forbidden would.
	if got := (&SwiftMigrationReconciler{Client: c, APIReader: c}).podWarningEvents(context.Background(), pod); got != nil {
		t.Errorf("list error: got %q, want none", got)
	}
}

// An offline migration whose destination pod never became Ready ran into
// spec.timeout with only "did not complete in time". The Timeout failure now
// says why the pod is not Ready, leaving out the events of the source pod,
// which had the same name.
func TestOffline_TimeoutNamesWhyDestinationPodIsNotReady(t *testing.T) {
	scheme := testScheme(t)
	guest := guestRunning("guest", "default", "10.244.1.5")
	guest.Spec.NodeName = "worker-2"
	mig := newMigration("m", "default")
	mig.Finalizers = []string{FinalizerName}
	mig.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	mig.Status.StartedAt = &started
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming
	mig.Status.Mode = migrationv1alpha1.SwiftMigrationModeOffline
	mig.Status.SourceNode = "worker-1"
	mig.Status.DestinationNode = "worker-2"

	dst := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "dst-uid"},
		Spec:       corev1.PodSpec{NodeName: "worker-2", Containers: []corev1.Container{{Name: "launcher"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{waiting("launcher", "ContainerCreating", "")},
		},
	}
	attach := `AttachVolume.Attach failed for volume "pvc-1" : volume pvc-1 failed to attach to node worker-2`
	attachEv := podEvent("guest.attach", dst, corev1.EventTypeWarning, "FailedAttachVolume", attach, time.Now())
	srcPod := dst.DeepCopy()
	srcPod.UID = "src-uid"
	srcEv := podEvent("guest.src", srcPod, corev1.EventTypeWarning, "FailedKillPod", "source pod event", time.Now())

	c := withEventIndex(fake.NewClientBuilder().WithScheme(scheme)).
		WithObjects(mig, guest, dst, attachEv, srcEv).
		WithStatusSubresource(mig).
		Build()
	rec := record.NewFakeRecorder(20)
	r := &SwiftMigrationReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: rec}

	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	var got migrationv1alpha1.SwiftMigration
	if err := c.Get(ctx, client.ObjectKey{Name: "m", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseFailed || got.Status.FailureReason != migrationv1alpha1.FailureReasonTimeout {
		t.Fatalf("phase=%s reason=%s, want Failed/Timeout", got.Status.Phase, got.Status.FailureReason)
	}
	detail := `destination pod "guest" is not Ready: container "launcher" waiting: ContainerCreating; Warning FailedAttachVolume: ` + attach
	want := "spec.timeout=30m0s exceeded since StartedAt; migration did not complete in time; " + detail
	if got.Status.FailureMessage != want {
		t.Errorf("failureMessage:\n got %q\nwant %q", got.Status.FailureMessage, want)
	}
	if !recordedEvent(rec, "Warning "+eventReasonDestinationPodNotReady+" "+detail) {
		t.Errorf("no %s Warning event carrying the detail", eventReasonDestinationPodNotReady)
	}
}

// recordedEvent drains rec and reports whether it recorded want.
func recordedEvent(rec *record.FakeRecorder, want string) bool {
	found := false
	for {
		select {
		case e := <-rec.Events:
			if e == want {
				found = true
			}
		default:
			return found
		}
	}
}

package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func TestPoolExecArgs(t *testing.T) {
	// command + args + cwd + the pool image env merged with spec.env (spec overrides
	// by key), no registry pull.
	sb := &sandboxv1alpha1.SwiftSandbox{Spec: sandboxv1alpha1.SwiftSandboxSpec{
		Command:    []string{"sh", "-c"},
		Args:       []string{"echo hi"},
		Env:        []corev1.EnvVar{{Name: "A", Value: "1"}},
		WorkingDir: "/tmp",
	}}
	argv, env, cwd := poolExecArgs(sb, []string{"PATH=/usr/bin", "A=image"})
	if strings.Join(argv, " ") != "sh -c echo hi" {
		t.Errorf("argv = %v", argv)
	}
	// image PATH kept; A overridden by spec.env; image order preserved.
	if strings.Join(env, ",") != "PATH=/usr/bin,A=1" || cwd != "/tmp" {
		t.Errorf("env=%v cwd=%q", env, cwd)
	}
	// No command -> nil argv (caller cold-falls-back to the image entrypoint).
	if a, _, _ := poolExecArgs(&sandboxv1alpha1.SwiftSandbox{}, nil); a != nil {
		t.Errorf("no-command argv should be nil, got %v", a)
	}
}

func TestNonControllerRefs(t *testing.T) {
	refs := []metav1.OwnerReference{
		{Name: "keep-nonctrl"},
		{Name: "pool", Controller: ptr.To(true)},
		{Name: "keep-explicit-false", Controller: ptr.To(false)},
	}
	out := nonControllerRefs(refs)
	if len(out) != 2 {
		t.Fatalf("expected 2 non-controller refs, got %d: %v", len(out), out)
	}
	for _, r := range out {
		if r.Controller != nil && *r.Controller {
			t.Errorf("controller ref survived: %v", r)
		}
	}
}

func checkoutTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	gv := schema.GroupVersion{Group: "sandbox.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gv,
		&sandboxv1alpha1.SwiftSandbox{}, &sandboxv1alpha1.SwiftSandboxList{},
		&sandboxv1alpha1.SwiftSandboxPool{}, &sandboxv1alpha1.SwiftSandboxPoolList{})
	metav1.AddToGroupVersion(s, gv)
	return s
}

// TestReconcileClaimedSlot_ExitMapsToTerminal is the key checkout behavior: a claimed
// slot's exec-status (mirroring the sandbox UID) with a non-zero exit code drives the
// sandbox to Failed, records the exit code, and destroys the consumed slot pod.
func TestReconcileClaimedSlot_ExitMapsToTerminal(t *testing.T) {
	s := checkoutTestScheme()
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "uid-1"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "alpine", PoolRef: &corev1.LocalObjectReference{Name: "pool"}},
		Status:     sandboxv1alpha1.SwiftSandboxStatus{PodRef: "pool-slot-abc"},
	}
	slot := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pool-slot-abc", Namespace: "ns",
		Annotations: map[string]string{
			annSandboxExecStatusID:     "uid-1",
			annSandboxExecStatus:       "complete",
			annSandboxExecStatusDetail: "7",
		},
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sb, slot).
		WithStatusSubresource(sb).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	if _, err := r.reconcileClaimedSlot(context.Background(), sb); err != nil {
		t.Fatalf("reconcileClaimedSlot: %v", err)
	}

	var got sandboxv1alpha1.SwiftSandbox
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &got)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.ExitCode == nil || *got.Status.ExitCode != 7 {
		t.Errorf("exitCode = %v, want 7", got.Status.ExitCode)
	}
	// the consumed slot pod is destroyed (the pool replenishes a fresh warm one).
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "pool-slot-abc"}, &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("slot pod should be deleted, got err=%v", err)
	}
}

// A status-id that does NOT mirror the sandbox UID is stale — the sandbox stays Running
// and the slot is not consumed.
func TestReconcileClaimedSlot_IgnoresStaleStatus(t *testing.T) {
	s := checkoutTestScheme()
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "uid-1"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{PoolRef: &corev1.LocalObjectReference{Name: "pool"}},
		Status:     sandboxv1alpha1.SwiftSandboxStatus{PodRef: "pool-slot-abc"},
	}
	slot := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pool-slot-abc", Namespace: "ns",
		Annotations: map[string]string{
			annSandboxExecStatusID: "someone-else", annSandboxExecStatus: "complete", annSandboxExecStatusDetail: "0",
		},
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sb, slot).WithStatusSubresource(sb).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.reconcileClaimedSlot(context.Background(), sb); err != nil {
		t.Fatalf("reconcileClaimedSlot: %v", err)
	}
	var got sandboxv1alpha1.SwiftSandbox
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &got)
	if got.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed || got.Status.Phase == sandboxv1alpha1.SwiftSandboxCompleted {
		t.Errorf("stale status must not terminalize; phase=%q", got.Status.Phase)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "pool-slot-abc"}, &corev1.Pod{}); err != nil {
		t.Errorf("slot pod should survive a stale status: %v", err)
	}
}

// endedSlot is claimed slot pod pool-slot-abc of sandbox sb in the given
// phase, carrying ann. When the pod has ended, its launcher container is
// terminated with exitCode and msg.
func endedSlot(phase corev1.PodPhase, exitCode int32, msg string, ann map[string]string) *corev1.Pod {
	slot := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-slot-abc", Namespace: "ns", Annotations: ann},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
		slot.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: launcherName, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: msg},
		}}}
	}
	return slot
}

// reconcileCheckout runs reconcileClaimedSlot once for sandbox sb (UID uid-1,
// Running, checked out onto pool-slot-abc) with slot as its pod (nil: the pod
// is gone), and returns the client to inspect.
func reconcileCheckout(t *testing.T, slot *corev1.Pod) client.Client {
	t.Helper()
	s := checkoutTestScheme()
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "uid-1"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "alpine", PoolRef: &corev1.LocalObjectReference{Name: "pool"}},
		Status:     sandboxv1alpha1.SwiftSandboxStatus{Phase: sandboxv1alpha1.SwiftSandboxRunning, PodRef: "pool-slot-abc"},
	}
	objs := []client.Object{sb}
	if slot != nil {
		objs = append(objs, slot)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(sb).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.reconcileClaimedSlot(context.Background(), sb); err != nil {
		t.Fatalf("reconcileClaimedSlot: %v", err)
	}
	return c
}

func checkoutSandbox(t *testing.T, c client.Client) *sandboxv1alpha1.SwiftSandbox {
	t.Helper()
	var got sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

// A checked-out sandbox learned its outcome only from swiftletd's exec status.
// When the slot pod ended first, or went away, that status never came, and the
// sandbox stayed Running until spec.timeout, or for good without one. It now
// fails, naming the pod and how it ended.
func TestReconcileClaimedSlot_SlotEndedBeforeTheWorkloadReported(t *testing.T) {
	for _, tc := range []struct {
		name       string
		slot       *corev1.Pod // nil: the pod is gone
		wantReason string
		wantMsg    []string
	}{
		{"pod failed, no exec status", endedSlot(corev1.PodFailed, 1, "guest kernel panic", nil), "SlotEnded",
			[]string{"pool-slot-abc", "(Failed, launcher exit 1)", "guest kernel panic"}},
		{"pod succeeded, no exec status", endedSlot(corev1.PodSucceeded, 0, "", nil), "SlotEnded",
			[]string{"pool-slot-abc", "(Succeeded, launcher exit 0)", "launcher exited"}},
		{"pod failed, exec status not final", endedSlot(corev1.PodFailed, 1, "",
			map[string]string{annSandboxExecStatusID: "uid-1", annSandboxExecStatus: "running"}), "SlotEnded",
			[]string{"pool-slot-abc", "(Failed, launcher exit 1)"}},
		{"pod gone", nil, "SlotLost", []string{"pool-slot-abc", "is gone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := reconcileCheckout(t, tc.slot)
			got := checkoutSandbox(t, c)
			if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed {
				t.Fatalf("phase = %q, want Failed", got.Status.Phase)
			}
			if cond := findCond(got, sandboxv1alpha1.SwiftSandboxConditionGuestRunning); cond == nil || cond.Reason != tc.wantReason {
				t.Errorf("GuestRunning = %+v, want reason %s", cond, tc.wantReason)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(got.Status.Message, want) {
					t.Errorf("message %q should contain %q", got.Status.Message, want)
				}
			}
			if got.Status.ExitCode != nil {
				t.Errorf("the workload's exit code is unknown, got exitCode=%d", *got.Status.ExitCode)
			}
			if got.Status.TerminalAt == nil {
				t.Error("terminalAt should be set")
			}
			if tc.slot != nil {
				// Kept for its logs, as a cold sandbox's launcher is.
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(tc.slot), &corev1.Pod{}); err != nil {
					t.Errorf("the ended slot pod should be kept: %v", err)
				}
			}
		})
	}
}

// A final exec status decides the outcome even when the slot pod ended after
// swiftletd wrote it: a completed checkout must not turn into a failed one.
func TestReconcileClaimedSlot_FinalStatusWinsOverAnEndedPod(t *testing.T) {
	for _, tc := range []struct {
		name      string
		phase     corev1.PodPhase
		status    string
		detail    string
		wantPhase sandboxv1alpha1.SwiftSandboxPhase
		wantCode  *int32
	}{
		{"complete 0, pod succeeded", corev1.PodSucceeded, "complete", "0", sandboxv1alpha1.SwiftSandboxCompleted, ptr.To[int32](0)},
		{"complete 0, pod failed", corev1.PodFailed, "complete", "0", sandboxv1alpha1.SwiftSandboxCompleted, ptr.To[int32](0)},
		{"complete 2, pod succeeded", corev1.PodSucceeded, "complete", "2", sandboxv1alpha1.SwiftSandboxFailed, ptr.To[int32](2)},
		{"exec failed, pod failed", corev1.PodFailed, "failed", "no such file", sandboxv1alpha1.SwiftSandboxFailed, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := reconcileCheckout(t, endedSlot(tc.phase, 1, "", map[string]string{
				annSandboxExecStatusID: "uid-1", annSandboxExecStatus: tc.status, annSandboxExecStatusDetail: tc.detail,
			}))
			got := checkoutSandbox(t, c)
			if got.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q (%s)", got.Status.Phase, tc.wantPhase, got.Status.Message)
			}
			if (got.Status.ExitCode == nil) != (tc.wantCode == nil) ||
				(tc.wantCode != nil && *got.Status.ExitCode != *tc.wantCode) {
				t.Errorf("exitCode = %v, want %v", got.Status.ExitCode, tc.wantCode)
			}
			if cond := findCond(got, sandboxv1alpha1.SwiftSandboxConditionGuestRunning); cond != nil && cond.Reason == "SlotEnded" {
				t.Errorf("a final exec status was overridden by the pod's end: %+v", cond)
			}
		})
	}
}

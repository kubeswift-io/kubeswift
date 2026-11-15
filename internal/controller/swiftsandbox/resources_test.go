package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func launcherOf(t *testing.T, pod *corev1.Pod) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == launcherName {
			return c
		}
	}
	t.Fatal("no launcher container")
	return corev1.Container{}
}

// The launcher requests the guest's memory plus overhead and a CPU per vCPU,
// and sets no limits.
func TestBuildPod_LauncherRequests(t *testing.T) {
	sb := egSandbox("")
	sb.Spec.CPU = 2
	sb.Spec.Memory = resource.MustParse("1Gi")
	c := launcherOf(t, buildPod(sb, "sandbox"))
	if got, want := c.Resources.Requests[corev1.ResourceMemory], resource.MustParse("1088Mi"); got.Cmp(want) != 0 {
		t.Errorf("memory request = %s, want %s", got.String(), want.String())
	}
	if got := c.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("cpu request = %s, want 2", got.String())
	}
	if len(c.Resources.Limits) != 0 {
		t.Errorf("limits = %v, want none", c.Resources.Limits)
	}

	// Unset size: the launcher's floors (1 vCPU, 128 MiB).
	c = launcherOf(t, buildPod(egSandbox(""), "sandbox"))
	if got := c.Resources.Requests[corev1.ResourceMemory]; got.Cmp(resource.MustParse("192Mi")) != 0 {
		t.Errorf("floor memory request = %s, want 192Mi", got.String())
	}
	if got := c.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("floor cpu request = %s, want 1", got.String())
	}
}

func TestBuildPod_LauncherCPUPerVCPU(t *testing.T) {
	old := LauncherCPUPerVCPU
	t.Cleanup(func() { LauncherCPUPerVCPU = old })
	sb := egSandbox("")
	sb.Spec.CPU = 4

	LauncherCPUPerVCPU = resource.MustParse("250m")
	if got := launcherOf(t, buildPod(sb, "sandbox")).Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("250m x 4 = %s, want 1", got.String())
	}
	LauncherCPUPerVCPU = resource.MustParse("0")
	if _, ok := launcherOf(t, buildPod(sb, "sandbox")).Resources.Requests[corev1.ResourceCPU]; ok {
		t.Error("zero per vCPU still requests CPU")
	}
}

func TestParseLauncherCPUPerVCPU(t *testing.T) {
	for in, want := range map[string]string{"": "1", "1": "1", "250m": "250m", "0": "0", "1.5": "1500m"} {
		q, err := ParseLauncherCPUPerVCPU(in)
		if err != nil || q.Cmp(resource.MustParse(want)) != 0 {
			t.Errorf("%q -> %s, %v; want %s", in, q.String(), err, want)
		}
	}
	for _, bad := range []string{"-1", "lots", "1Gi?"} {
		if _, err := ParseLauncherCPUPerVCPU(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A launcher pod no node can take holds the sandbox Pending with the
// scheduler's message and one Warning Event, then Materializing once placed.
func TestReconcilePodState_Unschedulable(t *testing.T) {
	s := checkoutTestScheme()
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "uid-1"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "alpine"},
		Status:     sandboxv1alpha1.SwiftSandboxStatus{Phase: sandboxv1alpha1.SwiftSandboxMaterializing},
	}
	sched := "0/3 nodes are available: 3 Insufficient memory."
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: sched,
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sb, pod).WithStatusSubresource(sb).Build()
	rec := record.NewFakeRecorder(10)
	r := &SwiftSandboxReconciler{Client: c, Scheme: s, Recorder: rec}
	get := func() *sandboxv1alpha1.SwiftSandbox {
		got := &sandboxv1alpha1.SwiftSandbox{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	for range 2 {
		if _, err := r.reconcilePodState(context.Background(), get(), pod); err != nil {
			t.Fatal(err)
		}
	}
	got := get()
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPending || got.Status.Message != "waiting for a node: "+sched {
		t.Errorf("phase %q message %q", got.Status.Phase, got.Status.Message)
	}
	if n := len(rec.Events); n != 1 {
		t.Errorf("%d events, want 1", n)
	}

	pod.Spec.NodeName = "n1"
	pod.Status.Conditions = nil
	if _, err := r.reconcilePodState(context.Background(), get(), pod); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.Status.Phase != sandboxv1alpha1.SwiftSandboxMaterializing {
		t.Errorf("placed: phase %q", got.Status.Phase)
	}
}

// A pool whose warm slots no node can take is Degraded with the scheduler's
// reason, once per transition; Warming otherwise.
func TestPoolStatus_UnschedulableSlots(t *testing.T) {
	s := checkoutTestScheme()
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "alpine", MinWarm: 2},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pool).WithStatusSubresource(pool).Build()
	rec := record.NewFakeRecorder(10)
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: s, Recorder: rec}
	get := func() *sandboxv1alpha1.SwiftSandboxPool {
		got := &sandboxv1alpha1.SwiftSandboxPool{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "p"}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	sched := "0/3 nodes are available: 3 Insufficient cpu."
	for range 2 {
		if _, err := r.updateStatus(context.Background(), get(), 1, 0, nil, 1, sched); err != nil {
			t.Fatal(err)
		}
	}
	got := get()
	w := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxPoolConditionWarm)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPoolDegraded || w == nil || w.Reason != reasonUnschedulable ||
		!strings.Contains(got.Status.Message, "1 waiting for a node: "+sched) {
		t.Errorf("phase %q message %q warm %+v", got.Status.Phase, got.Status.Message, w)
	}
	if n := len(rec.Events); n != 1 {
		t.Errorf("%d events, want 1", n)
	}
	if _, err := r.updateStatus(context.Background(), get(), 1, 0, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.Status.Phase != sandboxv1alpha1.SwiftSandboxPoolWarming {
		t.Errorf("schedulable again: phase %q", got.Status.Phase)
	}
}

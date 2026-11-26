package podmode

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsandbox"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

var standInKey = types.NamespacedName{Namespace: "tenant", Name: "sb-1"}

func getStandIn(t *testing.T, c client.Client) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), standInKey, &pod); err != nil {
		t.Fatal(err)
	}
	return &pod
}

func getSandbox(t *testing.T, c client.Client) *sandboxv1alpha1.SwiftSandbox {
	t.Helper()
	var sb sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: pmNS, Name: sbKey}, &sb); err != nil {
		t.Fatal(err)
	}
	return &sb
}

func readyCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == podmode.ConditionMicroVMReady {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func setPhase(t *testing.T, c client.Client, phase sandboxv1alpha1.SwiftSandboxPhase, code *int32) {
	t.Helper()
	sb := getSandbox(t, c)
	sb.Status.Phase, sb.Status.ExitCode = phase, code
	if err := c.Status().Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
}

func setShim(t *testing.T, c client.Client, restarts int32, running bool) {
	t.Helper()
	pod := getStandIn(t, c)
	cs := corev1.ContainerStatus{Name: pod.Spec.Containers[0].Name, RestartCount: restarts}
	if running {
		cs.State.Running = &corev1.ContainerStateRunning{}
	} else {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

// linkedRunning links a stand-in with the given restart policy and brings its
// microVM to Running with a launcher.
func linkedRunning(t *testing.T, policy corev1.RestartPolicy) (*Reconciler, client.Client) {
	t.Helper()
	p := standIn(t)
	p.Spec.RestartPolicy = policy
	launcher := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: sbKey, Namespace: pmNS, Labels: map[string]string{swiftsandbox.SandboxLabelKey: sbKey}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.2.3"},
	}
	r, c, _ := setup(t, p, launcher)
	reconcileOnce(t, r, "tenant", "sb-1")
	setPhase(t, c, sandboxv1alpha1.SwiftSandboxRunning, nil)
	setShim(t, c, 0, true)
	reconcileOnce(t, r, "tenant", "sb-1")
	return r, c
}

func TestReadinessGate(t *testing.T) {
	p := standIn(t)
	if len(p.Spec.ReadinessGates) != 1 || p.Spec.ReadinessGates[0].ConditionType != podmode.ConditionMicroVMReady {
		t.Fatalf("readiness gates %v", p.Spec.ReadinessGates)
	}
	r, c, _ := setup(t, p)
	reconcileOnce(t, r, "tenant", "sb-1")
	if cond := readyCondition(getStandIn(t, c)); cond == nil || cond.Status != corev1.ConditionFalse || cond.Reason != "MicroVMStarting" {
		t.Errorf("before the microVM runs: %+v", cond)
	}

	r, c = linkedRunning(t, corev1.RestartPolicyAlways)
	if cond := readyCondition(getStandIn(t, c)); cond == nil || cond.Status != corev1.ConditionTrue {
		t.Errorf("running: %+v", cond)
	}
	_ = r
}

func TestExitWithoutRestart(t *testing.T) {
	for name, tc := range map[string]struct {
		policy corev1.RestartPolicy
		phase  sandboxv1alpha1.SwiftSandboxPhase
		code   *int32
		want   string
	}{
		"Never, completed":      {corev1.RestartPolicyNever, sandboxv1alpha1.SwiftSandboxCompleted, ptr.To[int32](0), "1:0"},
		"Never, failed":         {corev1.RestartPolicyNever, sandboxv1alpha1.SwiftSandboxFailed, ptr.To[int32](3), "1:3"},
		"OnFailure, completed":  {corev1.RestartPolicyOnFailure, sandboxv1alpha1.SwiftSandboxCompleted, nil, "1:0"},
		"failed without a code": {corev1.RestartPolicyNever, sandboxv1alpha1.SwiftSandboxFailed, nil, "1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			r, c := linkedRunning(t, tc.policy)
			setPhase(t, c, tc.phase, tc.code)
			reconcileOnce(t, r, "tenant", "sb-1")
			pod := getStandIn(t, c)
			if pod.Annotations[podmode.AnnotationExit] != tc.want || pod.Annotations[podmode.AnnotationExitRestarts] != "0" {
				t.Errorf("annotations %v", pod.Annotations)
			}
			if cond := readyCondition(pod); cond == nil || cond.Status != corev1.ConditionFalse || cond.Reason != "MicroVMExited" {
				t.Errorf("condition %+v", cond)
			}
			// Even if the shim restarted, these policies do not restart
			// the workload.
			setShim(t, c, 1, true)
			reconcileOnce(t, r, "tenant", "sb-1")
			if RunOf(getStandIn(t, c)) != 1 || getSandbox(t, c).DeletionTimestamp != nil {
				t.Error("restarted")
			}
		})
	}
}

func TestExitThenRestart(t *testing.T) {
	r, c := linkedRunning(t, corev1.RestartPolicyAlways)
	setPhase(t, c, sandboxv1alpha1.SwiftSandboxFailed, ptr.To[int32](2))
	reconcileOnce(t, r, "tenant", "sb-1")
	if got := getStandIn(t, c).Annotations[podmode.AnnotationExit]; got != "1:2" {
		t.Fatalf("exit %q", got)
	}

	// Backing off: the shim is not running again yet.
	setShim(t, c, 1, false)
	reconcileOnce(t, r, "tenant", "sb-1")
	if RunOf(getStandIn(t, c)) != 1 {
		t.Fatal("restarted before the shim ran again")
	}

	// The kubelet restarted the shim: run 2 in a new microVM.
	setShim(t, c, 1, true)
	reconcileOnce(t, r, "tenant", "sb-1")
	if RunOf(getStandIn(t, c)) != 2 {
		t.Fatalf("run %d", RunOf(getStandIn(t, c)))
	}
	var list sandboxv1alpha1.SwiftSandboxList
	_ = c.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("old microVM kept: %d", len(list.Items))
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	sb := getSandbox(t, c)
	if sb.Annotations[podmode.AnnotationRun] != "2" || sb.Status.Phase != "" {
		t.Errorf("new microVM %v %q", sb.Annotations, sb.Status.Phase)
	}
	// The exit of run 1 stays until run 2 ends; the restarted shim ignores it.
	if got := getStandIn(t, c).Annotations[podmode.AnnotationExit]; got != "1:2" {
		t.Errorf("exit %q", got)
	}
	setPhase(t, c, sandboxv1alpha1.SwiftSandboxCompleted, ptr.To[int32](0))
	reconcileOnce(t, r, "tenant", "sb-1")
	if got := getStandIn(t, c).Annotations[podmode.AnnotationExit]; got != "2:0" {
		t.Errorf("exit of run 2 %q", got)
	}
}

func TestSandboxOfAnotherRunIsReplaced(t *testing.T) {
	r, c := linkedRunning(t, corev1.RestartPolicyAlways)
	pod := getStandIn(t, c)
	pod.Annotations[podmode.AnnotationRun] = "2"
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	var list sandboxv1alpha1.SwiftSandboxList
	_ = c.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("microVM of run 1 kept")
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	if getSandbox(t, c).Annotations[podmode.AnnotationRun] != "2" {
		t.Error("no microVM for run 2")
	}
}

func TestEndedRunIsNotBootedAgain(t *testing.T) {
	r, c := linkedRunning(t, corev1.RestartPolicyNever)
	setPhase(t, c, sandboxv1alpha1.SwiftSandboxCompleted, ptr.To[int32](0))
	reconcileOnce(t, r, "tenant", "sb-1")
	if err := c.Delete(context.Background(), getSandbox(t, c)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	var list sandboxv1alpha1.SwiftSandboxList
	_ = c.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Error("a run that ended was booted again")
	}
}

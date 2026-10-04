package swiftsandbox

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

func probedSandbox() *sandboxv1alpha1.SwiftSandbox {
	sb := plainSandbox("busybox:1")
	sb.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{{Name: "http-app", Port: 3000}}
	sb.Spec.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("http-app"), HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Probe", Value: "1"}}}}}
	sb.Spec.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(3000)}}, PeriodSeconds: 5, FailureThreshold: 2}
	return sb
}

// The intent swiftletd reads has the port resolved and every default applied.
func TestProbesIntent_ResolvesPortsAndAppliesDefaults(t *testing.T) {
	got := probesIntent(probedSandbox())
	want := &runtimeintent.SandboxProbesIntent{
		Readiness: &runtimeintent.SandboxProbeIntent{Kind: "http", Port: 3000, Path: "/",
			Headers:       []runtimeintent.ProbeHeader{{Name: "X-Probe", Value: "1"}},
			PeriodSeconds: 10, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 3},
		Liveness: &runtimeintent.SandboxProbeIntent{Kind: "tcp", Port: 3000,
			PeriodSeconds: 5, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 2},
	}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		t.Errorf("probesIntent = %s", gj)
	}
	if probesIntent(plainSandbox("busybox:1")) != nil {
		t.Error("no probes, no intent")
	}
}

// A cold launch carries its probes; a warm slot never does (its checkout
// hands them over). A pod exposing ports gets the readiness gate.
func TestBuild_ProbesAndReadinessGate(t *testing.T) {
	sb := probedSandbox()
	if ri := buildIntent(sb, "sandbox", "/r.ext4", "", execSpec{Argv: []string{"/srv"}}, false); ri.SandboxProbes == nil {
		t.Error("a cold launch must carry its probes")
	}
	if ri := buildIntent(sb, "sandbox", "/r.ext4", "", execSpec{}, true); ri.SandboxProbes != nil {
		t.Error("an idle warm slot must carry no probes")
	}
	pod := buildPod(sb, "sandbox")
	if len(pod.Spec.ReadinessGates) != 1 || pod.Spec.ReadinessGates[0].ConditionType != WorkloadReadyGate {
		t.Errorf("readiness gates = %+v", pod.Spec.ReadinessGates)
	}
	if g := buildPod(plainSandbox("busybox:1"), "sandbox").Spec.ReadinessGates; len(g) != 0 {
		t.Errorf("a pod exposing no ports needs no gate, got %+v", g)
	}
}

// runningLauncher is sb's launcher pod, running, as the kubelet reports it.
func runningLauncher(sb *sandboxv1alpha1.SwiftSandbox, ann map[string]string) *corev1.Pod {
	pod := buildPod(sb, "sandbox")
	pod.Annotations = ann
	pod.Status = corev1.PodStatus{
		Phase:             corev1.PodRunning,
		Conditions:        []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: launcherName, Ready: true}},
	}
	return pod
}

func podGate(t *testing.T, c client.Client, name string) (corev1.ConditionStatus, []corev1.PodCondition) {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &pod); err != nil {
		t.Fatal(err)
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == WorkloadReadyGate {
			return c.Status, pod.Status.Conditions
		}
	}
	return "", pod.Status.Conditions
}

// WorkloadReady and the pod's gate follow swiftletd's readiness verdict, and
// the kubelet's own pod conditions survive the gate write.
func TestReconcile_WorkloadReadyFollowsTheProbe(t *testing.T) {
	ctx := context.Background()
	sb := probedSandbox()
	pod := runningLauncher(sb, nil)
	r, c := sandboxReconciler(sb, pod)

	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxRunning || cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Probing" {
		t.Fatalf("before any verdict: phase %s, WorkloadReady %+v", got.Status.Phase, cond)
	}
	if gate, _ := podGate(t, c, "sb"); gate != corev1.ConditionFalse {
		t.Errorf("gate = %q before any verdict, want False", gate)
	}

	setAnnotations(t, c, "sb", map[string]string{annWorkloadReady: "true", annWorkloadReadyDetail: "http GET 192.168.99.10:3000/: 200"})
	reconcileSB(t, r, "sb")
	cond = apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, ": 200") {
		t.Errorf("after a passing verdict WorkloadReady = %+v", cond)
	}
	gate, conds := podGate(t, c, "sb")
	if gate != corev1.ConditionTrue {
		t.Errorf("gate = %q, want True", gate)
	}
	kept := false
	for _, pc := range conds {
		kept = kept || (pc.Type == corev1.ContainersReady && pc.Status == corev1.ConditionTrue)
	}
	if !kept {
		t.Errorf("the gate write dropped the kubelet's conditions: %+v", conds)
	}

	setAnnotations(t, c, "sb", map[string]string{annWorkloadReady: "false", annWorkloadReadyDetail: "http GET 192.168.99.10:3000/: 503"})
	reconcileSB(t, r, "sb")
	if gate, _ := podGate(t, c, "sb"); gate != corev1.ConditionFalse {
		t.Errorf("gate = %q after a failing verdict, want False", gate)
	}
	_ = ctx
}

// Without a readiness probe the workload counts as ready once the guest runs.
func TestReconcile_NoReadinessProbeIsReadyWhenRunning(t *testing.T) {
	sb := probedSandbox()
	sb.Spec.ReadinessProbe, sb.Spec.LivenessProbe = nil, nil
	r, c := sandboxReconciler(sb, runningLauncher(sb, nil))
	reconcileSB(t, r, "sb")
	cond := apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("WorkloadReady = %+v", cond)
	}
	if gate, _ := podGate(t, c, "sb"); gate != corev1.ConditionTrue {
		t.Errorf("gate = %q, want True", gate)
	}
}

// A failed liveness verdict ends the sandbox and its launcher.
func TestReconcile_LivenessFailureEndsTheSandbox(t *testing.T) {
	sb := probedSandbox()
	r, c := sandboxReconciler(sb, runningLauncher(sb, map[string]string{annLivenessFailed: "tcp connect 192.168.99.10:3000: connection refused"}))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "LivenessProbeFailed" ||
		!strings.Contains(got.Status.Message, "connection refused") {
		t.Errorf("want Failed/LivenessProbeFailed with the probe's message, got %s %+v %q", got.Status.Phase, cond, got.Status.Message)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err == nil {
		t.Error("the launcher must be deleted")
	}
}

// A launcher that ended after its liveness verdict is a liveness failure, not
// a Completed sandbox (the workload never wrote an exit code).
func TestFinishTerminal_LivenessBeforeExitCode(t *testing.T) {
	sb := probedSandbox()
	pod := runningLauncher(sb, map[string]string{annLivenessFailed: "timed out"})
	pod.Status.Phase = corev1.PodSucceeded
	r, c := sandboxReconciler(sb, pod)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || !strings.Contains(got.Status.Message, "liveness probe failed") {
		t.Errorf("got %s: %q", got.Status.Phase, got.Status.Message)
	}
}

// With the webhook off, an exec probe never reaches a launcher.
func TestReconcile_InvalidProbeFailsBeforeLaunch(t *testing.T) {
	sb := plainSandbox(testImage(t))
	sb.Spec.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	if got := getSandbox(t, c, "sb"); got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || !strings.Contains(got.Status.Message, "exec probes") {
		t.Errorf("got %s: %q", got.Status.Phase, got.Status.Message)
	}
}

// A checkout hands its probes to swiftletd with the workload.
func TestCheckout_HandsProbesOverWithTheWorkload(t *testing.T) {
	pool := shapedPool()
	pool.Namespace = "default"
	pool.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{{Name: "http-app", Port: 3000}}
	slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
	slot.Namespace = "default"
	sb := matchingSandbox()
	sb.Namespace = "default"
	sb.Spec.Network.Ports = pool.Spec.Network.Ports
	sb.Spec.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http-app")}}}
	r, c := sandboxReconciler(pool, slot, sb)
	reconcileSB(t, r, "sb")

	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(slot), &p); err != nil {
		t.Fatal(err)
	}
	var args struct {
		Probes *runtimeintent.SandboxProbesIntent `json:"probes"`
	}
	if err := json.Unmarshal([]byte(p.Annotations[annSandboxExecActionArgs]), &args); err != nil {
		t.Fatalf("action args %q: %v", p.Annotations[annSandboxExecActionArgs], err)
	}
	if args.Probes == nil || args.Probes.Readiness == nil || args.Probes.Readiness.Kind != "tcp" || args.Probes.Readiness.Port != 3000 {
		t.Errorf("action args probes = %+v", args.Probes)
	}
}

func setAnnotations(t *testing.T, c client.Client, name string, ann map[string]string) {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	for k, v := range ann {
		pod.Annotations[k] = v
	}
	if err := c.Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
}

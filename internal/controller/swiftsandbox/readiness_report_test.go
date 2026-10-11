package swiftsandbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kubeswift-io/kubeswift/internal/scheme"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

// A pod exposing ports carries the gate and the mark that swiftletd reports
// on it; its intent tells swiftletd which condition to write. A pod without
// ports gets neither.
func TestBuild_LauncherReportsTheGate(t *testing.T) {
	t.Setenv(swiftguest.LauncherStatusGateEnv, "true")
	sb := probedSandbox()
	pod := buildPod(sb, "sandbox")
	if !launcherReportsGate(pod) {
		t.Fatalf("a gated pod must be marked for swiftletd to report: %v", pod.Annotations)
	}
	ri := buildIntent(sb, "sandbox", "/r.ext4", "", execSpec{Argv: []string{"/srv"}}, false)
	if ri.SandboxProbes == nil || ri.SandboxProbes.ReadinessGate != string(WorkloadReadyGate) {
		t.Errorf("cold intent probes = %+v", ri.SandboxProbes)
	}
	plain := plainSandbox("busybox:1")
	if p := buildPod(plain, "sandbox"); launcherReportsGate(p) || p.Annotations[annReadinessReport] != "" {
		t.Errorf("a pod exposing no ports is not marked: %v", p.Annotations)
	}
	// Without the launcher status policy the pod keeps the annotation path.
	t.Setenv(swiftguest.LauncherStatusGateEnv, "")
	if p := buildPod(sb, "sandbox"); launcherReportsGate(p) {
		t.Errorf("no policy, no gate reporting: %v", p.Annotations)
	}
	if ri := buildIntent(sb, "sandbox", "/r.ext4", "", execSpec{Argv: []string{"/srv"}}, false); ri.SandboxProbes.ReadinessGate != "" {
		t.Errorf("no policy: intent gate = %q", ri.SandboxProbes.ReadinessGate)
	}
}

// spec.readinessStartup reaches swiftletd in milliseconds with its defaults
// applied; it is dropped when there is no readiness probe to speed up.
func TestProbesIntent_ReadinessStartup(t *testing.T) {
	sb := probedSandbox()
	sb.Spec.ReadinessStartup = &sandboxv1alpha1.SandboxReadinessStartup{Interval: &metav1.Duration{Duration: 50 * time.Millisecond}}
	got := probesIntent(sb, true)
	want := &runtimeintent.ReadinessStartupIntent{IntervalMs: 50, WindowMs: 30000}
	if got.ReadinessStartup == nil || *got.ReadinessStartup != *want {
		t.Errorf("readinessStartup = %+v, want %+v", got.ReadinessStartup, want)
	}
	if got.ReadinessGate != string(WorkloadReadyGate) {
		t.Errorf("readinessGate = %q", got.ReadinessGate)
	}
	if probesIntent(sb, false).ReadinessGate != "" {
		t.Error("a pod swiftletd does not report on gets no gate in its intent")
	}
	sb.Spec.ReadinessProbe = nil
	if got := probesIntent(sb, true); got.ReadinessStartup != nil || got.ReadinessGate != "" {
		t.Errorf("liveness only: %+v", got)
	}
}

// gatedLauncher is sb's launcher pod, marked for swiftletd to report the gate,
// with the gate condition set as swiftletd would.
func gatedLauncher(sb *sandboxv1alpha1.SwiftSandbox, actionID string, gate *corev1.PodCondition) *corev1.Pod {
	pod := ownLauncher(sb, buildPod(sb, "sandbox"))
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annReadinessReport] = readinessReportPodCondition
	if actionID != "" {
		pod.Annotations[annSandboxExecActionID] = actionID
	}
	pod.Status = corev1.PodStatus{
		Phase:             corev1.PodRunning,
		Conditions:        []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: launcherName, Ready: true}},
	}
	if gate != nil {
		pod.Status.Conditions = append(pod.Status.Conditions, *gate)
	}
	return pod
}

// countPodWrites wraps a test client and counts every write to a pod.
func countPodWrites(objs ...client.Object) (*SwiftSandboxReconciler, client.Client, *int) {
	n := 0
	count := func(obj client.Object) {
		if _, ok := obj.(*corev1.Pod); ok {
			n++
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				count(obj)
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				count(obj)
				return cl.Patch(ctx, obj, p, opts...)
			},
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				count(obj)
				return cl.SubResource(sub).Patch(ctx, obj, p, opts...)
			},
		}).Build()
	return &SwiftSandboxReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(20)}, c, &n
}

// On a pod swiftletd reports on, its verdict on the gate is the workload's
// readiness: the controller mirrors it to WorkloadReady and writes nothing
// to the pod. A cold launcher's message carries no workload prefix.
func TestReconcile_WorkloadReadyFromTheLaunchersGate(t *testing.T) {
	sb := probedSandbox()
	pod := gatedLauncher(sb, "", &corev1.PodCondition{Type: WorkloadReadyGate, Status: corev1.ConditionTrue,
		Reason: "ProbeSucceeded", Message: "http GET 192.168.99.10:3000/: 200"})
	r, c, writes := countPodWrites(sb, pod)
	reconcileSB(t, r, "sb")
	cond := apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Message != "http GET 192.168.99.10:3000/: 200" {
		t.Errorf("WorkloadReady = %+v", cond)
	}
	if *writes != 0 {
		t.Errorf("the controller wrote the pod %d times; swiftletd owns the gate", *writes)
	}
}

// Before swiftletd's first verdict the controller writes nothing either: the
// kubelet treats a missing gate condition as not ready, and a controller
// write there would be the second pod write the kubelet waits on.
func TestReconcile_NoGateWriteBeforeTheVerdict(t *testing.T) {
	sb := probedSandbox()
	r, c, writes := countPodWrites(sb, gatedLauncher(sb, "", nil))
	reconcileSB(t, r, "sb")
	cond := apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Probing" {
		t.Errorf("WorkloadReady = %+v", cond)
	}
	if *writes != 0 {
		t.Errorf("pod writes = %d, want 0", *writes)
	}
}

// A warm slot's verdict counts only for the workload this sandbox runs on it.
// A True that names another workload (stale, duplicated or delayed) is not
// this sandbox's readiness, and the controller clears it so the kubelet stops
// routing to the pod.
func TestGateReadiness_BelongsToTheCurrentWorkload(t *testing.T) {
	sb := probedSandbox()
	sb.UID = types.UID("uid-b")
	cases := []struct {
		name, action, msg string
		want              bool
	}{
		{"this workload", "uid-b", "workload uid-b: tcp connect ok", true},
		{"this workload, after an artifact fetch", "uid-b" + fetchedActionSuffix, "workload uid-b" + fetchedActionSuffix + ": ok", true},
		{"the first dispatch's verdict after a refetch", "uid-b" + fetchedActionSuffix, "workload uid-b: ok", false},
		{"another workload's verdict", "uid-b", "workload uid-a: ok", false},
		{"no workload in the message", "uid-b", "tcp connect ok", false},
		{"the slot runs another sandbox", "uid-a", "workload uid-a: ok", false},
	}
	for _, tc := range cases {
		pod := gatedLauncher(sb, tc.action, &corev1.PodCondition{Type: WorkloadReadyGate, Status: corev1.ConditionTrue, Message: tc.msg})
		ready, reason, msg := workloadReadiness(sb, pod)
		if ready != tc.want {
			t.Errorf("%s: ready = %v (%s %q), want %v", tc.name, ready, reason, msg, tc.want)
		}
		if ready && msg != "tcp connect ok" && msg != "ok" {
			t.Errorf("%s: the message keeps the prefix: %q", tc.name, msg)
		}
	}
}

func TestReconcile_ClearsAForeignTrueGate(t *testing.T) {
	sb := probedSandbox()
	sb.UID = types.UID("uid-b")
	pod := gatedLauncher(sb, "", &corev1.PodCondition{Type: WorkloadReadyGate, Status: corev1.ConditionTrue, Message: "workload uid-a: ok"})
	pod.Annotations[annSandboxExecActionID] = "uid-b"
	ready, _, _ := workloadReadiness(sb, pod)
	if ready {
		t.Fatal("a foreign verdict must not make the workload ready")
	}
	r, c := sandboxReconciler(sb, pod)
	if err := r.syncWorkloadReady(context.Background(), sb, pod); err != nil {
		t.Fatal(err)
	}
	if gate, _ := podGate(t, c, "sb"); gate != corev1.ConditionFalse {
		t.Errorf("gate = %q, want the foreign True cleared", gate)
	}
}

// A checkout without Secrets puts its workload in the claim: one pod write
// from the controller, so the kubelet applies the workload's readiness gate
// at once instead of after a PLEG relist.
func TestCheckout_ClaimCarriesTheWorkload(t *testing.T) {
	pool := shapedPool()
	pool.Namespace = "default"
	pool.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{{Name: "http-app", Port: 3000}}
	slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
	slot.Namespace = "default"
	slot.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: WorkloadReadyGate}}
	slot.Annotations[annReadinessReport] = readinessReportPodCondition
	sb := matchingSandbox()
	sb.Namespace = "default"
	sb.Spec.Network.Ports = pool.Spec.Network.Ports
	sb.Spec.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http-app")}}}
	sb.Spec.ReadinessStartup = &sandboxv1alpha1.SandboxReadinessStartup{}
	r, c, writes := countPodWrites(pool, slot, sb)
	reconcileSB(t, r, "sb")

	if *writes != 1 {
		t.Errorf("pod writes during the checkout = %d, want 1 (the claim)", *writes)
	}
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(slot), &p); err != nil {
		t.Fatal(err)
	}
	if p.Labels[SlotStateLabelKey] != slotStateClaimed || p.Annotations[annSandboxExecActionID] != string(sb.UID) {
		t.Fatalf("claim: labels %v annotations %v", p.Labels, p.Annotations)
	}
	var args struct {
		Probes *runtimeintent.SandboxProbesIntent `json:"probes"`
	}
	if err := json.Unmarshal([]byte(p.Annotations[annSandboxExecActionArgs]), &args); err != nil {
		t.Fatal(err)
	}
	if args.Probes == nil || args.Probes.ReadinessGate != string(WorkloadReadyGate) || args.Probes.ReadinessStartup == nil {
		t.Errorf("action probes = %+v", args.Probes)
	}
}

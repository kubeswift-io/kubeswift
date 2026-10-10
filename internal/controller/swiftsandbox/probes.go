package swiftsandbox

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// probesIntent turns spec.readinessProbe / spec.livenessProbe into what
// swiftletd runs; nil when the sandbox has neither. Callers validate first
// (ValidateProbes), so a port always resolves. reportsGate is whether the
// launcher pod that runs the workload has swiftletd report readiness on its
// readiness gate (launcherReportsGate).
func probesIntent(sb *sandboxv1alpha1.SwiftSandbox, reportsGate bool) *runtimeintent.SandboxProbesIntent {
	if sb.Spec.ReadinessProbe == nil && sb.Spec.LivenessProbe == nil {
		return nil
	}
	out := &runtimeintent.SandboxProbesIntent{
		Readiness: probeIntent(sb.Spec.ReadinessProbe, sb.Spec.Network),
		Liveness:  probeIntent(sb.Spec.LivenessProbe, sb.Spec.Network),
	}
	if sb.Spec.ReadinessProbe != nil {
		if st := sb.Spec.ReadinessStartup; st != nil {
			out.ReadinessStartup = &runtimeintent.ReadinessStartupIntent{
				IntervalMs: st.IntervalOrDefault().Milliseconds(),
				WindowMs:   st.WindowOrDefault().Milliseconds(),
			}
		}
		if reportsGate {
			out.ReadinessGate = string(WorkloadReadyGate)
		}
	}
	return out
}

// probeIntent applies the Kubernetes probe defaults: period 10s, timeout 1s,
// success threshold 1, failure threshold 3, HTTP path /.
func probeIntent(p *corev1.Probe, n sandboxv1alpha1.SandboxNetwork) *runtimeintent.SandboxProbeIntent {
	if p == nil {
		return nil
	}
	orDefault := func(v, d int32) int32 {
		if v <= 0 {
			return d
		}
		return v
	}
	out := &runtimeintent.SandboxProbeIntent{
		InitialDelaySeconds: p.InitialDelaySeconds,
		PeriodSeconds:       orDefault(p.PeriodSeconds, 10),
		TimeoutSeconds:      orDefault(p.TimeoutSeconds, 1),
		SuccessThreshold:    orDefault(p.SuccessThreshold, 1),
		FailureThreshold:    orDefault(p.FailureThreshold, 3),
	}
	if h := p.HTTPGet; h != nil {
		out.Kind = "http"
		out.Port, _ = sandboxwebhook.ProbePort(h.Port, n)
		out.Path = h.Path
		if out.Path == "" {
			out.Path = "/"
		}
		for _, hd := range h.HTTPHeaders {
			out.Headers = append(out.Headers, runtimeintent.ProbeHeader{Name: hd.Name, Value: hd.Value})
		}
		return out
	}
	out.Kind = "tcp"
	out.Port, _ = sandboxwebhook.ProbePort(p.TCPSocket.Port, n)
	return out
}

// Annotations swiftletd writes from the probes (rust/swiftletd/src/probe.rs).
const (
	annWorkloadReady       = "kubeswift.io/sandbox-workload-ready"
	annWorkloadReadyDetail = "kubeswift.io/sandbox-workload-ready-detail"
	annLivenessFailed      = "kubeswift.io/sandbox-liveness-failed"
)

// WorkloadReadyGate is the readiness gate on a launcher pod that exposes
// ports: the pod is Ready, and in a Service's endpoints, only while it is True.
const WorkloadReadyGate corev1.PodConditionType = "sandbox.kubeswift.io/workload-ready"

// annReadinessReport marks a launcher pod whose swiftletd writes the readiness
// probe's verdict to WorkloadReadyGate itself, through pods/status, instead of
// to annotations for the controller to copy. Set when the pod is built, so a
// pod from an older controller keeps the annotation path.
//
// One write matters here: after any pod write the kubelet waits for its next
// PLEG relist (up to 1 s) before it recomputes Ready for the next change, so
// an annotation and then the gate made Ready lag the workload by 1 to 2 s.
const (
	annReadinessReport          = "kubeswift.io/sandbox-readiness-report"
	readinessReportPodCondition = "pod-condition"
)

// launcherReportsGate is whether swiftletd on pod reports readiness to the
// workload readiness gate.
func launcherReportsGate(pod *corev1.Pod) bool {
	return pod.Annotations[annReadinessReport] == readinessReportPodCondition && hasWorkloadGate(pod)
}

func hasWorkloadGate(pod *corev1.Pod) bool {
	for _, g := range pod.Spec.ReadinessGates {
		if g.ConditionType == WorkloadReadyGate {
			return true
		}
	}
	return false
}

// workloadPrefix is how swiftletd starts the gate condition's message for a
// warm slot's workload (rust/swiftletd/src/probe.rs workload_prefix).
func workloadPrefix(actionID string) string {
	return "workload " + actionID + ": "
}

// livenessFailed reports swiftletd's liveness verdict and its message.
func livenessFailed(pod *corev1.Pod) (string, bool) {
	msg, ok := pod.Annotations[annLivenessFailed]
	return msg, ok
}

// workloadReadiness is whether the sandbox's workload is ready, and why: the
// readiness probe's last verdict, or, with no probe, the running guest.
func workloadReadiness(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) (bool, string, string) {
	if sb.Spec.ReadinessProbe == nil {
		return true, "NoReadinessProbe", "no readiness probe; the guest is running"
	}
	if launcherReportsGate(pod) {
		return gateReadiness(sb, pod)
	}
	detail := pod.Annotations[annWorkloadReadyDetail]
	switch pod.Annotations[annWorkloadReady] {
	case "true":
		return true, "ProbeSucceeded", detail
	case "false":
		return false, "ProbeFailed", detail
	default:
		return false, "Probing", "waiting for the readiness probe to pass"
	}
}

// gateReadiness reads the verdict swiftletd wrote to the readiness gate. On a
// warm slot the verdict counts only for the workload the slot runs for this
// sandbox: its message must name that workload.
func gateReadiness(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) (bool, string, string) {
	for _, c := range pod.Status.Conditions {
		if c.Type != WorkloadReadyGate {
			continue
		}
		msg, ok := gateMessage(sb, pod, c.Message)
		if !ok {
			break
		}
		switch c.Status {
		case corev1.ConditionTrue:
			return true, "ProbeSucceeded", msg
		case corev1.ConditionFalse:
			return false, "ProbeFailed", msg
		}
	}
	return false, "Probing", "waiting for the readiness probe to pass"
}

// gateMessage strips the workload prefix from a gate condition's message, and
// reports whether the condition belongs to this sandbox's workload. A cold
// launcher runs one workload, its own, and writes no prefix.
func gateMessage(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod, msg string) (string, bool) {
	id, warm := pod.Annotations[annSandboxExecActionID]
	if !warm {
		return msg, true
	}
	if id != string(sb.UID) && id != string(sb.UID)+fetchedActionSuffix {
		return "", false
	}
	if !strings.HasPrefix(msg, workloadPrefix(id)) {
		return "", false
	}
	return strings.TrimPrefix(msg, workloadPrefix(id)), true
}

// syncWorkloadReady sets the WorkloadReady condition and, on a pod with the
// readiness gate, keeps the gate in step: the controller writes it unless
// swiftletd does (launcherReportsGate), in which case it only clears a True
// verdict that does not belong to this sandbox's workload. The pod condition is
// a strategic merge on status.conditions (merge key: type), so the kubelet's
// own conditions are never overwritten.
func (r *SwiftSandboxReconciler) syncWorkloadReady(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) error {
	ready, reason, msg := workloadReadiness(sb, pod)
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionWorkloadReady, Status: status,
		Reason: reason, Message: msg, ObservedGeneration: sb.Generation,
	})

	if !hasWorkloadGate(pod) {
		return nil
	}
	var current corev1.ConditionStatus
	for _, c := range pod.Status.Conditions {
		if c.Type == WorkloadReadyGate {
			current = c.Status
		}
	}
	want := corev1.ConditionFalse
	if ready {
		want = corev1.ConditionTrue
	}
	if launcherReportsGate(pod) && sb.Spec.ReadinessProbe != nil {
		// swiftletd owns the gate. Step in only to clear a True that is not
		// this sandbox's workload's (gateReadiness then reports not ready).
		if ready || current != corev1.ConditionTrue {
			return nil
		}
	} else if current == want {
		return nil
	}
	orig := pod.DeepCopy()
	cond := corev1.PodCondition{Type: WorkloadReadyGate, Status: want, LastTransitionTime: metav1.Now(), Reason: reason, Message: msg}
	replaced := false
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == WorkloadReadyGate {
			pod.Status.Conditions[i], replaced = cond, true
		}
	}
	if !replaced {
		pod.Status.Conditions = append(pod.Status.Conditions, cond)
	}
	return r.Status().Patch(ctx, pod, client.StrategicMergeFrom(orig))
}

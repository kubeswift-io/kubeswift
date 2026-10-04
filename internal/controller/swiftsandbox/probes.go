package swiftsandbox

import (
	"context"

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
// (ValidateProbes), so a port always resolves.
func probesIntent(sb *sandboxv1alpha1.SwiftSandbox) *runtimeintent.SandboxProbesIntent {
	if sb.Spec.ReadinessProbe == nil && sb.Spec.LivenessProbe == nil {
		return nil
	}
	return &runtimeintent.SandboxProbesIntent{
		Readiness: probeIntent(sb.Spec.ReadinessProbe, sb.Spec.Network),
		Liveness:  probeIntent(sb.Spec.LivenessProbe, sb.Spec.Network),
	}
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

// syncWorkloadReady sets the WorkloadReady condition and, on a pod with the
// readiness gate, the matching pod condition, so a Service's endpoints follow
// the workload. The pod condition is a strategic merge on status.conditions
// (merge key: type), so the kubelet's own conditions are never overwritten.
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

	gated := false
	for _, g := range pod.Spec.ReadinessGates {
		gated = gated || g.ConditionType == WorkloadReadyGate
	}
	if !gated {
		return nil
	}
	want := corev1.ConditionFalse
	if ready {
		want = corev1.ConditionTrue
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == WorkloadReadyGate && c.Status == want {
			return nil
		}
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

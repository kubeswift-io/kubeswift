package swiftsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

const (
	// retentionMaxRequeue caps the wait before re-checking a terminal sandbox for
	// ttl expiry (mirrors swiftmigration).
	retentionMaxRequeue = time.Hour
	// pollInterval re-checks a live sandbox (also drives the spec.timeout gate).
	pollInterval = 5 * time.Second
)

// SwiftSandboxReconciler runs OCI-rootfs microVM sandboxes.
type SwiftSandboxReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func isTerminal(p sandboxv1alpha1.SwiftSandboxPhase) bool {
	return p == sandboxv1alpha1.SwiftSandboxCompleted || p == sandboxv1alpha1.SwiftSandboxFailed
}

func (r *SwiftSandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sb sandboxv1alpha1.SwiftSandbox
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if sb.DeletionTimestamp != nil {
		// The launcher pod + intent ConfigMap are owned by the SwiftSandbox and GC
		// with it; the digest-keyed rootfs cache is a shared node artifact, not
		// per-sandbox owned. No finalizer needed in v1.
		return ctrl.Result{}, nil
	}
	if isTerminal(sb.Status.Phase) {
		return r.handleRetention(ctx, &sb)
	}

	if err := swiftguest.EnsureSwiftletdRBAC(ctx, r.Client, sb.Namespace); err != nil {
		return ctrl.Result{}, err
	}

	kernelName := defaultKernelProfile
	if sb.Spec.KernelProfileRef != nil && sb.Spec.KernelProfileRef.Name != "" {
		kernelName = sb.Spec.KernelProfileRef.Name
	}

	// spec.poolRef: claim a warm slot from a SwiftSandboxPool (sub-second checkout) and
	// inject the workload over vsock, else cold-fallback. Own path (its pod is a re-parented
	// pool slot, or a cold pod named sb.Name — see reconcilePooled).
	if sb.Spec.PoolRef != nil {
		return r.reconcilePooled(ctx, &sb, kernelName)
	}

	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, &pod)
	switch {
	case apierrors.IsNotFound(err):
		return r.createLaunch(ctx, &sb, kernelName)
	case err != nil:
		return ctrl.Result{}, err
	}
	return r.reconcilePodState(ctx, &sb, &pod)
}

// createLaunch resolves the image and creates the intent ConfigMap + launcher pod.
func (r *SwiftSandboxReconciler) createLaunch(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, kernelName string) (ctrl.Result, error) {
	ri, err := resolveImage(sb)
	if err != nil {
		return r.fail(ctx, sb, "ImageResolveFailed", err.Error())
	}
	intent := buildIntent(sb, kernelName, ri.RootfsPath, ri.Exec)
	intentJSON, err := runtimeintent.Serialize(intent)
	if err != nil {
		return ctrl.Result{}, err
	}
	cm := buildIntentConfigMap(sb, intentJSON)
	if err := controllerutil.SetControllerReference(sb, cm, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	pod := buildPod(sb, kernelName)
	if err := controllerutil.SetControllerReference(sb, pod, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	if networked(sb) {
		np := buildNetworkPolicy(sb)
		if err := controllerutil.SetControllerReference(sb, np, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, np); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	}
	sb.Status.Rootfs = &sandboxv1alpha1.SandboxRootfsStatus{Digest: ri.Digest, CachePath: ri.RootfsPath}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionResolved, Status: metav1.ConditionTrue,
		Reason: "Resolved", Message: "image digest " + ri.Digest, ObservedGeneration: sb.Generation,
	})
	return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxMaterializing, "materializing rootfs")
}

func (r *SwiftSandboxReconciler) reconcilePodState(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) (ctrl.Result, error) {
	// Run cap.
	if sb.Status.StartedAt != nil && sb.Spec.Timeout != nil &&
		time.Since(sb.Status.StartedAt.Time) > sb.Spec.Timeout.Duration {
		_ = r.Delete(ctx, pod)
		return r.fail(ctx, sb, "DeadlineExceeded", "sandbox exceeded spec.timeout")
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return r.finishTerminal(ctx, sb, pod)
	}

	// Materialize init failure (before the launcher starts) is an honest, specific
	// terminal state, not a generic pod failure.
	for i := range pod.Status.InitContainerStatuses {
		cs := &pod.Status.InitContainerStatuses[i]
		if cs.Name == materializeInitName && cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return r.fail(ctx, sb, "RootfsMaterializeFailed", firstNonEmpty(cs.State.Terminated.Message, "sandbox-materialize failed"))
		}
	}

	if launcherReady(pod) {
		if sb.Status.StartedAt == nil {
			now := metav1.Now()
			sb.Status.StartedAt = &now
		}
		sb.Status.NodeName = pod.Spec.NodeName
		sb.Status.PodRef = pod.Name
		applyMaterializeResult(sb, pod)
		applyGuestAnnotations(sb, pod)
		apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
			Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning, Status: metav1.ConditionTrue,
			Reason: "GuestRunning", Message: guestRunningMessage(sb), ObservedGeneration: sb.Generation,
		})
		return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxRunning, "guest running")
	}

	// Still coming up (materializing / launcher not ready).
	return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxMaterializing, "materializing rootfs")
}

// finishTerminal maps a terminated launcher pod to Completed/Failed. It prefers the
// WORKLOAD's exit code — swiftletd writes it to kubeswift.io/sandbox-exit-code from the
// guest console's KUBESWIFT-EXIT-CODE marker — because CH/the launcher exit 0 on a
// clean power-off regardless of what the workload returned. When the marker is absent
// (e.g. the guest died before the bridge emitted it), it falls back to the pod phase +
// the launcher container's exit code.
func (r *SwiftSandboxReconciler) finishTerminal(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) (ctrl.Result, error) {
	applyMaterializeResult(sb, pod)
	if code, ok := sandboxWorkloadExitCode(pod); ok {
		sb.Status.ExitCode = &code
		if code == 0 {
			return r.terminal(ctx, sb, sandboxv1alpha1.SwiftSandboxCompleted, "Completed", "workload exited 0")
		}
		return r.terminal(ctx, sb, sandboxv1alpha1.SwiftSandboxFailed, "WorkloadFailed", fmt.Sprintf("workload exited %d", code))
	}
	if code, ok := launcherExitCode(pod); ok {
		sb.Status.ExitCode = &code
	}
	if pod.Status.Phase == corev1.PodFailed {
		return r.terminal(ctx, sb, sandboxv1alpha1.SwiftSandboxFailed, "GuestFailed", podFailureMessage(pod))
	}
	return r.terminal(ctx, sb, sandboxv1alpha1.SwiftSandboxCompleted, "Completed", "workload exited")
}

func (r *SwiftSandboxReconciler) fail(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, reason, msg string) (ctrl.Result, error) {
	return r.terminal(ctx, sb, sandboxv1alpha1.SwiftSandboxFailed, reason, msg)
}

// terminal stamps the terminal phase + terminalAt (once) and persists.
func (r *SwiftSandboxReconciler) terminal(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, phase sandboxv1alpha1.SwiftSandboxPhase, reason, msg string) (ctrl.Result, error) {
	if sb.Status.TerminalAt == nil {
		now := metav1.Now()
		sb.Status.TerminalAt = &now
	}
	sb.Status.Phase = phase
	sb.Status.Message = msg
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning, Status: metav1.ConditionFalse,
		Reason: reason, Message: msg, ObservedGeneration: sb.Generation,
	})
	if err := r.Status().Update(ctx, sb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SwiftSandboxReconciler) setPhase(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, phase sandboxv1alpha1.SwiftSandboxPhase, msg string) (ctrl.Result, error) {
	sb.Status.Phase = phase
	sb.Status.Message = msg
	if err := r.Status().Update(ctx, sb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// handleRetention deletes a terminal sandbox once terminalAt+ttl has elapsed
// (mirrors swiftmigration.handleTerminalRetention; the 1h requeue cap).
func (r *SwiftSandboxReconciler) handleRetention(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	if sb.Spec.TTL == nil || sb.Status.TerminalAt == nil {
		return ctrl.Result{}, nil
	}
	expiry := sb.Status.TerminalAt.Add(sb.Spec.TTL.Duration)
	if wait := time.Until(expiry); wait > 0 {
		if wait > retentionMaxRequeue {
			wait = retentionMaxRequeue
		}
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	log.FromContext(ctx).Info("SwiftSandbox TTL expired; deleting", "sandbox", sb.Name)
	if err := r.Delete(ctx, sb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

func (r *SwiftSandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.SwiftSandbox{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}

// --- pod-state helpers ---

func launcherReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == launcherName {
			return cs.Ready || cs.State.Running != nil
		}
	}
	return false
}

func launcherExitCode(pod *corev1.Pod) (int32, bool) {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == launcherName && cs.State.Terminated != nil {
			return cs.State.Terminated.ExitCode, true
		}
	}
	return 0, false
}

// annSandboxExitCode carries the workload's real exit code, written by swiftletd from
// the guest console's KUBESWIFT-EXIT-CODE marker (see rust/swiftletd/src/report.rs).
const annSandboxExitCode = "kubeswift.io/sandbox-exit-code"

// sandboxWorkloadExitCode reads the workload exit code swiftletd recorded on the
// launcher pod. Absent when the guest died before the bridge-init could emit it.
func sandboxWorkloadExitCode(pod *corev1.Pod) (int32, bool) {
	v := pod.Annotations[annSandboxExitCode]
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

func podFailureMessage(pod *corev1.Pod) string {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == launcherName && cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
			return cs.State.Terminated.Message
		}
	}
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	return "launcher pod failed"
}

// applyMaterializeResult reads the sandbox-materialize init container's
// termination message (best-effort) into status.rootfs (mirrors the snapshot
// clonecommon.JobTransferReport pattern). Missing/unparseable = no-op.
func applyMaterializeResult(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) {
	for i := range pod.Status.InitContainerStatuses {
		cs := &pod.Status.InitContainerStatuses[i]
		if cs.Name != materializeInitName || cs.State.Terminated == nil || cs.State.Terminated.Message == "" {
			continue
		}
		var res materialize.Result
		if json.Unmarshal([]byte(cs.State.Terminated.Message), &res) != nil {
			return
		}
		if sb.Status.Rootfs == nil {
			sb.Status.Rootfs = &sandboxv1alpha1.SandboxRootfsStatus{}
		}
		if res.Digest != "" {
			sb.Status.Rootfs.Digest = res.Digest
		}
		if res.RootfsPath != "" {
			sb.Status.Rootfs.CachePath = res.RootfsPath
		}
		sb.Status.Rootfs.SizeBytes = res.SizeBytes
		return
	}
}

// applyGuestAnnotations maps swiftletd's launcher-pod annotations onto the sandbox
// status — the same reporting path SwiftGuest uses (see
// internal/controller/swiftguest/status.go::MapPodToStatus). swiftletd writes these
// once CH reaches socket-ready (runtime) and once the guest DHCPs (network).
// Best-effort: a missing/blank annotation leaves the field untouched, so a
// network:none sandbox (no lease) simply keeps status.network nil.
func applyGuestAnnotations(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) {
	if pidStr := pod.Annotations[swiftguest.PodAnnotationGuestRuntimePID]; pidStr != "" {
		if pid, err := strconv.ParseInt(pidStr, 10, 64); err == nil {
			hypervisor := pod.Annotations[swiftguest.PodAnnotationGuestHypervisor]
			if hypervisor == "" {
				hypervisor = "cloud-hypervisor"
			}
			sb.Status.Runtime = &sandboxv1alpha1.SandboxRuntimeStatus{PID: pid, Hypervisor: hypervisor}
		}
	}
	if ip := pod.Annotations[swiftguest.PodAnnotationGuestIP]; ip != "" {
		sb.Status.Network = &sandboxv1alpha1.SandboxNetworkStatus{PrimaryIP: ip}
	}
}

// guestRunningMessage describes the GuestRunning=True condition. It prefers the
// swiftletd-reported runtime (real guest confirmation) over the bare
// launcher-readiness proxy, so `kubectl describe` shows the pid/IP when known.
func guestRunningMessage(sb *sandboxv1alpha1.SwiftSandbox) string {
	if sb.Status.Runtime != nil && sb.Status.Runtime.PID > 0 {
		msg := fmt.Sprintf("cloud-hypervisor running (pid %d)", sb.Status.Runtime.PID)
		if sb.Status.Network != nil && sb.Status.Network.PrimaryIP != "" {
			msg += ", ip " + sb.Status.Network.PrimaryIP
		}
		return msg
	}
	return "launcher ready (guest starting)"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

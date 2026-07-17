package swiftsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

// sandbox-exec action/status annotation keys — MUST match the swiftletd action loop's
// SANDBOX_KEYS (rust/swiftletd/src/action.rs). The controller writes the action; swiftletd
// runs the workload over vsock and writes the status (exit code in the detail).
const (
	annSandboxExecAction       = "kubeswift.io/sandbox-exec-action"
	annSandboxExecActionID     = "kubeswift.io/sandbox-exec-action-id"
	annSandboxExecActionArgs   = "kubeswift.io/sandbox-exec-action-args"
	annSandboxExecStatus       = "kubeswift.io/sandbox-exec-status"
	annSandboxExecStatusID     = "kubeswift.io/sandbox-exec-status-id"
	annSandboxExecStatusDetail = "kubeswift.io/sandbox-exec-status-detail"
)

// reconcilePooled drives a SwiftSandbox that references a warm SwiftSandboxPool
// (spec.poolRef): it claims a pre-booted warm slot and injects this sandbox's workload
// over vsock (sub-second checkout), falling back to the cold path on a miss.
//
// status.podRef discriminates the states after the first reconcile:
//   - "" : first reconcile — adopt an existing claim, else claim, else cold-fallback.
//   - != sb.Name : a claimed warm slot (its pod is <pool>-slot-<x>).
//   - == sb.Name : a cold-fallback — behaves exactly like a non-pooled sandbox.
func (r *SwiftSandboxReconciler) reconcilePooled(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, kernelName string) (ctrl.Result, error) {
	if sb.Status.PodRef != "" && sb.Status.PodRef != sb.Name {
		return r.reconcileClaimedSlot(ctx, sb)
	}
	if sb.Status.PodRef == sb.Name {
		// A prior pool miss fell back to the cold path — follow it (the normal flow).
		var pod corev1.Pod
		err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}, &pod)
		if apierrors.IsNotFound(err) {
			return r.createLaunch(ctx, sb, kernelName)
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		return r.reconcilePodState(ctx, sb, &pod)
	}

	// First reconcile. The workload argv comes from the sandbox spec with NO registry
	// pull (the sub-second point); a sandbox with no command needs the image entrypoint,
	// which only the cold materialize path knows — so it cold-falls-back.
	// The pool resolved the image env once (status.imageEnv); merge spec.env over it so
	// the injected workload sees the image env too — parity with a cold sandbox, no pull.
	var imageEnv []string
	var pool sandboxv1alpha1.SwiftSandboxPool
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.PoolRef.Name}, &pool); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// Pool gone — tryClaimWarmSlot finds no slots and cold-falls-back below.
	} else {
		imageEnv = pool.Status.ImageEnv
	}
	argv, env, cwd := poolExecArgs(sb, imageEnv)
	if len(argv) == 0 {
		return r.coldFallback(ctx, sb, kernelName, "no command to inject (needs image entrypoint)")
	}

	// Adopt an already-claimed slot from a partial prior reconcile (the pod claim
	// succeeded but the status update didn't) before claiming a new one — no double-claim.
	slot, err := r.findClaimedSlot(ctx, sb)
	if err != nil {
		return ctrl.Result{}, err
	}
	if slot == nil {
		if slot, err = r.tryClaimWarmSlot(ctx, sb); err != nil {
			return ctrl.Result{}, err
		}
		if slot == nil {
			return r.coldFallback(ctx, sb, kernelName, "no warm slot available")
		}
		// Inject the workload once, on the fresh claim (an adopted slot already has it).
		if err := r.stampExecAction(ctx, slot, sb, argv, env, cwd); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(sb, corev1.EventTypeNormal, "CheckedOut",
			"claimed warm slot %s from pool %s", slot.Name, sb.Spec.PoolRef.Name)
		if metrics.MarkSandboxCheckoutObserved(string(sb.UID)) {
			metrics.SandboxCheckoutsTotal.WithLabelValues("hit").Inc()
		}
	}

	now := metav1.Now()
	sb.Status.PodRef = slot.Name
	sb.Status.NodeName = slot.Spec.NodeName
	if sb.Status.StartedAt == nil {
		sb.Status.StartedAt = &now
	}
	// Echo the pool-shared model onto the checkout for observability (the slot was
	// booted with it mounted RO at MountPath). The digest lives on the pool status.
	if pool.Spec.Model != nil {
		sb.Status.Model = &sandboxv1alpha1.SandboxModelStatus{MountPath: pool.Spec.Model.ModelMountPath()}
	}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning, Status: metav1.ConditionTrue,
		Reason:             "CheckedOut",
		Message:            "claimed warm slot " + slot.Name + " from pool " + sb.Spec.PoolRef.Name,
		ObservedGeneration: sb.Generation,
	})
	return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxRunning,
		"running (checked out from pool "+sb.Spec.PoolRef.Name+")")
}

// coldFallback records the miss and delegates to the normal cold path, marking
// status.podRef=sb.Name so subsequent reconciles follow the cold pod (no re-claim).
func (r *SwiftSandboxReconciler) coldFallback(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, kernelName, reason string) (ctrl.Result, error) {
	log.FromContext(ctx).Info("SwiftSandbox pool miss; cold-fallback", "sandbox", sb.Name, "reason", reason)
	r.Recorder.Eventf(sb, corev1.EventTypeNormal, "PoolColdFallback",
		"pool %s: %s; booting cold", sb.Spec.PoolRef.Name, reason)
	if metrics.MarkSandboxCheckoutObserved(string(sb.UID)) {
		metrics.SandboxCheckoutsTotal.WithLabelValues("cold").Inc()
	}
	sb.Status.PodRef = sb.Name
	return r.createLaunch(ctx, sb, kernelName)
}

// poolExecArgs builds the workload exec (argv/env/cwd) from the sandbox spec WITHOUT a
// registry pull. argv = command + args (nil when no command — the caller cold-falls-back
// to resolve the image entrypoint). env is the pool's resolved image env with spec.env
// overlaid by key (parity with the cold path's mergeEnv), so the injected workload sees
// the image env too; the pool supplied imageEnv from its one-time resolve.
func poolExecArgs(sb *sandboxv1alpha1.SwiftSandbox, imageEnv []string) (argv, env []string, cwd string) {
	if len(sb.Spec.Command) == 0 {
		return nil, nil, ""
	}
	argv = append(argv, sb.Spec.Command...)
	argv = append(argv, sb.Spec.Args...)
	env = mergeEnv(imageEnv, sb.Spec.Env)
	return argv, env, sb.Spec.WorkingDir
}

// findClaimedSlot returns this sandbox's already-claimed slot pod, if any (labeled
// SandboxLabelKey=sb.Name + slot-state=claimed) — used to adopt a partial claim.
func (r *SwiftSandboxReconciler) findClaimedSlot(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(sb.Namespace),
		client.MatchingLabels{SandboxLabelKey: sb.Name, SlotStateLabelKey: slotStateClaimed}); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			return &pods.Items[i], nil
		}
	}
	return nil, nil
}

// tryClaimWarmSlot atomically claims a Ready warm slot of the sandbox's pool: it flips
// the slot's slot-state warm->claimed, labels it for this sandbox, and re-parents its pod
// from the pool to this SwiftSandbox. A conflicting concurrent claim loses the optimistic
// Update (409) and the loop tries the next slot. Returns nil when none is claimable.
func (r *SwiftSandboxReconciler) tryClaimWarmSlot(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(sb.Namespace),
		client.MatchingLabels{PoolLabelKey: sb.Spec.PoolRef.Name, SlotStateLabelKey: slotStateWarm}); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil || !launcherReady(p) {
			continue
		}
		claimed := p.DeepCopy()
		if claimed.Labels == nil {
			claimed.Labels = map[string]string{}
		}
		claimed.Labels[SlotStateLabelKey] = slotStateClaimed
		claimed.Labels[SandboxLabelKey] = sb.Name
		// Re-parent: drop the pool's controller ownerRef, make this sandbox the owner so
		// the slot pod GCs with the sandbox (and the pool replenishes the warm count).
		claimed.OwnerReferences = nonControllerRefs(claimed.OwnerReferences)
		if err := controllerutil.SetControllerReference(sb, claimed, r.Scheme); err != nil {
			return nil, err
		}
		if err := r.Update(ctx, claimed); err != nil {
			if apierrors.IsConflict(err) {
				continue // another claim won this slot; try the next
			}
			return nil, err
		}
		return claimed, nil
	}
	return nil, nil
}

func nonControllerRefs(refs []metav1.OwnerReference) []metav1.OwnerReference {
	var out []metav1.OwnerReference
	for _, ref := range refs {
		if ref.Controller == nil || !*ref.Controller {
			out = append(out, ref)
		}
	}
	return out
}

// stampExecAction writes the sandbox-exec action annotations on the claimed slot pod so
// swiftletd runs the workload over vsock (SANDBOX_KEYS). The action-id is the sandbox UID
// (idempotent + correlatable with the status swiftletd writes back).
func (r *SwiftSandboxReconciler) stampExecAction(ctx context.Context, slot *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox, argv, env []string, cwd string) error {
	args := map[string]interface{}{"argv": argv}
	if len(env) > 0 {
		args["env"] = env
	}
	if cwd != "" {
		args["cwd"] = cwd
	}
	if sb.Spec.Timeout != nil {
		args["timeoutSeconds"] = int64(sb.Spec.Timeout.Duration.Seconds())
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return err
	}
	patch := client.MergeFrom(slot.DeepCopy())
	if slot.Annotations == nil {
		slot.Annotations = map[string]string{}
	}
	slot.Annotations[annSandboxExecAction] = "run"
	slot.Annotations[annSandboxExecActionID] = string(sb.UID)
	slot.Annotations[annSandboxExecActionArgs] = string(argsJSON)
	return r.Patch(ctx, slot, patch)
}

// reconcileClaimedSlot drives a checked-out sandbox: it waits for swiftletd's
// sandbox-exec-status (mirroring our action-id = sb.UID), maps the exec exit code to the
// terminal phase, and destroys the consumed slot (the pool replenishes a fresh one). The
// slot's pod does NOT terminate on workload exit (its idle keeper keeps running), so the
// terminal signal is the exec status annotation, not pod termination.
func (r *SwiftSandboxReconciler) reconcileClaimedSlot(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Status.PodRef}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, sb, "SlotLost", "claimed warm slot pod disappeared")
		}
		return ctrl.Result{}, err
	}
	if sb.Status.StartedAt != nil && sb.Spec.Timeout != nil &&
		time.Since(sb.Status.StartedAt.Time) > sb.Spec.Timeout.Duration {
		_ = r.Delete(ctx, &pod)
		return r.fail(ctx, sb, "DeadlineExceeded", "sandbox exceeded spec.timeout")
	}
	applyGuestAnnotations(sb, &pod) // pid / ip (best-effort)

	// Trust the exec status only once it mirrors OUR action-id.
	if pod.Annotations[annSandboxExecStatusID] != string(sb.UID) {
		return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxRunning, "running (checked out)")
	}
	switch pod.Annotations[annSandboxExecStatus] {
	case "complete":
		code := int32(0)
		if n, err := strconv.ParseInt(pod.Annotations[annSandboxExecStatusDetail], 10, 32); err == nil {
			code = int32(n)
		}
		sb.Status.ExitCode = &code
		_ = r.Delete(ctx, &pod) // consume the slot; the pool replenishes a fresh warm one
		if code == 0 {
			return r.terminal(ctx, sb, sandboxv1alpha1.SwiftSandboxCompleted, "Completed", "workload exited 0")
		}
		return r.terminal(ctx, sb, sandboxv1alpha1.SwiftSandboxFailed, "WorkloadFailed",
			fmt.Sprintf("workload exited %d", code))
	case "failed":
		_ = r.Delete(ctx, &pod)
		return r.fail(ctx, sb, "ExecFailed",
			"checkout exec failed: "+pod.Annotations[annSandboxExecStatusDetail])
	default:
		return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxRunning, "running (checked out)")
	}
}

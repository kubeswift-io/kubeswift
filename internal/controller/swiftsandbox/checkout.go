package swiftsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
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
	poolFound := true
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.PoolRef.Name}, &pool); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		poolFound = false
	} else {
		imageEnv = pool.Status.ImageEnv
	}
	argv, env, cwd := poolExecArgs(sb, imageEnv)
	if len(argv) == 0 {
		return r.coldFallback(ctx, sb, kernelName, "no command to inject (needs image entrypoint)")
	}

	// Artifacts are resolved and authorized with this sandbox's credentials
	// before a slot is claimed, so no slot is held while the registry is asked
	// (cached for a digest reference, resolver.go). Also on an adopted slot,
	// whose status may not have been written.
	if len(sb.Spec.Artifacts) > 0 {
		if err := sandboxwebhook.ValidateArtifacts(&sb.Spec); err != nil {
			return r.fail(ctx, sb, "InvalidArtifacts", err.Error())
		}
		arts, ready, err := resolveArtifacts(ctx, r.APIReader, sb, r.lookup(sb))
		if err != nil {
			if registryRefused(err) {
				r.registryFailures.Delete(client.ObjectKeyFromObject(sb))
				return r.fail(ctx, sb, "ArtifactResolveFailed", err.Error())
			}
			return r.registryUnavailable(ctx, sb, err)
		}
		if !ready {
			return r.waitForRegistry(ctx, sb)
		}
		r.registryFailures.Delete(client.ObjectKeyFromObject(sb))
		sb.Status.Artifacts = arts
	}

	// Adopt an already-claimed slot from a partial prior reconcile (the pod claim
	// succeeded but the status update didn't) before claiming a new one — no double-claim.
	// It matched when it was claimed; a pool edit since must not strand it.
	slot, err := r.findClaimedSlot(ctx, sb)
	if err != nil {
		return ctrl.Result{}, err
	}
	if slot == nil {
		if !poolFound {
			return r.coldFallback(ctx, sb, kernelName, "pool not found")
		}
		// Refused before a slot is claimed: it would land on a privileged pod.
		if err := sandboxwebhook.ValidatePodMetadata(sb.Spec.PodMetadata); err != nil {
			return r.fail(ctx, sb, "InvalidPodMetadata", err.Error())
		}
		if err := sandboxwebhook.ValidateProbes(&sb.Spec); err != nil {
			return r.fail(ctx, sb, "InvalidProbe", err.Error())
		}
		if err := sandboxwebhook.ValidateEnv(sb.Spec.Env); err != nil {
			return r.fail(ctx, sb, "InvalidEnv", err.Error())
		}
		if err := sandboxwebhook.ValidateSecretFiles(&sb.Spec); err != nil {
			return r.fail(ctx, sb, "InvalidSecretFiles", err.Error())
		}
		if err := sandboxwebhook.ValidateArtifacts(&sb.Spec); err != nil {
			return r.fail(ctx, sb, "InvalidArtifacts", err.Error())
		}
		// Missing Secrets: wait without holding a slot.
		if reason, msg, err := checkSecretEnv(ctx, r.APIReader, sb); err != nil {
			return ctrl.Result{}, err
		} else if reason != "" {
			return r.waitForReference(ctx, sb, reason, msg)
		}
		// A checkout only injects a command: the slot's shape is what the
		// workload gets. Only a sandbox the slot honors may take one; anything
		// else boots cold with its own settings, and the Event says why.
		if diff := slotMismatches(&pool, sb); len(diff) > 0 {
			return r.coldFallback(ctx, sb, kernelName,
				"its slots differ from this sandbox in "+strings.Join(diff, ", "))
		}
		if slot, err = r.tryClaimWarmSlot(ctx, sb, poolSlotProfile(&pool)); err != nil {
			return ctrl.Result{}, err
		}
		if slot == nil {
			if len(sb.Spec.Artifacts) > 0 {
				return r.coldFallback(ctx, sb, kernelName,
					"no warm slot that can take artifacts is available (a slot needs kernels/sandbox 6.6.15 or kernels/gpu-sandbox 6.6.4 or newer)")
			}
			return r.coldFallback(ctx, sb, kernelName, "no warm slot available")
		}
		r.Recorder.Eventf(sb, corev1.EventTypeNormal, "CheckedOut",
			"claimed warm slot %s from pool %s", slot.Name, sb.Spec.PoolRef.Name)
		if metrics.MarkSandboxCheckoutObserved(string(sb.UID)) {
			metrics.SandboxCheckoutsTotal.WithLabelValues("hit").Inc()
		}
	}

	// Inject the workload unless this sandbox's already is. Checked on an
	// adopted slot too: a claim whose inject failed used to be adopted on the
	// next pass as if injected, and the workload never ran. The slot's own
	// account is granted this sandbox's Secrets first, so swiftletd can read
	// them the moment it sees the action.
	if !ownExecAction(slot, sb) {
		if err := swiftguest.EnsureLauncherIdentity(ctx, r.Client, r.Scheme, slot, slot.Name,
			swiftguest.SandboxLauncher, slotAccount(slot), secretNamesFor(slotAccount(slot), sb, slot.Name)); err != nil {
			return ctrl.Result{}, err
		}
		arts, err := warmArtifactArgs(ctx, r.APIReader, sb)
		if err != nil {
			_ = r.Delete(ctx, slot)
			return r.fail(ctx, sb, "ArtifactResolveFailed", err.Error())
		}
		if err := r.stampExecAction(ctx, slot, sb, string(sb.UID), argv, env, cwd, arts); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The slot's intent ConfigMap and deny-ingress NetworkPolicy go with its
	// pod to the claiming sandbox. Left pool-owned they outlived the checkout
	// -- one orphaned pair per claim, until the pool itself was deleted.
	if err := r.adoptSlotObjects(ctx, sb, slot); err != nil {
		return ctrl.Result{}, err
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
	// The slot's network-init enforces the allowlist it was booted with.
	setEgressAllowed(sb, parseEgressAllowed(slot.Annotations[EgressAllowedAnnotation]))
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
// profile is the pool's current slot profile; the caller has checked it honors sb.
func (r *SwiftSandboxReconciler) tryClaimWarmSlot(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, profile string) (*corev1.Pod, error) {
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
		// Only a slot booted under the pool's current shape (a slot from
		// before a pool edit is not; the pool recycles it).
		if p.Annotations[SlotProfileAnnotation] != profile {
			continue
		}
		// Secrets are granted only to a launcher's own account; a slot from
		// before per-pod accounts runs as the shared one.
		if usesSecrets(sb) && slotAccount(p) != swiftguest.SandboxLauncherServiceAccountFor(p.Name) {
			continue
		}
		// Artifacts need a slot whose bridge and launcher can project them.
		if len(sb.Spec.Artifacts) > 0 && !slotSupportsWarmMounts(p) {
			continue
		}
		claimed := p.DeepCopy()
		if claimed.Labels == nil {
			claimed.Labels = map[string]string{}
		}
		claimed.Labels[SlotStateLabelKey] = slotStateClaimed
		claimed.Labels[SandboxLabelKey] = sb.Name
		// spec.podMetadata, in the same write as the claim: a Service
		// selecting the sandbox by its labels never sees a half-claimed slot.
		applyPodMetadata(claimed, sb.Spec.PodMetadata)
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
func (r *SwiftSandboxReconciler) stampExecAction(ctx context.Context, slot *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox, actionID string, argv, env []string, cwd string, arts []warmArtifactArg) error {
	args := map[string]interface{}{"argv": argv}
	if len(arts) > 0 {
		args["artifacts"] = arts
	}
	if len(env) > 0 {
		args["env"] = env
	}
	if cwd != "" {
		args["cwd"] = cwd
	}
	if sb.Spec.Timeout != nil {
		args["timeoutSeconds"] = int64(sb.Spec.Timeout.Duration.Seconds())
	}
	if probes := probesIntent(sb); probes != nil {
		args["probes"] = probes
	}
	// References only; swiftletd reads the values and sends them over vsock.
	if refs := secretEnvRefs(sb); len(refs) > 0 {
		args["secretEnv"] = refs
	}
	if refs := secretFileRefs(sb); len(refs) > 0 {
		args["secretFiles"] = refs
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
	slot.Annotations[annSandboxExecActionID] = actionID
	slot.Annotations[annSandboxExecActionArgs] = string(argsJSON)
	return r.Patch(ctx, slot, patch)
}

// reconcileClaimedSlot drives a checked-out sandbox: it waits for swiftletd's
// sandbox-exec-status (mirroring our action-id = sb.UID), maps the exec exit code to the
// terminal phase, and destroys the consumed slot (the pool replenishes a fresh one). The
// slot's pod does NOT terminate on workload exit (its idle keeper keeps running), so the
// terminal signal is the exec status annotation, not pod termination. A slot pod that
// ends, or goes away, before that status arrives ends the sandbox as Failed.
func (r *SwiftSandboxReconciler) reconcileClaimedSlot(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: sb.Status.PodRef}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, sb, "SlotLost", fmt.Sprintf(
				"claimed warm slot pod %s is gone and the workload never reported an exit", sb.Status.PodRef))
		}
		return ctrl.Result{}, err
	}
	if sb.Status.StartedAt != nil && sb.Spec.Timeout != nil &&
		time.Since(sb.Status.StartedAt.Time) > sb.Spec.Timeout.Duration {
		_ = r.Delete(ctx, &pod)
		return r.fail(ctx, sb, "DeadlineExceeded", "sandbox exceeded spec.timeout")
	}
	applyGuestAnnotations(sb, &pod) // pid / ip (best-effort)

	// Trust the exec status only once it mirrors OUR action-id. A final status
	// decides the outcome even if the pod has ended since: swiftletd writes it
	// before its launcher stops.
	// The status must answer the action now on the slot: after an artifact
	// fetch that is the second dispatch, and the first one's ArtifactMissing
	// no longer counts.
	actionID := pod.Annotations[annSandboxExecActionID]
	if actionID == "" {
		actionID = string(sb.UID)
	}
	if (actionID == string(sb.UID) || actionID == string(sb.UID)+fetchedActionSuffix) &&
		pod.Annotations[annSandboxExecStatusID] == actionID {
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
			detail := pod.Annotations[annSandboxExecStatusDetail]
			// The first dispatch found artifacts missing on the node: fetch
			// them there and dispatch again, once. The VM stays booted.
			if missing, ok := artifactMissing(detail); ok && actionID == string(sb.UID) {
				return r.fetchMissingArtifacts(ctx, sb, &pod, missing)
			}
			_ = r.Delete(ctx, &pod)
			reason, msg := checkoutFailure(detail)
			return r.fail(ctx, sb, reason, msg)
		}
	}

	// No final status, and the slot pod has ended: swiftletd is gone with it,
	// so the status will never come. The sandbox used to stay Running until
	// spec.timeout, or for good without one. The workload's exit code is not
	// known and stays unset; the launcher's goes in the message. The pod is
	// kept for its logs, as a cold sandbox's launcher is. It holds nothing
	// now: the pool releases the GPU of a slot pod that has ended.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return r.fail(ctx, sb, "SlotEnded", claimedSlotEndMessage(&pod))
	}
	if msg, failed := livenessFailed(&pod); failed {
		_ = r.Delete(ctx, &pod)
		return r.fail(ctx, sb, "LivenessProbeFailed", "liveness probe failed: "+msg)
	}
	// The claimed slot's grant is this sandbox's to converge (the pool's pass
	// leaves claimed slots alone), with this sandbox's Secrets.
	if err := swiftguest.EnsureLauncherIdentity(ctx, r.Client, r.Scheme, &pod, pod.Name,
		swiftguest.SandboxLauncher, slotAccount(&pod), secretNamesFor(slotAccount(&pod), sb, pod.Name)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncWorkloadReady(ctx, sb, &pod); err != nil {
		return ctrl.Result{}, err
	}
	return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxRunning, "running (checked out)")
}

// claimedSlotEndMessage names a claimed slot pod that ended before its
// workload reported, how it ended, and why.
func claimedSlotEndMessage(pod *corev1.Pod) string {
	how := string(pod.Status.Phase)
	if code, ok := launcherExitCode(pod); ok {
		how += fmt.Sprintf(", launcher exit %d", code)
	}
	return fmt.Sprintf("claimed warm slot pod %s ended (%s) before the workload reported an exit: %s",
		pod.Name, how, slotEndMessage(pod))
}

// adoptSlotObjects makes the claiming sandbox the controller of the slot's
// intent ConfigMap and NetworkPolicy (when they exist), as checkout does for
// the slot pod. Idempotent.
func (r *SwiftSandboxReconciler) adoptSlotObjects(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, slot *corev1.Pod) error {
	slotSB := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: slot.Name, Namespace: slot.Namespace}}
	for _, obj := range []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: intentConfigMapName(slotSB), Namespace: slot.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: buildNetworkPolicy(slotSB).Name, Namespace: slot.Namespace}},
	} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if metav1.IsControlledBy(obj, sb) {
			continue
		}
		obj.SetOwnerReferences(nonControllerRefs(obj.GetOwnerReferences()))
		if err := controllerutil.SetControllerReference(sb, obj, r.Scheme); err != nil {
			return err
		}
		if err := r.Update(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}

// checkoutFailure is the reason and message for a checkout swiftletd refused.
// It names a refusal it shares with a cold launch ("KernelUnsupported: ...",
// "SecretUnavailable: ...") by the same reason the cold path uses.
func checkoutFailure(detail string) (reason, msg string) {
	for _, known := range []string{"KernelUnsupported", "SecretUnavailable", "ArtifactMissing"} {
		if rest, ok := strings.CutPrefix(detail, known+": "); ok {
			return known, rest
		}
	}
	return "ExecFailed", "checkout exec failed: " + detail
}

// ownExecAction reports whether the slot carries this sandbox's exec action:
// the first dispatch, or the second after an artifact fetch.
func ownExecAction(slot *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) bool {
	id := slot.Annotations[annSandboxExecActionID]
	return id != "" && (id == string(sb.UID) || id == string(sb.UID)+fetchedActionSuffix)
}

// fetchMissingArtifacts runs the artifact fetch on the slot's node and, when
// it succeeds, dispatches the same exec action again under a new id.
func (r *SwiftSandboxReconciler) fetchMissingArtifacts(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, slot *corev1.Pod, missing []string) (ctrl.Result, error) {
	state, msg, err := r.ensureArtifactFetch(ctx, sb, slot, missing)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch state {
	case fetchFailed:
		_ = r.Delete(ctx, slot)
		return r.fail(ctx, sb, "ArtifactMaterializeFailed", msg)
	case fetchDone:
		patch := client.MergeFrom(slot.DeepCopy())
		slot.Annotations[annSandboxExecActionID] = string(sb.UID) + fetchedActionSuffix
		if err := r.Patch(ctx, slot, patch); err != nil {
			return ctrl.Result{}, err
		}
		return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxRunning, "running (checked out, artifacts fetched)")
	}
	return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxRunning,
		"fetching artifacts "+strings.Join(missing, ", ")+" for warm slot "+slot.Name)
}

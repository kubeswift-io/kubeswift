package swiftsandbox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftgpu"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

const (
	// PoolLabelKey ties a warm slot (its launcher pod / intent ConfigMap /
	// NetworkPolicy) to the owning SwiftSandboxPool.
	PoolLabelKey = "sandbox.kubeswift.io/pool"
	// SlotStateLabelKey marks a slot's checkout state: warm (unclaimed) vs claimed.
	SlotStateLabelKey = "sandbox.kubeswift.io/slot-state"
	slotStateWarm     = "warm"
	slotStateClaimed  = "claimed"

	poolPollInterval = 10 * time.Second
)

// SwiftSandboxPoolReconciler maintains a warm buffer of pre-booted, workload-less
// sandbox slots so a SwiftSandbox can check out sub-second. Co-located with the
// SwiftSandbox controller to reuse its launch builders (buildIntent/buildPod/…) via a
// synthetic per-slot SwiftSandbox carrying the pool's slot shape; the slot boots as a
// bridge-side idle keeper (kubeswift.idle=1), image-independent.
type SwiftSandboxPoolReconciler struct {
	client.Client
	// APIReader is an uncached reader (mgr.GetAPIReader) used to Get the pool's
	// imagePullSecret without opening a cluster-wide secrets informer.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
}

func (r *SwiftSandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool sandboxv1alpha1.SwiftSandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pool.DeletionTimestamp != nil {
		// Warm slots (pods / intent ConfigMaps / NetworkPolicies) are owned by the pool
		// and cascade-GC with it. A GPU pool additionally carries a finalizer so its
		// per-slot SwiftGPU allocations (status fields on separate SwiftGPUNodes, not
		// owner-ref'd) are released before the pool goes.
		if controllerutil.ContainsFinalizer(&pool, poolGPUFinalizer) {
			if err := r.reconcileSlotGPUGC(ctx, &pool, map[string]bool{}); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&pool, poolGPUFinalizer)
			if err := r.Update(ctx, &pool); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	// A GPU pool needs the release finalizer before it warms any GPU slot.
	if pool.Spec.GPUProfileRef != nil && !controllerutil.ContainsFinalizer(&pool, poolGPUFinalizer) {
		controllerutil.AddFinalizer(&pool, poolGPUFinalizer)
		if err := r.Update(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Warm slots run swiftletd, which reports via pod annotations — ensure the
	// per-namespace reporter RoleBinding (idempotent), as the sandbox controller does.
	if err := swiftguest.EnsureSwiftletdRBAC(ctx, r.Client, pool.Namespace); err != nil {
		return ctrl.Result{}, err
	}

	kernelName := defaultKernelProfile
	if pool.Spec.KernelProfileRef != nil && pool.Spec.KernelProfileRef.Name != "" {
		kernelName = pool.Spec.KernelProfileRef.Name
	}

	// Census of the pool's live slots.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(pool.Namespace), client.MatchingLabels{PoolLabelKey: pool.Name}); err != nil {
		return ctrl.Result{}, err
	}
	var ready, warmLive, claimed int
	var warmPods []*corev1.Pod
	// Every pool pod that still EXISTS (any phase, incl. terminating) — used to GC
	// the GPUs of slots whose pods are gone. A terminating pod's CH may still hold
	// its VFIO group, so its allocation is kept until the pod is truly gone.
	liveSlotPods := map[string]bool{}
	for i := range pods.Items {
		liveSlotPods[pods.Items[i].Name] = true
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		// Terminal/terminating slots don't count — owner-GC or the next pass replaces them.
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if p.Labels[SlotStateLabelKey] == slotStateWarm {
			warmLive++
			warmPods = append(warmPods, p)
			if launcherReady(p) {
				ready++
			}
		} else {
			claimed++ // claimed slots (Phase 3) are tracked but not replenished here
		}
	}

	// Release the GPU of any slot whose pod is gone (drain / checkout completion /
	// churn). Runs before warming so freed GPUs are available for new slots.
	if err := r.reconcileSlotGPUGC(ctx, &pool, liveSlotPods); err != nil {
		return ctrl.Result{}, err
	}

	// Resolve the image only when we must — creating slots, or not yet resolved — so a
	// steady Ready pool does no per-reconcile registry calls.
	want := slotsToCreate(int(pool.Spec.MinWarm), int(pool.Spec.MaxWarm), warmLive)
	var ri *resolvedImage
	if want > 0 || pool.Status.Rootfs == nil || pool.Status.Rootfs.Digest == "" {
		auth, err := pullSecretAuth(ctx, r.APIReader, pool.Namespace, pool.Spec.ImagePullSecret, pool.Spec.Image)
		if err != nil {
			return r.degraded(ctx, &pool, ready, claimed, "ImagePullSecretInvalid", err.Error())
		}
		resolved, err := resolveImage(r.slotTemplate(&pool, "resolve"), auth)
		if err != nil {
			return r.degraded(ctx, &pool, ready, claimed, "ImageResolveFailed", err.Error())
		}
		ri = &resolved
	}
	for i := 0; i < want; i++ {
		if err := r.createWarmSlot(ctx, &pool, kernelName, *ri); err != nil {
			// A GPU pool sized above the free-GPU count stops warming here (holds
			// what it could) instead of erroring every reconcile — minWarm > GPUs
			// is an operator sizing choice, surfaced as ready < minWarm.
			if pool.Spec.GPUProfileRef != nil && errors.Is(err, swiftgpu.ErrNoCapacity) {
				break
			}
			return ctrl.Result{}, err
		}
	}
	// Drain excess warm slots when the desired count dropped below the live count
	// (a `kubectl scale`/HPA scale-down). want>0 and this are mutually exclusive.
	for _, p := range pickWarmToDrain(warmPods, slotsToDelete(int(pool.Spec.MinWarm), warmLive)) {
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	// (scale-to-zero on a quiet pool is done via the scale subresource + an HPA on
	// minWarm, not a pool-side idle timer; node-spread is applied per slot in
	// createWarmSlot via warmSlotTopologySpread.)

	return r.updateStatus(ctx, &pool, ready, claimed, ri)
}

// slotsToCreate is the pure warm-slot arithmetic: how many new slots to bring up this
// pass to reach minWarm, capped by the effective max. spec.maxWarm=0 means "no cap
// beyond minWarm", and a max set below minWarm is a misconfiguration where minWarm
// wins — both fold into effMax = max(maxWarm, minWarm). (Sane min/max bounds are the
// webhook's job; the operator owns the number, like SwiftGuestPool replicas.)
func slotsToCreate(minWarm, maxWarm, warmLive int) int {
	effMax := maxWarm
	if effMax < minWarm {
		effMax = minWarm
	}
	want := minWarm - warmLive
	if room := effMax - warmLive; want > room {
		want = room
	}
	if want < 0 {
		want = 0
	}
	return want
}

// slotsToDelete is the drain-side arithmetic: how many warm slots to remove this
// pass because the live count exceeds the desired minWarm — e.g. after
// `kubectl scale sboxpool --replicas=N` or an HPA lowered it. Without this the
// scale subresource could only ratchet the buffer up. Claimed slots are never
// counted here (they belong to a SwiftSandbox).
func slotsToDelete(minWarm, warmLive int) int {
	if warmLive > minWarm {
		return warmLive - minWarm
	}
	return 0
}

// pickWarmToDrain selects up to n warm slot pods to delete, preferring
// not-launcher-ready (still-warming) slots so a ready, checkout-serviceable slot
// is drained last.
func pickWarmToDrain(warm []*corev1.Pod, n int) []*corev1.Pod {
	if n >= len(warm) {
		return warm
	}
	sort.SliceStable(warm, func(i, j int) bool {
		return !launcherReady(warm[i]) && launcherReady(warm[j])
	})
	return warm[:n]
}

// warmSlotTopologySpread spreads a pool's warm slots one-per-node across kernel-nodes
// (MaxSkew 1 over the hostname), soft so it never blocks warming.
func warmSlotTopologySpread(poolName string) []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "kubernetes.io/hostname",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{PoolLabelKey: poolName}},
	}}
}

// slotTemplate builds the synthetic (unpersisted) SwiftSandbox for a warm slot: the
// pool's slot shape, with NO workload command — the slot boots as a bridge-side idle
// keeper (kubeswift.idle=1) and a checkout injects the workload later over vsock. The
// launch builders read only its fields; the resulting pod/ConfigMap/NetworkPolicy are
// owned by the POOL.
func (r *SwiftSandboxPoolReconciler) slotTemplate(pool *sandboxv1alpha1.SwiftSandboxPool, name string) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pool.Namespace},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image:              pool.Spec.Image,
			ImagePullSecret:    pool.Spec.ImagePullSecret,
			VerifyKeySecretRef: pool.Spec.VerifyKeySecretRef,
			RootfsMode:         pool.Spec.RootfsMode,
			CPU:                pool.Spec.CPU,
			Memory:             pool.Spec.Memory,
			Network:            pool.Spec.Network,
			KernelProfileRef:   pool.Spec.KernelProfileRef,
			NodeSelector:       pool.Spec.NodeSelector,
		},
	}
}

// createWarmSlot brings up one warm slot: the intent ConfigMap + launcher pod (+ a
// deny-ingress NetworkPolicy when networked), all owned by the pool and labeled warm.
func (r *SwiftSandboxPoolReconciler) createWarmSlot(ctx context.Context, pool *sandboxv1alpha1.SwiftSandboxPool, kernelName string, ri resolvedImage) error {
	slot := r.slotTemplate(pool, pool.Name+"-slot-"+utilrand.String(5))

	// Warm GPU pool: allocate a GPU for this slot and stamp its spec.gpuProfileRef
	// + status.GPU so the launch builders produce a GPU-aware slot (node pin,
	// gpu-init, explicit device intent — B1 machinery). An ErrNoCapacity here is
	// propagated so the reconcile stops warming rather than looping.
	if pool.Spec.GPUProfileRef != nil {
		if err := r.allocateSlotGPU(ctx, pool, slot); err != nil {
			return err
		}
	}

	// idle=true: a warm keeper carries no workload (execSpec{}); it boots to the
	// bridge idle loop (kubeswift.idle=1) so warming never depends on the image
	// having a sleep binary, and a distroless image can be pooled. ri.Exec.Env is
	// still stamped into the pool status for the checkout env-merge (see updateStatus).
	intent := buildIntent(slot, kernelName, ri.RootfsPath, execSpec{}, true)
	intentJSON, err := runtimeintent.Serialize(intent)
	if err != nil {
		return err
	}
	cm := buildIntentConfigMap(slot, intentJSON)
	cm.Labels = map[string]string{PoolLabelKey: pool.Name}
	if err := controllerutil.SetControllerReference(pool, cm, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	pod := buildPod(slot, kernelName)
	pod.Labels[PoolLabelKey] = pool.Name
	pod.Labels[SlotStateLabelKey] = slotStateWarm
	// Spread the pool's slots across kernel-nodes so a checkout landing on any node is
	// likely to find a warm slot there (warming is node-local). Soft (ScheduleAnyway):
	// never block warming just because one node is full.
	pod.Spec.TopologySpreadConstraints = warmSlotTopologySpread(pool.Name)
	if err := controllerutil.SetControllerReference(pool, pod, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	if networked(slot) {
		np := buildNetworkPolicy(slot)
		if np.Labels == nil {
			np.Labels = map[string]string{}
		}
		np.Labels[PoolLabelKey] = pool.Name
		if err := controllerutil.SetControllerReference(pool, np, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, np); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func (r *SwiftSandboxPoolReconciler) updateStatus(ctx context.Context, pool *sandboxv1alpha1.SwiftSandboxPool, ready, claimed int, ri *resolvedImage) (ctrl.Result, error) {
	pool.Status.WarmReplicas = int32(ready)
	pool.Status.ClaimedReplicas = int32(claimed)
	pool.Status.ObservedGeneration = pool.Generation
	// The scale subresource selectorpath — the warm-slot label selector, so
	// `kubectl scale sboxpool` and an HPA can target the pool's warm buffer.
	pool.Status.Selector = PoolLabelKey + "=" + pool.Name
	if ri != nil {
		pool.Status.Rootfs = &sandboxv1alpha1.SandboxRootfsStatus{Digest: ri.Digest, CachePath: ri.RootfsPath}
		// The slot template carries no workload env, so ri.Exec.Env is the image's
		// own config env — stamp it so a checkout can merge without re-pulling.
		pool.Status.ImageEnv = ri.Exec.Env
		apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type: sandboxv1alpha1.SwiftSandboxPoolConditionResolved, Status: metav1.ConditionTrue,
			Reason: "Resolved", Message: "image digest " + ri.Digest, ObservedGeneration: pool.Generation,
		})
	}
	msg := fmt.Sprintf("%d/%d warm slots ready", ready, pool.Spec.MinWarm)
	phase := sandboxv1alpha1.SwiftSandboxPoolWarming
	warm := metav1.ConditionFalse
	if int32(ready) >= pool.Spec.MinWarm {
		phase, warm = sandboxv1alpha1.SwiftSandboxPoolReady, metav1.ConditionTrue
	}
	pool.Status.Phase = phase
	pool.Status.Message = msg
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxPoolConditionWarm, Status: warm,
		Reason: "Warm", Message: msg, ObservedGeneration: pool.Generation,
	})
	if err := r.Status().Update(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: poolPollInterval}, nil
}

// degraded surfaces a resolve/warm failure honestly (Resolved=False, phase=Degraded)
// rather than stalling silently.
func (r *SwiftSandboxPoolReconciler) degraded(ctx context.Context, pool *sandboxv1alpha1.SwiftSandboxPool, ready, claimed int, reason, msg string) (ctrl.Result, error) {
	pool.Status.WarmReplicas = int32(ready)
	pool.Status.ClaimedReplicas = int32(claimed)
	pool.Status.Selector = PoolLabelKey + "=" + pool.Name
	pool.Status.Phase = sandboxv1alpha1.SwiftSandboxPoolDegraded
	pool.Status.Message = msg
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxPoolConditionResolved, Status: metav1.ConditionFalse,
		Reason: reason, Message: msg, ObservedGeneration: pool.Generation,
	})
	if err := r.Status().Update(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: poolPollInterval}, nil
}

func (r *SwiftSandboxPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.SwiftSandboxPool{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}

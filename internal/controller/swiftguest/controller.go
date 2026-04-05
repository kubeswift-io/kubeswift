package swiftguest

import (
	"context"
	"errors"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/metrics"
	"github.com/projectbeskar/kubeswift/internal/resolved"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
	"github.com/projectbeskar/kubeswift/internal/seed"
)

// gpuHypervisorAnnotation overrides hypervisor selection for manual QEMU testing
// without GPU hardware. The annotation value is used as-is ("qemu" or "cloud-hypervisor").
// Only read by the controller; never written.
const gpuHypervisorAnnotation = "kubeswift.io/hypervisor-override"

const (
	SeedConfigMapSuffix = "-seed"

	// defaultNetworkConfig is used when SwiftSeedProfile has no networkData.
	// Matches both predictable naming (en*: ens3, enp0s3, eno1) and legacy (eth*: eth0).
	// Ubuntu/Debian use en*; Rocky/RHEL may use eth0 when net.ifnames=0.
	// Top-level "network:" is required by cloud-init/netplan.
	defaultNetworkConfig = `network:
  version: 2
  ethernets:
    predictable:
      match:
        name: en*
      dhcp4: true
    legacy:
      match:
        name: eth*
      dhcp4: true
`
)

// SwiftGuestReconciler reconciles SwiftGuest resources.
type SwiftGuestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile implements the reconcile loop.
func (r *SwiftGuestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, req.NamespacedName, &guest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	res := resolved.NewResolver(r.Client)
	rg, err := res.Resolve(ctx, &guest)
	if err != nil {
		var re *resolved.ResolutionError
		if errors.As(err, &re) {
			// Set Resolved=False, phase=Failed; do not create pod
			status := guest.Status.DeepCopy()
			SetResolvedCondition(status, false, re.Reason)
			status.Phase = swiftv1alpha1.SwiftGuestPhaseFailed
			recordGuestMetrics(&guest, &guest.Status, status, nil)
			if err := r.patchStatus(ctx, &guest, status); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("resolution failed", "reason", re.Reason, "resource", re.AffectedResource)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Set Resolved=True
	status := guest.Status.DeepCopy()
	SetResolvedCondition(status, true, "")

	// Seed rendering: when ResolvedGuest has Seed, render and create ConfigMap
	seedConfigMapName := ""
	if rg.HasSeed() {
		userData, metaData, networkData, err := seed.Render(ctx, r.Client, guest.Namespace, rg.Seed)
		if err != nil {
			logger.Error(err, "seed render failed")
			return ctrl.Result{}, err
		}
		if networkData == "" {
			networkData = defaultNetworkConfig
		}
		seedConfigMapName = guest.Name + SeedConfigMapSuffix
		desiredCM := seed.BuildConfigMap(seedConfigMapName, guest.Namespace, userData, metaData, networkData)
		if err := controllerutil.SetControllerReference(&guest, desiredCM, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		var existingCM corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: seedConfigMapName}, &existingCM); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			if err := r.Create(ctx, desiredCM); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			if !equality.Semantic.DeepEqual(existingCM.Data, desiredCM.Data) ||
				!equality.Semantic.DeepEqual(existingCM.OwnerReferences, desiredCM.OwnerReferences) {
				existingCM.Data = desiredCM.Data
				existingCM.OwnerReferences = desiredCM.OwnerReferences
				if err := r.Update(ctx, &existingCM); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}

	// Set hypervisor from GPU allocation status when gpuProfileRef is set.
	// The annotation override (kubeswift.io/hypervisor-override) takes precedence
	// for manual QEMU testing without real GPU hardware.
	if guest.Spec.GPUProfileRef != nil && guest.Status.GPU != nil {
		rg.Hypervisor = guest.Status.GPU.Hypervisor
	}
	if override, ok := guest.Annotations[gpuHypervisorAnnotation]; ok && override != "" {
		rg.Hypervisor = override
	}

	// Create or update intent ConfigMap
	intentConfigMapName := guest.Name + IntentConfigMapSuffix
	intent := runtimeintent.Build(rg)

	// Attach GPU intent when GPUs have been allocated (Phase 3+).
	if guest.Spec.GPUProfileRef != nil && isGPUAllocated(&guest) {
		gpuIntent, err := r.buildGPUIntent(ctx, &guest)
		if err != nil {
			logger.Error(err, "failed to build GPU intent")
			return ctrl.Result{}, err
		}
		intent.GPU = gpuIntent
	}
	intentJSON, err := runtimeintent.Serialize(intent)
	if err != nil {
		logger.Error(err, "failed to serialize runtime intent")
		return ctrl.Result{}, err
	}
	desiredIntentCM := &corev1.ConfigMap{
		ObjectMeta: ctrl.ObjectMeta{
			Name:      intentConfigMapName,
			Namespace: guest.Namespace,
		},
		Data: map[string]string{
			IntentFile: string(intentJSON),
		},
	}
	if err := controllerutil.SetControllerReference(&guest, desiredIntentCM, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	var existingIntentCM corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: intentConfigMapName}, &existingIntentCM); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desiredIntentCM); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		if !equality.Semantic.DeepEqual(existingIntentCM.Data, desiredIntentCM.Data) {
			existingIntentCM.Data = desiredIntentCM.Data
			if err := r.Update(ctx, &existingIntentCM); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if rg.GetLifecycle() == "stop" {
		// Guest is intentionally stopped. If a pod exists and is completed or doesn't exist,
		// update status to Stopped and return without recreating the pod.
		var existingPod corev1.Pod
		podErr := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: guest.Name}, &existingPod)
		if podErr != nil && client.IgnoreNotFound(podErr) != nil {
			return ctrl.Result{}, podErr
		}
		podGone := apierrors.IsNotFound(podErr)
		podDone := !podGone && (existingPod.Status.Phase == corev1.PodSucceeded || existingPod.Status.Phase == corev1.PodFailed)
		if podGone || podDone {
			status.Phase = swiftv1alpha1.SwiftGuestPhaseStopped
			var podForStopped *corev1.Pod
			if !podGone {
				podForStopped = &existingPod
			}
			recordGuestMetrics(&guest, &guest.Status, status, podForStopped)
			if err := r.patchStatus(ctx, &guest, status); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	if guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyRestartOnFailure ||
		guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyAlways {

		var existingPod corev1.Pod
		podErr := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: guest.Name}, &existingPod)
		if podErr != nil && !apierrors.IsNotFound(podErr) {
			return ctrl.Result{}, podErr
		}
		podGone := apierrors.IsNotFound(podErr)

		if !podGone {
			shouldRestart := false
			if existingPod.Status.Phase == corev1.PodFailed {
				shouldRestart = true
			}
			if guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyAlways &&
				existingPod.Status.Phase == corev1.PodSucceeded {
				shouldRestart = true
			}

			if shouldRestart {
				// Compute backoff: 10s * 2^restartCount, capped at 300s
				backoff := time.Duration(10) * time.Second
				for i := 0; i < int(guest.Status.RestartCount); i++ {
					backoff *= 2
					if backoff > 300*time.Second {
						backoff = 300 * time.Second
						break
					}
				}

				// Check if enough time has passed since last restart
				if guest.Status.LastRestartTime != nil {
					elapsed := time.Since(guest.Status.LastRestartTime.Time)
					if elapsed < backoff {
						remaining := backoff - elapsed
						if err := r.patchStatus(ctx, &guest, status); err != nil {
							return ctrl.Result{}, err
						}
						return ctrl.Result{RequeueAfter: remaining}, nil
					}
				}

				// Delete the pod so controller recreates it on next reconcile
				if err := r.Delete(ctx, &existingPod); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}

				// Update restart tracking
				now := metav1.Now()
				status.RestartCount++
				status.LastRestartTime = &now
				status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
				recordGuestMetrics(&guest, &guest.Status, status, &existingPod)
				if err := r.patchStatus(ctx, &guest, status); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		}
	}

	// GPU gate: if gpuProfileRef is set, wait for GPUAllocated=True before creating
	// the launcher pod. The SwiftGPU controller runs independently and sets this
	// condition once devices are reserved on a SwiftGPUNode.
	if guest.Spec.GPUProfileRef != nil && !isGPUAllocated(&guest) {
		logger.Info("waiting for GPU allocation", "guest", req.NamespacedName)
		status.Phase = swiftv1alpha1.SwiftGuestPhasePending
		if err := r.patchStatus(ctx, &guest, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// For disk boot, ensure per-guest root disk clone exists and is ready.
	if rg.PreparedImage.PVCName != "" && !rg.HasKernel() {
		clonePVC, err := r.EnsureRootDiskClone(ctx, &guest, rg)
		if err != nil {
			// Clone not ready — requeue
			status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
			if patchErr := r.patchStatus(ctx, &guest, status); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// Override the PVC name to use the clone instead of the shared image PVC.
		rg.PreparedImage.PVCName = clonePVC
	}

	// Build and create/update pod
	desiredPod, err := r.buildPod(ctx, &guest, rg, seedConfigMapName, intentConfigMapName)
	if err != nil {
		logger.Error(err, "failed to build pod spec")
		return ctrl.Result{}, err
	}
	if err := controllerutil.SetControllerReference(&guest, desiredPod, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	var existingPod corev1.Pod
	var podForMetrics *corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: guest.Name}, &existingPod); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desiredPod); err != nil {
			return ctrl.Result{}, err
		}
		// Pod just created; set initial status (Pending/Scheduling)
		status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
		SetPodScheduledCondition(status, nil, false, "Scheduling")
	} else {
		// Pod exists; update status from pod
		podForMetrics = &existingPod
		MapPodToStatus(&existingPod, status)
		// If guest is running but IP not yet discovered, requeue to catch annotation update
		if status.Phase == swiftv1alpha1.SwiftGuestPhaseRunning &&
			(status.Network == nil || status.Network.PrimaryIP == "") {
			recordGuestMetrics(&guest, &guest.Status, status, podForMetrics)
			if err := r.patchStatus(ctx, &guest, status); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// TODO: consider updating pod spec if resolved changed (e.g., resources)
	}

	recordGuestMetrics(&guest, &guest.Status, status, podForMetrics)
	if err := r.patchStatus(ctx, &guest, status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func recordGuestMetrics(guest *swiftv1alpha1.SwiftGuest, oldStatus, newStatus *swiftv1alpha1.SwiftGuestStatus, pod *corev1.Pod) {
	ns := guest.Namespace
	oldPhase := swiftv1alpha1.SwiftGuestPhase("")
	if oldStatus != nil {
		oldPhase = oldStatus.Phase
	}
	newPhase := swiftv1alpha1.SwiftGuestPhase("")
	if newStatus != nil {
		newPhase = newStatus.Phase
	}

	// GuestRunningTotal: increment on transition to Running, decrement on transition away
	if newPhase == swiftv1alpha1.SwiftGuestPhaseRunning && oldPhase != swiftv1alpha1.SwiftGuestPhaseRunning {
		metrics.GuestRunningTotal.WithLabelValues(ns).Inc()
	}
	if oldPhase == swiftv1alpha1.SwiftGuestPhaseRunning && newPhase != swiftv1alpha1.SwiftGuestPhaseRunning {
		metrics.GuestRunningTotal.WithLabelValues(ns).Dec()
		metrics.UnmarkVMBootObserved(ns + "/" + guest.Name)
	}

	// VMBootSeconds: observe when GuestRunning becomes True (once per boot)
	if newPhase == swiftv1alpha1.SwiftGuestPhaseRunning && pod != nil {
		guestRunning := findCondition(newStatus, "GuestRunning")
		if guestRunning != nil && guestRunning.Status == metav1.ConditionTrue {
			key := guest.Namespace + "/" + guest.Name
			if metrics.MarkVMBootObserved(key) {
				elapsed := time.Since(pod.CreationTimestamp.Time).Seconds()
				metrics.VMBootSeconds.WithLabelValues(ns).Observe(elapsed)
			}
		}
	}

	// VMFailuresTotal: increment on transition to Failed
	if newPhase == swiftv1alpha1.SwiftGuestPhaseFailed && oldPhase != swiftv1alpha1.SwiftGuestPhaseFailed {
		reason := "unknown"
		if c := findCondition(newStatus, "PodScheduled"); c != nil && c.Message != "" {
			reason = c.Message
		} else if c := findCondition(newStatus, "GuestRunning"); c != nil && c.Message != "" {
			reason = c.Message
		} else if c := findCondition(newStatus, "Resolved"); c != nil && c.Message != "" {
			reason = c.Message
		}
		metrics.VMFailuresTotal.WithLabelValues(ns, reason).Inc()
	}
}

func findCondition(status *swiftv1alpha1.SwiftGuestStatus, condType string) *metav1.Condition {
	if status == nil {
		return nil
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			return &status.Conditions[i]
		}
	}
	return nil
}

func (r *SwiftGuestReconciler) patchStatus(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, status *swiftv1alpha1.SwiftGuestStatus) error {
	if equality.Semantic.DeepEqual(guest.Status, status) {
		return nil
	}
	patch := client.MergeFrom(guest.DeepCopy())
	guest.Status = *status
	return r.Status().Patch(ctx, guest, patch)
}

// swiftImageToSwiftGuests enqueues SwiftGuests that reference a SwiftImage when the SwiftImage changes.
func (r *SwiftGuestReconciler) swiftImageToSwiftGuests(ctx context.Context, obj client.Object) []reconcile.Request {
	var img imagev1alpha1.SwiftImage
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), &img); err != nil {
		return nil
	}
	var list swiftv1alpha1.SwiftGuestList
	if err := r.List(ctx, &list, client.InNamespace(img.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		g := &list.Items[i]
		if g.Spec.ImageRef != nil && g.Spec.ImageRef.Name == img.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(g)})
		}
	}
	return reqs
}

// SetupWithManager registers the reconciler with the manager.
func (r *SwiftGuestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&swiftv1alpha1.SwiftGuest{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Watches(&imagev1alpha1.SwiftImage{}, handler.EnqueueRequestsFromMapFunc(r.swiftImageToSwiftGuests)).
		Complete(r)
}

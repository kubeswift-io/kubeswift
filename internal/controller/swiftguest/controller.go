package swiftguest

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
	"github.com/projectbeskar/kubeswift/internal/seed"
)

const (
	SeedConfigMapSuffix = "-seed"

	// defaultNetworkConfig is used when SwiftSeedProfile has no networkData.
	// Matches both predictable naming (en*: ens3, enp0s3, eno1) and legacy (eth*: eth0).
	// Ubuntu/Debian use en*; Rocky/RHEL may use eth0 when net.ifnames=0.
	defaultNetworkConfig = `version: 2
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

	// Create or update intent ConfigMap
	intentConfigMapName := guest.Name + IntentConfigMapSuffix
	intent := runtimeintent.Build(rg)
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

	// Build and create/update pod
	desiredPod := BuildPod(&guest, rg, seedConfigMapName, intentConfigMapName)
	if err := controllerutil.SetControllerReference(&guest, desiredPod, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	var existingPod corev1.Pod
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
		MapPodToStatus(&existingPod, status)
		// TODO: consider updating pod spec if resolved changed (e.g., resources)
	}

	if err := r.patchStatus(ctx, &guest, status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
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
		if g.Spec.ImageRef.Name == img.Name {
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
		Watches(&imagev1alpha1.SwiftImage{}, handler.EnqueueRequestsFromMapFunc(r.swiftImageToSwiftGuests)).
		Complete(r)
}

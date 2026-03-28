package swiftkernel

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kernelv1alpha1 "github.com/projectbeskar/kubeswift/api/kernel/v1alpha1"
)

// SwiftKernelReconciler reconciles SwiftKernel resources.
type SwiftKernelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile implements the reconcile loop.
func (r *SwiftKernelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sk kernelv1alpha1.SwiftKernel
	if err := r.Get(ctx, req.NamespacedName, &sk); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if sk.Status.Phase == kernelv1alpha1.SwiftKernelPhaseReady {
		return ctrl.Result{}, nil
	}

	phase := sk.Status.Phase
	if phase == "" {
		phase = kernelv1alpha1.SwiftKernelPhasePending
	}

	status := sk.Status.DeepCopy()

	switch phase {
	case kernelv1alpha1.SwiftKernelPhasePending:
		if err := r.StartPull(ctx, &sk); err != nil {
			logger.Error(err, "pull start failed")
			return ctrl.Result{}, err
		}
		SetPhase(status, kernelv1alpha1.SwiftKernelPhasePulling)

	case kernelv1alpha1.SwiftKernelPhasePulling:
		done, localPath, errMsg, err := r.CheckPullStatus(ctx, &sk)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{}, nil
		}
		if errMsg != "" {
			SetPhase(status, kernelv1alpha1.SwiftKernelPhaseFailed)
			SetFailedCondition(status, ReasonPullFailed, errMsg)
		} else {
			SetPhase(status, kernelv1alpha1.SwiftKernelPhaseReady)
			status.LocalPath = localPath
			SetReadyCondition(status)
		}

	case kernelv1alpha1.SwiftKernelPhaseReady, kernelv1alpha1.SwiftKernelPhaseFailed:
		return ctrl.Result{}, nil
	}

	sk.Status = *status
	if err := r.Status().Update(ctx, &sk); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *SwiftKernelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kernelv1alpha1.SwiftKernel{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

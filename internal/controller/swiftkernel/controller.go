package swiftkernel

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
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

	if sk.Status.Phase == kernelv1alpha1.SwiftKernelPhaseFailed {
		return ctrl.Result{}, nil
	}

	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList, client.MatchingLabels{"kubeswift.io/kernel-node": "true"}); err != nil {
		return ctrl.Result{}, err
	}

	status := sk.Status.DeepCopy()

	if len(nodeList.Items) == 0 {
		SetPhase(status, kernelv1alpha1.SwiftKernelPhasePending)
		SetNoKernelNodesCondition(status)
		status.NodeStatuses = nil
		sk.Status = *status
		if err := r.Status().Update(ctx, &sk); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("no kernel-capable nodes found, waiting for labeled nodes")
		return ctrl.Result{}, nil
	}

	nodeStatuses := make([]kernelv1alpha1.NodeKernelStatus, 0, len(nodeList.Items))
	var failedNode, failedMsg string
	anyFailed := false
	allReady := true

	for _, node := range nodeList.Items {
		nodeName := node.Name
		nodePhase, errMsg, err := r.CheckNodePullStatus(ctx, &sk, nodeName)
		if err != nil {
			return ctrl.Result{}, err
		}

		switch nodePhase {
		case kernelv1alpha1.SwiftKernelPhasePending:
			if err := r.StartPullOnNode(ctx, &sk, nodeName); err != nil {
				logger.Error(err, "pull start failed", "node", nodeName)
				return ctrl.Result{}, err
			}
			nodeStatuses = append(nodeStatuses, kernelv1alpha1.NodeKernelStatus{NodeName: nodeName, Phase: kernelv1alpha1.SwiftKernelPhasePulling})
			allReady = false
		case kernelv1alpha1.SwiftKernelPhasePulling:
			nodeStatuses = append(nodeStatuses, kernelv1alpha1.NodeKernelStatus{NodeName: nodeName, Phase: kernelv1alpha1.SwiftKernelPhasePulling})
			allReady = false
		case kernelv1alpha1.SwiftKernelPhaseReady:
			nodeStatuses = append(nodeStatuses, kernelv1alpha1.NodeKernelStatus{NodeName: nodeName, Phase: kernelv1alpha1.SwiftKernelPhaseReady})
		case kernelv1alpha1.SwiftKernelPhaseFailed:
			nodeStatuses = append(nodeStatuses, kernelv1alpha1.NodeKernelStatus{NodeName: nodeName, Phase: kernelv1alpha1.SwiftKernelPhaseFailed})
			if !anyFailed {
				failedNode = nodeName
				failedMsg = errMsg
			}
			anyFailed = true
			allReady = false
		}
	}

	if anyFailed {
		SetPhase(status, kernelv1alpha1.SwiftKernelPhaseFailed)
		SetFailedCondition(status, ReasonPullFailed, failedMsg, failedNode)
	} else if allReady {
		SetPhase(status, kernelv1alpha1.SwiftKernelPhaseReady)
		SetReadyCondition(status)
	} else {
		SetPhase(status, kernelv1alpha1.SwiftKernelPhasePulling)
		SetNodesPullingCondition(status)
	}

	status.NodeStatuses = nodeStatuses
	sk.Status = *status
	if err := r.Status().Update(ctx, &sk); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *SwiftKernelReconciler) mapNodeToSwiftKernels(ctx context.Context, obj client.Object) []reconcile.Request {
	var list kernelv1alpha1.SwiftKernelList
	if err := r.List(ctx, &list, client.InNamespace(metav1.NamespaceAll)); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			},
		})
	}
	return reqs
}

func (r *SwiftKernelReconciler) nodeKernelLabelPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetLabels()["kubeswift.io/kernel-node"] == "true"
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldVal := ""
			newVal := ""
			if old := e.ObjectOld.GetLabels(); old != nil {
				oldVal = old["kubeswift.io/kernel-node"]
			}
			if new := e.ObjectNew.GetLabels(); new != nil {
				newVal = new["kubeswift.io/kernel-node"]
			}
			return oldVal != newVal
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
}

// SetupWithManager registers the reconciler with the manager.
func (r *SwiftKernelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kernelv1alpha1.SwiftKernel{}).
		Owns(&batchv1.Job{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.mapNodeToSwiftKernels),
			builder.WithPredicates(r.nodeKernelLabelPredicate()),
		).
		Complete(r)
}

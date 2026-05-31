package migrationcert

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Node role labels Kubernetes stamps on control-plane members. A node
// carrying either is excluded from per-node migration certificates:
// disk-boot guests (the live-migration-capable workload class) schedule
// onto workers, and control-plane nodes do not run launcher pods.
const (
	controlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"
	masterRoleLabel       = "node-role.kubernetes.io/master"
)

// MigrationCertReconciler ensures one cert-manager Certificate per worker
// node for the Phase 3c live-migration mTLS transport (Option B per-node
// identity). It watches Nodes directly: a worker joining triggers
// Certificate creation; a worker leaving (or being relabeled
// control-plane) triggers cleanup.
//
// This reconciler is only constructed when --migration-mtls-enabled=true.
// It requires cert-manager installed cluster-side. With the flag off it
// is never registered, so default clusters incur no cert-manager
// dependency and no behavior change.
type MigrationCertReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// SystemNamespace is where per-node Certificates (and their
	// cert-manager-written Secrets) live — the controller's own
	// namespace. Sourced from POD_NAMESPACE (same value as leader
	// election). Per-node Certs live here, NOT in guest namespaces;
	// secret.go distributes the issued Secret to guest namespaces at
	// migration time.
	SystemNamespace string
}

// Reconcile ensures the per-node Certificate matches the node's existence
// and role.
func (r *MigrationCertReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	nodeName := req.Name

	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		if apierrors.IsNotFound(err) {
			// Node deleted: garbage-collect its per-node Certificate.
			// The cert name derives from the node name, which survives
			// in req even though the Node object is gone.
			if derr := deleteNodeCertificate(ctx, r.Client, r.SystemNamespace, nodeName); derr != nil {
				return ctrl.Result{}, derr
			}
			logger.Info("node deleted, removed migration certificate", "node", nodeName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !isWorkerNode(&node) {
		// Control-plane node: ensure no stale per-node cert lingers
		// (e.g. a node that was a worker first and got relabeled).
		if err := deleteNodeCertificate(ctx, r.Client, r.SystemNamespace, nodeName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := ensureNodeCertificate(ctx, r.Client, r.SystemNamespace, nodeName); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("ensured migration certificate", "node", nodeName)
	return ctrl.Result{}, nil
}

// isWorkerNode reports whether a node should hold a per-node migration
// certificate. Any control-plane/master role label excludes it.
func isWorkerNode(node *corev1.Node) bool {
	labels := node.GetLabels()
	if _, ok := labels[controlPlaneRoleLabel]; ok {
		return false
	}
	if _, ok := labels[masterRoleLabel]; ok {
		return false
	}
	return true
}

// workerNodePredicate keeps the reconciler off the node-heartbeat churn.
// Create/Delete always enqueue (a node appearing or disappearing changes
// the desired cert set). Update enqueues only when worker/control-plane
// membership flips — the only Node mutation that changes desired state.
func (r *MigrationCertReconciler) workerNodePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return false
			}
			return isWorkerNode(oldNode) != isWorkerNode(newNode)
		},
	}
}

// SetupWithManager registers the reconciler. The Node IS the reconcile
// target (For), so no map function is needed — unlike SwiftKernel, which
// watches Nodes only to re-enqueue SwiftKernel CRs.
func (r *MigrationCertReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}, builder.WithPredicates(r.workerNodePredicate())).
		Named("migrationcert").
		Complete(r)
}

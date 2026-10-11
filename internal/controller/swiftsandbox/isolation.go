package swiftsandbox

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// launcherIsolationFinalizer keeps a sandbox launcher's NetworkPolicy until no
// pod it selects can still run. The policy and the launcher pod are both
// dependents of the sandbox (or of the pool, for a warm slot), so garbage
// collection, a namespace deletion or a plain delete removed the policy at
// once while the launcher, and the workload in its microVM, ran on for the
// pod's grace period with no ingress isolation.
//
// The safe point is the pod, not the sandbox: a pod object is removed only
// after the kubelet has stopped its containers, and a pod in phase Succeeded
// or Failed runs nothing. Force-deleting a pod skips that guarantee; that is
// the operator's explicit choice.
const launcherIsolationFinalizer = "kubeswift.io/launcher-isolation"

// isolationRecheck re-reads the selected pods of a policy held by its
// finalizer, in case a pod event was missed.
const isolationRecheck = 10 * time.Second

// IsolationReconciler holds each sandbox launcher NetworkPolicy until the pods
// it protects are gone. It also adds the finalizer to policies created before
// it existed.
type IsolationReconciler struct {
	client.Client
	// APIReader reads the selected pods uncached before a policy is let go: a
	// pod the cache has not seen yet must not count as absent.
	APIReader client.Reader
}

// isLauncherPolicy is whether np is a sandbox launcher's NetworkPolicy: owned
// by a SwiftSandbox or a SwiftSandboxPool, or carrying the finalizer (after
// an orphaning delete stripped its owners).
func isLauncherPolicy(np *networkingv1.NetworkPolicy) bool {
	if controllerutil.ContainsFinalizer(np, launcherIsolationFinalizer) {
		return true
	}
	for _, ref := range np.OwnerReferences {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != sandboxv1alpha1.GroupName {
			continue
		}
		if ref.Kind == "SwiftSandbox" || ref.Kind == "SwiftSandboxPool" {
			return true
		}
	}
	return false
}

func (r *IsolationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var np networkingv1.NetworkPolicy
	if err := r.Get(ctx, req.NamespacedName, &np); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !isLauncherPolicy(&np) {
		return ctrl.Result{}, nil
	}
	if np.DeletionTimestamp == nil {
		if controllerutil.ContainsFinalizer(&np, launcherIsolationFinalizer) {
			return ctrl.Result{}, nil
		}
		controllerutil.AddFinalizer(&np, launcherIsolationFinalizer)
		return ctrl.Result{}, r.Update(ctx, &np)
	}
	if !controllerutil.ContainsFinalizer(&np, launcherIsolationFinalizer) {
		return ctrl.Result{}, nil
	}
	running, err := r.protectedPods(ctx, &np)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(running) > 0 {
		klog.FromContext(ctx).V(1).Info("launcher NetworkPolicy held: a pod it protects can still run",
			"networkPolicy", np.Name, "pods", running)
		return ctrl.Result{RequeueAfter: isolationRecheck}, nil
	}
	controllerutil.RemoveFinalizer(&np, launcherIsolationFinalizer)
	if err := r.Update(ctx, &np); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	klog.FromContext(ctx).Info("launcher NetworkPolicy released: no pod it protects can run", "networkPolicy", np.Name)
	return ctrl.Result{}, nil
}

// protectedPods lists the pods np selects that may still run: present and
// not Succeeded or Failed.
func (r *IsolationReconciler) protectedPods(ctx context.Context, np *networkingv1.NetworkPolicy) ([]string, error) {
	sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
	if err != nil {
		return nil, fmt.Errorf("NetworkPolicy %s/%s: pod selector: %w", np.Namespace, np.Name, err)
	}
	var pods corev1.PodList
	if err := r.APIReader.List(ctx, &pods, client.InNamespace(np.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	var running []string
	for i := range pods.Items {
		if !podFinished(&pods.Items[i]) {
			running = append(running, pods.Items[i].Name)
		}
	}
	return running, nil
}

// policiesForPod maps a launcher pod event to the held policies that select it.
func (r *IsolationReconciler) policiesForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	var nps networkingv1.NetworkPolicyList
	if err := r.List(ctx, &nps, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range nps.Items {
		np := &nps.Items[i]
		if np.DeletionTimestamp == nil || !controllerutil.ContainsFinalizer(np, launcherIsolationFinalizer) {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
		if err != nil || !sel.Matches(labels.Set(obj.GetLabels())) {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(np)})
	}
	return out
}

func (r *IsolationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	launcherPod := predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetLabels()[SandboxLabelKey]
		return ok
	})
	launcherPolicy := predicate.NewPredicateFuncs(func(o client.Object) bool {
		np, ok := o.(*networkingv1.NetworkPolicy)
		return ok && isLauncherPolicy(np)
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("sandbox-isolation").
		For(&networkingv1.NetworkPolicy{}, builder.WithPredicates(launcherPolicy)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.policiesForPod), builder.WithPredicates(launcherPod)).
		Complete(r)
}

// ensureLauncherPolicy creates a launcher's NetworkPolicy before its pod, with
// the isolation finalizer. ok is false while a policy by that name belongs to
// someone else or is being deleted: that is a previous launcher by the same
// name still stopping, and a pod created now would match a policy that is
// about to go.
func ensureLauncherPolicy(ctx context.Context, c client.Client, owner client.Object, np *networkingv1.NetworkPolicy) (bool, string, error) {
	controllerutil.AddFinalizer(np, launcherIsolationFinalizer)
	err := c.Create(ctx, np)
	if err == nil {
		return true, "", nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return false, "", err
	}
	var existing networkingv1.NetworkPolicy
	if err := c.Get(ctx, client.ObjectKeyFromObject(np), &existing); err != nil {
		return false, "", client.IgnoreNotFound(err)
	}
	if existing.DeletionTimestamp != nil || !metav1.IsControlledBy(&existing, owner) {
		return false, fmt.Sprintf("NetworkPolicy %s still isolates a previous launcher by this name; waiting for that launcher to stop", np.Name), nil
	}
	return true, "", nil
}

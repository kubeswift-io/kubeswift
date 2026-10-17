package swiftsandbox

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/artifactpolicy"
)

// ArtifactPolicyStatusReconciler reports whether each
// SwiftArtifactVerificationPolicy can be enforced: its keys' identities, the
// namespaces it selects, and a Ready condition. It never changes a policy's
// spec (its RBAC is read plus status). Enforcement does not wait for it: the
// sandbox and placement controllers evaluate policies themselves and refuse
// what an invalid one governs.
type ArtifactPolicyStatusReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *ArtifactPolicyStatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p sandboxv1alpha1.SwiftArtifactVerificationPolicy
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	c := artifactpolicy.Compile(&p)
	next := p.Status.DeepCopy()
	next.ObservedGeneration = p.Generation
	next.Authorities = c.Authorities

	sel, err := metav1.LabelSelectorAsSelector(&p.Spec.NamespaceSelector)
	if err == nil {
		var nss corev1.NamespaceList
		if err := r.List(ctx, &nss); err != nil {
			return ctrl.Result{}, err
		}
		var n int32
		for i := range nss.Items {
			ls := labels.Merge(labels.Set(nss.Items[i].Labels), labels.Set{"kubernetes.io/metadata.name": nss.Items[i].Name})
			if sel.Matches(ls) {
				n++
			}
		}
		next.Namespaces = n
	}

	cond := metav1.Condition{Type: sandboxv1alpha1.VerificationPolicyConditionReady, Status: metav1.ConditionTrue,
		Reason: sandboxv1alpha1.VerificationPolicyReasonValid, Message: "the policy is enforced", ObservedGeneration: p.Generation}
	if c.Err != nil {
		reason := sandboxv1alpha1.VerificationPolicyReasonInvalid
		for _, a := range c.Authorities {
			if a.KeyID == "" {
				reason = sandboxv1alpha1.VerificationPolicyReasonInvalidKey
			}
		}
		cond.Status, cond.Reason = metav1.ConditionFalse, reason
		cond.Message = c.Err.Error() + "; the artifacts it governs are refused until it is fixed"
	}
	prev := apimeta.FindStatusCondition(p.Status.Conditions, sandboxv1alpha1.VerificationPolicyConditionReady)
	becameInvalid := c.Err != nil && (prev == nil || prev.Status == metav1.ConditionTrue)
	apimeta.SetStatusCondition(&next.Conditions, cond)
	if apiequality.Semantic.DeepEqual(next, &p.Status) {
		return ctrl.Result{}, nil
	}
	p.Status = *next
	if err := r.Status().Update(ctx, &p); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	if becameInvalid && r.Recorder != nil {
		r.Recorder.Event(&p, corev1.EventTypeWarning, cond.Reason, cond.Message)
	}
	return ctrl.Result{}, nil
}

func (r *ArtifactPolicyStatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A namespace's labels change how many namespaces a policy selects.
	allPolicies := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list sandboxv1alpha1.SwiftArtifactVerificationPolicyList
		if err := r.List(ctx, &list); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.SwiftArtifactVerificationPolicy{}).
		Watches(&corev1.Namespace{}, allPolicies).
		Named("swiftartifactverificationpolicy").
		Complete(r)
}

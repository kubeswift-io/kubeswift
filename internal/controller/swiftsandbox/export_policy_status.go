package swiftsandbox

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/exportpolicy"
)

// exportPolicyRecheck re-reads a policy's credential Secret: Secrets are not
// watched (no cluster-wide Secret informer), so a Secret removed or fixed
// later shows up within this period.
const exportPolicyRecheck = 5 * time.Minute

// ExportPolicyStatusReconciler reports whether each
// SwiftArtifactExportPolicy is usable: its spec compiles, its credential
// Secret is in place, and how many namespaces it selects. It never changes a
// policy's spec. Exports do not wait for it: the sandbox controller resolves
// policies itself and refuses what an unusable one would allow.
type ExportPolicyStatusReconciler struct {
	client.Client
	// APIReader reads the credential Secret without a Secret informer.
	APIReader client.Reader
	Recorder  record.EventRecorder
	// SystemNamespace holds export credentials (the controller's namespace).
	SystemNamespace string
}

func (r *ExportPolicyStatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	defer requeueOnConflict(ctx, &res, &err)
	var p sandboxv1alpha1.SwiftArtifactExportPolicy
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	next := p.Status.DeepCopy()
	next.ObservedGeneration = p.Generation
	next.Namespaces = 0

	cond := metav1.Condition{Type: sandboxv1alpha1.ExportPolicyConditionReady, Status: metav1.ConditionTrue,
		Reason: sandboxv1alpha1.ExportPolicyReasonValid, ObservedGeneration: p.Generation}
	sel, selErr := metav1.LabelSelectorAsSelector(&p.Spec.NamespaceSelector)
	if selErr == nil {
		var nss corev1.NamespaceList
		if err := r.List(ctx, &nss); err != nil {
			return ctrl.Result{}, err
		}
		for i := range nss.Items {
			ls := labels.Merge(labels.Set(nss.Items[i].Labels), labels.Set{"kubernetes.io/metadata.name": nss.Items[i].Name})
			if sel.Matches(ls) {
				next.Namespaces++
			}
		}
	}
	switch {
	case selErr != nil:
		cond.Status, cond.Reason = metav1.ConditionFalse, sandboxv1alpha1.ExportPolicyReasonInvalid
		cond.Message = fmt.Sprintf("namespaceSelector: %v; outputs in namespaces it would select are refused", selErr)
	case exportpolicy.Validate(&p.Spec) != nil:
		cond.Status, cond.Reason = metav1.ConditionFalse, sandboxv1alpha1.ExportPolicyReasonInvalid
		cond.Message = exportpolicy.Validate(&p.Spec).Error() + "; outputs in the namespaces it selects are refused"
	default:
		msg, err := r.credentialProblem(ctx, p.Spec.CredentialsSecretRef.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if msg != "" {
			cond.Status, cond.Reason = metav1.ConditionFalse, sandboxv1alpha1.ExportPolicyReasonCredentialsMissing
			cond.Message = msg + "; outputs it allows are refused"
		} else {
			cond.Message = fmt.Sprintf("outputs from %d namespace(s) may be exported", next.Namespaces)
		}
	}

	prev := apimeta.FindStatusCondition(p.Status.Conditions, sandboxv1alpha1.ExportPolicyConditionReady)
	becameUnusable := cond.Status == metav1.ConditionFalse && (prev == nil || prev.Status == metav1.ConditionTrue || prev.Reason != cond.Reason)
	apimeta.SetStatusCondition(&next.Conditions, cond)
	if apiequality.Semantic.DeepEqual(next, &p.Status) {
		return ctrl.Result{RequeueAfter: exportPolicyRecheck}, nil
	}
	p.Status = *next
	if err := r.Status().Update(ctx, &p); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	if becameUnusable && r.Recorder != nil {
		r.Recorder.Event(&p, corev1.EventTypeWarning, cond.Reason, cond.Message)
	}
	return ctrl.Result{RequeueAfter: exportPolicyRecheck}, nil
}

// credentialProblem says what is wrong with the policy's credential Secret,
// or "" when it is usable.
func (r *ExportPolicyStatusReconciler) credentialProblem(ctx context.Context, name string) (string, error) {
	if r.SystemNamespace == "" {
		return "the controller has no namespace to keep export credentials in", nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var s corev1.Secret
	if err := reader.Get(ctx, client.ObjectKey{Namespace: r.SystemNamespace, Name: name}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("credential Secret %s/%s does not exist", r.SystemNamespace, name), nil
		}
		return "", err
	}
	if s.Labels[sandboxv1alpha1.ExportCredentialsLabel] != "true" {
		return fmt.Sprintf("Secret %s/%s is not labelled %s=true", r.SystemNamespace, name, sandboxv1alpha1.ExportCredentialsLabel), nil
	}
	if len(s.Data[corev1.DockerConfigJsonKey]) == 0 {
		return fmt.Sprintf("Secret %s/%s has no %s", r.SystemNamespace, name, corev1.DockerConfigJsonKey), nil
	}
	return "", nil
}

func (r *ExportPolicyStatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A namespace's labels change how many namespaces a policy selects.
	allPolicies := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list sandboxv1alpha1.SwiftArtifactExportPolicyList
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
		For(&sandboxv1alpha1.SwiftArtifactExportPolicy{}).
		Watches(&corev1.Namespace{}, allPolicies).
		Named("swiftartifactexportpolicy").
		Complete(r)
}

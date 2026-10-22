package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// The status says whether a policy can be used and, when not, why.
func TestExportPolicyStatus(t *testing.T) {
	invalid := exportPolicy("invalid", "uid-i", "push")
	invalid.Spec.Repositories = []string{"no-host-here"}
	for _, tc := range []struct {
		name   string
		policy *sandboxv1alpha1.SwiftArtifactExportPolicy
		objs   []client.Object
		status metav1.ConditionStatus
		reason string
		msg    string
	}{
		{"usable", exportPolicy("p", "uid-p", "push"), []client.Object{exportSecret("push", true)},
			metav1.ConditionTrue, sandboxv1alpha1.ExportPolicyReasonValid, "1 namespace"},
		{"secret missing", exportPolicy("p", "uid-p", "push"), nil,
			metav1.ConditionFalse, sandboxv1alpha1.ExportPolicyReasonCredentialsMissing, "does not exist"},
		{"secret not set aside", exportPolicy("p", "uid-p", "push"), []client.Object{exportSecret("push", false)},
			metav1.ConditionFalse, sandboxv1alpha1.ExportPolicyReasonCredentialsMissing, "not labelled"},
		{"spec invalid", invalid, []client.Object{exportSecret("push", true)},
			metav1.ConditionFalse, sandboxv1alpha1.ExportPolicyReasonInvalid, "refused"},
	} {
		objs := append([]client.Object{tc.policy, defaultNamespace(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"}}}, tc.objs...)
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
			WithStatusSubresource(&sandboxv1alpha1.SwiftArtifactExportPolicy{}).Build()
		rec := record.NewFakeRecorder(5)
		r := &ExportPolicyStatusReconciler{Client: c, APIReader: c, Recorder: rec, SystemNamespace: testSystemNS}
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tc.policy.Name}})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if res.RequeueAfter != exportPolicyRecheck {
			t.Errorf("%s: requeue %v, want a periodic recheck of the Secret", tc.name, res.RequeueAfter)
		}
		var p sandboxv1alpha1.SwiftArtifactExportPolicy
		_ = c.Get(context.Background(), types.NamespacedName{Name: tc.policy.Name}, &p)
		cond := apimeta.FindStatusCondition(p.Status.Conditions, sandboxv1alpha1.ExportPolicyConditionReady)
		if cond == nil || cond.Status != tc.status || cond.Reason != tc.reason || !strings.Contains(cond.Message, tc.msg) {
			t.Errorf("%s: Ready %+v, want %s/%s containing %q", tc.name, cond, tc.status, tc.reason, tc.msg)
		}
		if p.Status.Namespaces != 1 {
			t.Errorf("%s: namespaces %d, want 1", tc.name, p.Status.Namespaces)
		}
		select {
		case e := <-rec.Events:
			if tc.status == metav1.ConditionTrue || !strings.Contains(e, tc.reason) {
				t.Errorf("%s: event %q", tc.name, e)
			}
		default:
			if tc.status == metav1.ConditionFalse {
				t.Errorf("%s: no Warning event for an unusable policy", tc.name)
			}
		}
	}
}

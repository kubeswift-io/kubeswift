package swiftimage

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
)

// An import that fails at the start put its error text in the condition
// reason. A reason must be a CamelCase token, so the apiserver refused the
// whole status write: the image never reached Failed and looped on reconciler
// errors with no status (#715, seen on a lab cluster). The fake client accepts
// any reason, so the conditions are checked with the apiserver's own rules.
func TestReconcile_ImportStartFailureWritesAValidCondition(t *testing.T) {
	for name, src := range map[string]imagev1alpha1.ImageSource{
		"pvcClone":  {PVCClone: &imagev1alpha1.PVCCloneSource{Name: "src"}},
		"no source": {},
	} {
		scheme := testScheme()
		img := &imagev1alpha1.SwiftImage{
			ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "default"},
			Spec:       imagev1alpha1.SwiftImageSpec{Format: imagev1alpha1.DiskFormatRaw, Source: src},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&imagev1alpha1.SwiftImage{}).WithObjects(img).Build()
		r := &SwiftImageReconciler{Client: c, Scheme: scheme}
		key := types.NamespacedName{Name: "img", Namespace: "default"}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("%s: reconcile: %v", name, err)
		}
		var got imagev1alpha1.SwiftImage
		if err := c.Get(context.Background(), key, &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase != imagev1alpha1.SwiftImagePhaseFailed {
			t.Errorf("%s: phase = %q, want Failed", name, got.Status.Phase)
		}
		if errs := metav1validation.ValidateConditions(got.Status.Conditions, field.NewPath("status", "conditions")); len(errs) > 0 {
			t.Errorf("%s: the apiserver would refuse these conditions: %v", name, errs.ToAggregate())
		}
		var reason, msg string
		for _, cond := range got.Status.Conditions {
			if cond.Status == metav1.ConditionTrue || cond.Reason == ReasonImportFailed {
				reason, msg = cond.Reason, cond.Message
			}
		}
		if reason != ReasonImportFailed || msg == "" {
			t.Errorf("%s: reason %q message %q, want %s with the error as message", name, reason, msg, ReasonImportFailed)
		}
		if name == "pvcClone" && !strings.Contains(msg, "pvcClone is not implemented yet") {
			t.Errorf("pvcClone: message %q", msg)
		}
	}
}

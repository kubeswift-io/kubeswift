package swiftimage

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

// TestReconcile_FailedImport_CountsImageImportTotal proves the O3 import
// outcome counter fires on a failed import — the case that was invisible
// before O3 (only the success-latency histogram existed). A no-source
// SwiftImage fails in StartImport, drives the reconcile to phase=Failed, and
// must increment image_import_total{result="failed"}.
func TestReconcile_FailedImport_CountsImageImportTotal(t *testing.T) {
	scheme := testScheme()
	img := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "badimg", Namespace: "imgns"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format: imagev1alpha1.DiskFormatRaw,
			Source: imagev1alpha1.ImageSource{}, // no source -> Failed
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&imagev1alpha1.SwiftImage{}).
		WithObjects(img).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme}

	before := testutil.ToFloat64(metrics.ImageImportTotal.WithLabelValues("imgns", "failed"))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "badimg", Namespace: "imgns"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(metrics.ImageImportTotal.WithLabelValues("imgns", "failed")); got != before+1 {
		t.Errorf("image_import_total{failed} = %v, want %v", got, before+1)
	}

	// Re-reconcile a terminal (Failed) image: the Ready/Failed switch case
	// returns early, so the counter must NOT move.
	stable := testutil.ToFloat64(metrics.ImageImportTotal.WithLabelValues("imgns", "failed"))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "badimg", Namespace: "imgns"},
	}); err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	if got := testutil.ToFloat64(metrics.ImageImportTotal.WithLabelValues("imgns", "failed")); got != stable {
		t.Errorf("terminal-image re-reconcile must not re-count; %v -> %v", stable, got)
	}
}

package namespaces

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func TestTerminating(t *testing.T) {
	now := metav1.Now()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "live"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "going", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "phase"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating}},
	).Build()
	for ns, want := range map[string]bool{"live": false, "going": true, "phase": true, "absent": false, "": false} {
		got, err := Terminating(context.Background(), c, ns)
		if err != nil {
			t.Fatalf("%s: %v", ns, err)
		}
		if got != want {
			t.Errorf("Terminating(%s) = %v, want %v", ns, got, want)
		}
	}
}

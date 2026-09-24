package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// A stopping guest whose launcher finishes terminating between the stop check
// and the launcher lookup must not get a fresh launcher in that pass.
func TestReconcile_StoppingGuestGetsNoNewLauncherInTheRace(t *testing.T) {
	g := kernelGuest()
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	g.Status = *ranStatus("pod-1")
	terminating := runningLauncher("worker-1")
	terminating.UID = "pod-1"
	lookups := 0
	created := 0
	c := guestClientBuilder(g, testGuestClass(), readyKernel(), terminating).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod && key.Name == testGuestName {
					lookups++
					if lookups > 1 { // gone by the second lookup
						return apierrors.NewNotFound(corev1.Resource("pods"), key.Name)
					}
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod {
					created++
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	if _, _, err := reconcileGuest(t, r); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Errorf("created %d launcher(s) for a guest with runPolicy Stopped", created)
	}
}

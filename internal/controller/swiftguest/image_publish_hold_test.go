package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func TestImagePublishHold(t *testing.T) {
	pub := func(phase imagev1alpha1.SwiftImagePublishPhase, guest string, finalizer bool) *imagev1alpha1.SwiftImagePublish {
		p := &imagev1alpha1.SwiftImagePublish{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p1"},
			Spec: imagev1alpha1.SwiftImagePublishSpec{
				Source: imagev1alpha1.ImagePublishSource{GuestRef: &corev1.LocalObjectReference{Name: guest}},
			},
			Status: imagev1alpha1.SwiftImagePublishStatus{Phase: phase},
		}
		if finalizer {
			p.Finalizers = []string{imagev1alpha1.FinalizerPublish}
		}
		return p
	}
	cases := []struct {
		name string
		pub  *imagev1alpha1.SwiftImagePublish
		want string
	}{
		{"publishing holds", pub(imagev1alpha1.SwiftImagePublishPublishing, "web", true), "p1"},
		{"pending holds", pub(imagev1alpha1.SwiftImagePublishPending, "web", true), "p1"},
		{"ready, not yet released, holds", pub(imagev1alpha1.SwiftImagePublishReady, "web", true), "p1"},
		{"ready and released", pub(imagev1alpha1.SwiftImagePublishReady, "web", false), ""},
		{"failed and released", pub(imagev1alpha1.SwiftImagePublishFailed, "web", false), ""},
		{"another guest's request", pub(imagev1alpha1.SwiftImagePublishPublishing, "other", true), ""},
		{"request gone", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(scheme.Scheme)
			if tc.pub != nil {
				b = b.WithObjects(tc.pub)
			}
			r := &SwiftGuestReconciler{Client: b.Build()}
			g := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web",
				Annotations: map[string]string{imagev1alpha1.AnnotationPublishHold: "p1"}}}
			got, err := r.imagePublishHold(context.Background(), g)
			if err != nil || got != tc.want {
				t.Fatalf("hold = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	r := &SwiftGuestReconciler{Client: fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()}
	if got, _ := r.imagePublishHold(context.Background(), &swiftv1alpha1.SwiftGuest{}); got != "" {
		t.Fatalf("unannotated guest held by %q", got)
	}
	var _ client.Reader = r.Client
}

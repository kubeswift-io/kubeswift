package eviction

import (
	"context"
	"net/http"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

func guestPod(name, ns, guest, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{guestLabelKey: guest},
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

func plainPod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func swiftGuest(name, ns string, mig *swiftv1alpha1.MigrationSpec) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       swiftv1alpha1.SwiftGuestSpec{Migration: mig},
	}
}

func boolPtr(b bool) *bool { return &b }

func evictReq(ns, name string, dryRun bool) admission.Request {
	r := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Name:      name,
			Namespace: ns,
		},
	}
	if dryRun {
		r.DryRun = boolPtr(true)
	}
	return r
}

func markerOf(t *testing.T, c client.Client, ns, name string) string {
	t.Helper()
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &g); err != nil {
		t.Fatalf("get guest %q: %v", name, err)
	}
	return g.Annotations[swiftv1alpha1.AnnotationDrainRequested]
}

func TestHandle(t *testing.T) {
	const ns = "default"
	vfioGuestSpec := func(name string) *swiftv1alpha1.SwiftGuest {
		g := swiftGuest(name, ns, &swiftv1alpha1.MigrationSpec{DrainPolicy: swiftv1alpha1.DrainPolicyLiveMigrate})
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
		return g
	}

	tests := []struct {
		name        string
		objects     []client.Object
		req         admission.Request
		wantAllowed bool
		wantMarked  string // expected drain-requested marker value ("" = absent)
	}{
		{
			name:        "pod not found allows",
			objects:     nil,
			req:         evictReq(ns, "ghost", false),
			wantAllowed: true,
		},
		{
			name:        "non-guest pod allows",
			objects:     []client.Object{plainPod("app", ns, "miles")},
			req:         evictReq(ns, "app", false),
			wantAllowed: true,
		},
		{
			name:        "guest pod without SwiftGuest CR allows (orphan)",
			objects:     []client.Object{guestPod("g-pod", ns, "g", "miles")},
			req:         evictReq(ns, "g-pod", false),
			wantAllowed: true,
		},
		{
			name: "migratable default policy denies and marks",
			objects: []client.Object{
				guestPod("g-pod", ns, "g", "miles"),
				swiftGuest("g", ns, nil), // nil migration → defaults (enabled, Migrate)
			},
			req:         evictReq(ns, "g-pod", false),
			wantAllowed: false,
			wantMarked:  "miles",
		},
		{
			name: "dry-run denies but does not mark",
			objects: []client.Object{
				guestPod("g-pod", ns, "g", "miles"),
				swiftGuest("g", ns, nil),
			},
			req:         evictReq(ns, "g-pod", true),
			wantAllowed: false,
			wantMarked:  "",
		},
		{
			name: "migration disabled denies without marking",
			objects: []client.Object{
				guestPod("g-pod", ns, "g", "miles"),
				swiftGuest("g", ns, &swiftv1alpha1.MigrationSpec{Enabled: boolPtr(false)}),
			},
			req:         evictReq(ns, "g-pod", false),
			wantAllowed: false,
			wantMarked:  "",
		},
		{
			name: "drainPolicy Block denies without marking",
			objects: []client.Object{
				guestPod("g-pod", ns, "g", "miles"),
				swiftGuest("g", ns, &swiftv1alpha1.MigrationSpec{DrainPolicy: swiftv1alpha1.DrainPolicyBlock}),
			},
			req:         evictReq(ns, "g-pod", false),
			wantAllowed: false,
			wantMarked:  "",
		},
		{
			name: "LiveMigrate on VFIO guest denies without marking",
			objects: []client.Object{
				guestPod("g-pod", ns, "g", "miles"),
				vfioGuestSpec("g"),
			},
			req:         evictReq(ns, "g-pod", false),
			wantAllowed: false,
			wantMarked:  "",
		},
		{
			name: "LiveMigrate on non-VFIO guest denies and marks",
			objects: []client.Object{
				guestPod("g-pod", ns, "g", "miles"),
				swiftGuest("g", ns, &swiftv1alpha1.MigrationSpec{DrainPolicy: swiftv1alpha1.DrainPolicyLiveMigrate}),
			},
			req:         evictReq(ns, "g-pod", false),
			wantAllowed: false,
			wantMarked:  "miles",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := scheme.Scheme
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objects...).Build()
			h := &Handler{Client: c}

			resp := h.Handle(context.Background(), tc.req)

			if resp.Allowed != tc.wantAllowed {
				t.Fatalf("Allowed = %v, want %v (msg=%q)", resp.Allowed, tc.wantAllowed, respMsg(resp))
			}
			if !tc.wantAllowed {
				if resp.Result == nil || resp.Result.Code != http.StatusTooManyRequests {
					t.Errorf("deny must use 429 TooManyRequests so drain retries; got %+v", resp.Result)
				}
			}
			// Marker assertion only when a SwiftGuest exists.
			if hasGuest(tc.objects) {
				if got := markerOf(t, c, ns, "g"); got != tc.wantMarked {
					t.Errorf("drain-requested marker = %q, want %q", got, tc.wantMarked)
				}
			}
		})
	}
}

// TestHandle_MarkerIdempotent verifies a second eviction (marker already set
// to this node) is still denied and does not error.
func TestHandle_MarkerIdempotent(t *testing.T) {
	const ns = "default"
	s := scheme.Scheme
	g := swiftGuest("g", ns, nil)
	g.Annotations = map[string]string{swiftv1alpha1.AnnotationDrainRequested: "miles"}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(guestPod("g-pod", ns, "g", "miles"), g).Build()
	h := &Handler{Client: c}

	resp := h.Handle(context.Background(), evictReq(ns, "g-pod", false))
	if resp.Allowed {
		t.Fatalf("already-marked migratable guest must still be denied; got allowed")
	}
	if got := markerOf(t, c, ns, "g"); got != "miles" {
		t.Errorf("marker = %q, want miles (unchanged)", got)
	}
}

func hasGuest(objs []client.Object) bool {
	for _, o := range objs {
		if _, ok := o.(*swiftv1alpha1.SwiftGuest); ok {
			return true
		}
	}
	return false
}

func respMsg(r admission.Response) string {
	if r.Result == nil {
		return ""
	}
	return r.Result.Message
}

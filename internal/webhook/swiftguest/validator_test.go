package swiftguest

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func guest(mut func(*swiftv1alpha1.SwiftGuest)) *swiftv1alpha1.SwiftGuest {
	g := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "cls"},
		},
	}
	if mut != nil {
		mut(g)
	}
	return g
}

func errContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want containing %q", err, want)
	}
}

func TestValidate_BootSourceExclusivity(t *testing.T) {
	// imageRef alone: OK.
	if err := validateSwiftGuest(guest(nil)); err != nil {
		t.Errorf("imageRef-only should be valid: %v", err)
	}
	// kernelRef alone: OK.
	if err := validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.ImageRef = nil
		g.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	})); err != nil {
		t.Errorf("kernelRef-only should be valid: %v", err)
	}
	// cloneFromSnapshot alone (no guestClassRef): OK.
	if err := validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.ImageRef = nil
		g.Spec.GuestClassRef = corev1.LocalObjectReference{}
		g.Spec.CloneFromSnapshot = &swiftv1alpha1.CloneFromSnapshotSource{
			SnapshotRef: corev1.LocalObjectReference{Name: "snap"},
		}
	})); err != nil {
		t.Errorf("cloneFromSnapshot-only should be valid (guestClassRef optional): %v", err)
	}
	// none set.
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) { g.Spec.ImageRef = nil })),
		"exactly one of spec.imageRef")
	// two set (imageRef + cloneFromSnapshot).
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.CloneFromSnapshot = &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "snap"}}
	})), "exactly one of spec.imageRef")
}

func TestValidate_CloneFromSnapshotRules(t *testing.T) {
	cloneGuest := func(mut func(*swiftv1alpha1.SwiftGuest)) *swiftv1alpha1.SwiftGuest {
		return guest(func(g *swiftv1alpha1.SwiftGuest) {
			g.Spec.ImageRef = nil
			g.Spec.GuestClassRef = corev1.LocalObjectReference{}
			g.Spec.CloneFromSnapshot = &swiftv1alpha1.CloneFromSnapshotSource{
				SnapshotRef: corev1.LocalObjectReference{Name: "snap"},
			}
			if mut != nil {
				mut(g)
			}
		})
	}
	// missing snapshotRef.name.
	errContains(t, validateSwiftGuest(cloneGuest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.CloneFromSnapshot.SnapshotRef.Name = ""
	})), "snapshotRef.name is required")
	// gpuProfileRef + cloneFromSnapshot: rejected.
	errContains(t, validateSwiftGuest(cloneGuest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
	})), "mutually exclusive with spec.gpuProfileRef")
}

func TestValidate_GuestClassRequiredForImageBoot(t *testing.T) {
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GuestClassRef = corev1.LocalObjectReference{}
	})), "spec.guestClassRef.name is required")
}

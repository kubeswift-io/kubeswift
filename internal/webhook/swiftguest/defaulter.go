package swiftguest

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// Defaulter defaults SwiftGuest fields.
type Defaulter struct{}

func (d *Defaulter) Default(ctx context.Context, obj runtime.Object) error {
	g, ok := obj.(*swiftv1alpha1.SwiftGuest)
	if !ok {
		return nil
	}
	if g.Spec.RunPolicy == "" {
		g.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning
	}
	return nil
}

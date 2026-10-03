// Package swiftstoragelocation validates storage.kubeswift.io locations at
// admission: a well-formed spec, and at most one default per level.
//
// The webhook is off by default; the CRD schema enforces what it can (oci or
// csi set; anonymous excludes a Secret name), and the controller's Ready
// condition reports a second default the webhook was not there to refuse.
package swiftstoragelocation

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

// ClusterValidator validates SwiftClusterStorageLocations.
type ClusterValidator struct {
	Client client.Reader
}

// NamespaceValidator validates SwiftStorageLocations.
type NamespaceValidator struct {
	Client client.Reader
}

func (v *ClusterValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	loc, ok := obj.(*storagev1alpha1.SwiftClusterStorageLocation)
	if !ok {
		return nil, fmt.Errorf("expected SwiftClusterStorageLocation, got %T", obj)
	}
	return nil, v.validate(ctx, loc, false)
}

func (v *ClusterValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	loc, ok := newObj.(*storagev1alpha1.SwiftClusterStorageLocation)
	if !ok {
		return nil, fmt.Errorf("expected SwiftClusterStorageLocation, got %T", newObj)
	}
	old, ok := oldObj.(*storagev1alpha1.SwiftClusterStorageLocation)
	if !ok {
		return nil, fmt.Errorf("expected SwiftClusterStorageLocation, got %T", oldObj)
	}
	if loc.DeletionTimestamp != nil {
		return nil, nil
	}
	return nil, v.validate(ctx, loc, old.Spec.Default)
}

func (v *ClusterValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate checks the spec and, when this location becomes a default, that no
// other cluster location already is. One already a default is not re-checked:
// an unrelated edit must not be refused because of a conflict it did not make.
func (v *ClusterValidator) validate(ctx context.Context, loc *storagev1alpha1.SwiftClusterStorageLocation, wasDefault bool) error {
	if err := storagelocation.Validate(&loc.Spec); err != nil {
		return err
	}
	if !loc.Spec.Default || wasDefault || v.Client == nil {
		return nil
	}
	defaults, err := storagelocation.ClusterDefaults(ctx, v.Client)
	if err != nil {
		return err
	}
	if others := storagelocation.Others(defaults, loc.Name); len(others) > 0 {
		return fmt.Errorf("SwiftClusterStorageLocation %s is already the cluster default; unset its spec.default first",
			strings.Join(others, ", "))
	}
	return nil
}

func (v *NamespaceValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	loc, ok := obj.(*storagev1alpha1.SwiftStorageLocation)
	if !ok {
		return nil, fmt.Errorf("expected SwiftStorageLocation, got %T", obj)
	}
	return nil, v.validate(ctx, loc, false)
}

func (v *NamespaceValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	loc, ok := newObj.(*storagev1alpha1.SwiftStorageLocation)
	if !ok {
		return nil, fmt.Errorf("expected SwiftStorageLocation, got %T", newObj)
	}
	old, ok := oldObj.(*storagev1alpha1.SwiftStorageLocation)
	if !ok {
		return nil, fmt.Errorf("expected SwiftStorageLocation, got %T", oldObj)
	}
	if loc.DeletionTimestamp != nil {
		return nil, nil
	}
	return nil, v.validate(ctx, loc, old.Spec.Default)
}

func (v *NamespaceValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *NamespaceValidator) validate(ctx context.Context, loc *storagev1alpha1.SwiftStorageLocation, wasDefault bool) error {
	if err := storagelocation.Validate(&loc.Spec); err != nil {
		return err
	}
	if !loc.Spec.Default || wasDefault || v.Client == nil {
		return nil
	}
	defaults, err := storagelocation.NamespaceDefaults(ctx, v.Client, loc.Namespace)
	if err != nil {
		return err
	}
	if others := storagelocation.Others(defaults, loc.Name); len(others) > 0 {
		return fmt.Errorf("SwiftStorageLocation %s is already the default in namespace %s; unset its spec.default first",
			strings.Join(others, ", "), loc.Namespace)
	}
	return nil
}

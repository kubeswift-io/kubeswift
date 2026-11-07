// Package swiftvolumeattachment contains the admission webhook validator for
// SwiftVolumeAttachment. It refuses at create what the controller would
// fail anyway (internal/hotplug rules), so the user gets the answer from
// kubectl. The controller applies the same rules, since this webhook is off
// by default and two attachments created at once both pass it.
package swiftvolumeattachment

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/hotplug"
)

// Validator validates SwiftVolumeAttachment resources.
type Validator struct {
	Client client.Reader
	// Enabled mirrors the controller-manager's --volume-hotplug flag. When
	// false every create is refused: nothing would act on the attachment.
	Enabled bool
}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sva, ok := obj.(*swiftv1alpha1.SwiftVolumeAttachment)
	if !ok {
		return nil, fmt.Errorf("expected SwiftVolumeAttachment, got %T", obj)
	}
	if !v.Enabled {
		return nil, fmt.Errorf("volume hotplug is not enabled on this cluster (chart value swiftGuest.volumeHotplug.enabled)")
	}
	var warnings admission.Warnings

	var guest swiftv1alpha1.SwiftGuest
	err := v.Client.Get(ctx, client.ObjectKey{Namespace: sva.Namespace, Name: sva.Spec.GuestRef.Name}, &guest)
	switch {
	case apierrors.IsNotFound(err):
		// Allowed: the attachment waits (Pending GuestNotFound), so a
		// manifest may create both at once.
		warnings = append(warnings, fmt.Sprintf("SwiftGuest %s does not exist yet; the attachment waits for it", sva.Spec.GuestRef.Name))
	case err != nil:
		return nil, fmt.Errorf("look up SwiftGuest %s: %w", sva.Spec.GuestRef.Name, err)
	default:
		if m := hotplug.GuestRefusal(&guest); m != "" {
			return nil, fmt.Errorf("%s", m)
		}
	}

	var pvc *corev1.PersistentVolumeClaim
	var claim corev1.PersistentVolumeClaim
	err = v.Client.Get(ctx, client.ObjectKey{Namespace: sva.Namespace, Name: sva.Spec.PVCRef.Name}, &claim)
	switch {
	case apierrors.IsNotFound(err):
		warnings = append(warnings, fmt.Sprintf("PersistentVolumeClaim %s does not exist yet; the attachment waits for it", sva.Spec.PVCRef.Name))
	case err != nil:
		return nil, fmt.Errorf("look up PersistentVolumeClaim %s: %w", sva.Spec.PVCRef.Name, err)
	default:
		pvc = &claim
		if m := hotplug.PVCRefusal(pvc); m != "" {
			return nil, fmt.Errorf("%s", m)
		}
		// A claim that waits for its first consumer is bound only once the
		// attachment pod runs; the controller checks the volume then.
		if pvc.Spec.VolumeName != "" {
			var pv corev1.PersistentVolume
			if err := v.Client.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, &pv); err == nil {
				if m := hotplug.PVRefusal(&pv); m != "" {
					return nil, fmt.Errorf("%s", m)
				}
			} else if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("look up PersistentVolume %s: %w", pvc.Spec.VolumeName, err)
			}
		}
	}

	// Every existing attachment came before this one.
	var list swiftv1alpha1.SwiftVolumeAttachmentList
	if err := v.Client.List(ctx, &list, client.InNamespace(sva.Namespace)); err != nil {
		return nil, fmt.Errorf("list SwiftVolumeAttachments: %w", err)
	}
	earlier := make([]swiftv1alpha1.SwiftVolumeAttachment, 0, len(list.Items))
	for _, o := range list.Items {
		if o.DeletionTimestamp.IsZero() && o.Name != sva.Name {
			earlier = append(earlier, o)
		}
	}
	if m := hotplug.SiblingRefusal(sva, earlier, pvc); m != "" {
		return nil, fmt.Errorf("%s", m)
	}
	return warnings, nil
}

// ValidateUpdate allows every update: the spec is immutable (a CEL rule on
// the CRD), and metadata and status are the controller's and the user's.
func (v *Validator) ValidateUpdate(_ context.Context, _, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete allows every delete: deleting is how a volume is detached,
// and the finalizer makes it safe.
func (v *Validator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

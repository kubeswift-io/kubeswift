package swiftimagepublish

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/rootdisk"
)

// Reasons a guest source waits.
const (
	ReasonGuestNotStopped = "GuestNotStopped"
	ReasonGuestHeld       = "GuestHeld"
)

// guestOSType is the osType to record: spec.osType, else the guest's
// image's, else linux.
func guestOSType(ctx context.Context, c client.Reader, pub *imagev1alpha1.SwiftImagePublish, guest *swiftv1alpha1.SwiftGuest) (imagev1alpha1.OSType, error) {
	if pub.Spec.OSType != "" {
		return pub.Spec.OSType, nil
	}
	if guest != nil && guest.Spec.ImageRef != nil {
		var img imagev1alpha1.SwiftImage
		err := c.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: guest.Spec.ImageRef.Name}, &img)
		if err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
		if img.Spec.OSType != "" {
			return img.Spec.OSType, nil
		}
	}
	return imagev1alpha1.OSTypeLinux, nil
}

// resolveGuestSource looks at a guest source and, once the guest is
// Stopped, holds it (AnnotationPublishHold) so it is not started while its
// root disk is read. It then checks, uncached, that the guest is still
// Stopped with no launcher: one created before the guest controller saw the
// hold would be writing the disk.
func (r *Reconciler) resolveGuestSource(ctx context.Context, pub *imagev1alpha1.SwiftImagePublish) (sourceResult, error) {
	name := pub.Spec.Source.GuestRef.Name
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: name}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return sourceResult{wait: &condition{ReasonSourceNotFound, "SwiftGuest " + name + " does not exist"}}, nil
		}
		return sourceResult{}, err
	}
	if guest.Spec.KernelRef != nil {
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftGuest " + name +
			" boots from a kernel (spec.kernelRef); only a disk-boot guest's root disk can be published"}}, nil
	}
	if guest.Spec.GuestClassRef.Name != "" {
		var class swiftv1alpha1.SwiftGuestClass
		err := r.Get(ctx, client.ObjectKey{Name: guest.Spec.GuestClassRef.Name}, &class)
		if err != nil && !apierrors.IsNotFound(err) {
			return sourceResult{}, err
		}
		if err == nil && class.Spec.SharedBaseDisk {
			return sourceResult{fail: &condition{ReasonSourceUnsupported, fmt.Sprintf(
				"SwiftGuest %s uses SwiftGuestClass %s with sharedBaseDisk: true; its disk is a node-local thin device, not a PVC, and cannot be published", name, class.Name)}}, nil
		}
	}
	if guest.Status.Phase != swiftv1alpha1.SwiftGuestPhaseStopped {
		return sourceResult{wait: &condition{ReasonGuestNotStopped, fmt.Sprintf(
			"SwiftGuest %s is %s; set its runPolicy to Stopped (the request waits, and then keeps it stopped until its disk is read)", name, phaseOr(guest.Status.Phase))}}, nil
	}

	if held := guest.Annotations[imagev1alpha1.AnnotationPublishHold]; held != pub.Name {
		if held != "" {
			if active, err := r.publishActive(ctx, pub.Namespace, held); err != nil {
				return sourceResult{}, err
			} else if active {
				return sourceResult{wait: &condition{ReasonGuestHeld, "SwiftGuest " + name + " is being published by SwiftImagePublish " + held}}, nil
			}
		}
		if err := setHold(ctx, r.Client, &guest, pub.Name); err != nil {
			if apierrors.IsConflict(err) {
				return sourceResult{wait: &condition{ReasonGuestHeld, "SwiftGuest " + name + " changed while it was being held; retrying"}}, nil
			}
			return sourceResult{}, err
		}
	}

	// Uncached: still Stopped, and no launcher.
	reader := r.reader()
	var live swiftv1alpha1.SwiftGuest
	if err := reader.Get(ctx, client.ObjectKeyFromObject(&guest), &live); err != nil {
		return sourceResult{}, client.IgnoreNotFound(err)
	}
	pod, err := swiftguest.LauncherPod(ctx, reader, &live)
	if err != nil {
		return sourceResult{}, err
	}
	if live.Status.Phase != swiftv1alpha1.SwiftGuestPhaseStopped || pod != nil {
		return sourceResult{wait: &condition{ReasonGuestNotStopped, "SwiftGuest " + name + " still has a launcher; waiting for it to stop"}}, nil
	}

	claim, _, err := rootdisk.Claim(ctx, reader, &live)
	if err != nil {
		return sourceResult{}, err
	}
	var pvc corev1.PersistentVolumeClaim
	if err := reader.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: claim}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftGuest " + name + " has no root disk PVC " + claim}}, nil
		}
		return sourceResult{}, err
	}
	size := pvc.Status.Capacity.Storage().Value()
	if size <= 0 {
		size = pvc.Spec.Resources.Requests.Storage().Value()
	}
	osType, err := guestOSType(ctx, r.Client, pub, &live)
	if err != nil {
		return sourceResult{}, err
	}
	return sourceResult{status: &imagev1alpha1.ImagePublishSourceStatus{
		Kind:      "SwiftGuest",
		Name:      live.Name,
		UID:       string(live.UID),
		PVCName:   claim,
		Block:     pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock,
		SizeBytes: size,
		OSType:    osType,
	}}, nil
}

func phaseOr(p swiftv1alpha1.SwiftGuestPhase) string {
	if p == "" {
		return "not Stopped"
	}
	return string(p)
}

// publishActive reports whether the request namespace/name exists and is
// not Ready or Failed.
func (r *Reconciler) publishActive(ctx context.Context, namespace, name string) (bool, error) {
	var other imagev1alpha1.SwiftImagePublish
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &other); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	switch other.Status.Phase {
	case imagev1alpha1.SwiftImagePublishReady, imagev1alpha1.SwiftImagePublishFailed:
		return other.DeletionTimestamp != nil && hasFinalizer(&other), nil
	}
	return true, nil
}

// setHold stamps the hold on guest, optimistically locked so it cannot race
// a change of the guest.
func setHold(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest, value string) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": guest.ResourceVersion,
		"annotations":     map[string]any{imagev1alpha1.AnnotationPublishHold: value},
	}})
	return c.Patch(ctx, guest, client.RawPatch(types.MergePatchType, patch))
}

// releaseHold removes this request's hold from its guest, if it still holds
// it. A guest that is gone holds nothing.
func (r *Reconciler) releaseHold(ctx context.Context, pub *imagev1alpha1.SwiftImagePublish) error {
	if pub.Spec.Source.GuestRef == nil {
		return nil
	}
	var guest swiftv1alpha1.SwiftGuest
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: pub.Spec.Source.GuestRef.Name}, &guest); err != nil {
		return client.IgnoreNotFound(err)
	}
	if guest.Annotations[imagev1alpha1.AnnotationPublishHold] != pub.Name {
		return nil
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": guest.ResourceVersion,
		"annotations":     map[string]any{imagev1alpha1.AnnotationPublishHold: nil},
	}})
	return r.Patch(ctx, &guest, client.RawPatch(types.MergePatchType, patch))
}

package hotplug

import (
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// The admission rules for a SwiftVolumeAttachment. The webhook applies them
// at create, and the controller applies them on every pass, because the
// webhook is off by default and two attachments created at once both pass
// it. Each returns "" when the attachment passes, else why it does not.

// hypervisorOverrideAnnotation selects QEMU for a guest without a GPU
// (webhook/swiftsnapshot.HypervisorOverrideAnnotation).
const hypervisorOverrideAnnotation = "kubeswift.io/hypervisor-override"

// GuestRefusal checks the guest: Cloud Hypervisor, no GPU, guest agent on.
func GuestRefusal(guest *swiftv1alpha1.SwiftGuest) string {
	switch {
	case guest.Spec.GPUProfileRef != nil || guest.Spec.GPUResourceClaim != nil:
		return fmt.Sprintf("SwiftGuest %s has a GPU; hot-plugged volumes are supported on Cloud Hypervisor guests without a GPU", guest.Name)
	case guest.Annotations[hypervisorOverrideAnnotation] == "qemu":
		return fmt.Sprintf("SwiftGuest %s runs on QEMU; hot-plugged volumes are supported on Cloud Hypervisor only", guest.Name)
	case guest.Spec.GuestAgent == nil || !guest.Spec.GuestAgent.Enabled:
		return fmt.Sprintf("SwiftGuest %s does not set spec.guestAgent.enabled; the guest agent is required to tell "+
			"whether the guest still uses a disk before it is detached", guest.Name)
	}
	return ""
}

// PVCRefusal checks the claim: volumeMode Block.
func PVCRefusal(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.VolumeMode == nil || *pvc.Spec.VolumeMode != corev1.PersistentVolumeBlock {
		return fmt.Sprintf("PersistentVolumeClaim %s is volumeMode Filesystem; a hot-plugged volume must be volumeMode Block", pvc.Name)
	}
	return ""
}

// PVRefusal checks the volume the claim is bound to: a CSI volume, the only
// kind whose device kubelet publishes where the launcher reads it.
func PVRefusal(pv *corev1.PersistentVolume) string {
	if pv.Spec.CSI == nil {
		return fmt.Sprintf("PersistentVolume %s is not a CSI volume; hot-plugged volumes must be provisioned by a CSI driver", pv.Name)
	}
	return ""
}

// SiblingRefusal checks the attachment against the namespace's other
// attachments that are not being deleted and came before it (see
// SortAttachments): one attachment per guest and claim, at most
// MaxVolumeAttachmentsPerGuest per guest, and a claim on two guests only if
// it is ReadWriteMany and every attachment of it is read-only. pvc may be nil
// (not created yet), which skips the access-mode part.
func SiblingRefusal(sva *swiftv1alpha1.SwiftVolumeAttachment, earlier []swiftv1alpha1.SwiftVolumeAttachment, pvc *corev1.PersistentVolumeClaim) string {
	guest, claim := sva.Spec.GuestRef.Name, sva.Spec.PVCRef.Name
	perGuest := 0
	for i := range earlier {
		o := &earlier[i]
		if o.Spec.GuestRef.Name != guest {
			continue
		}
		perGuest++
		if o.Spec.PVCRef.Name == claim {
			return fmt.Sprintf("SwiftVolumeAttachment %s already attaches PersistentVolumeClaim %s to SwiftGuest %s", o.Name, claim, guest)
		}
	}
	if perGuest >= swiftv1alpha1.MaxVolumeAttachmentsPerGuest {
		return fmt.Sprintf("SwiftGuest %s already has %d volume attachments, the most a guest can have", guest, swiftv1alpha1.MaxVolumeAttachmentsPerGuest)
	}
	for i := range earlier {
		o := &earlier[i]
		if o.Spec.PVCRef.Name != claim || o.Spec.GuestRef.Name == guest {
			continue
		}
		if !sva.Spec.ReadOnly || !o.Spec.ReadOnly {
			return fmt.Sprintf("PersistentVolumeClaim %s is already attached to SwiftGuest %s by %s; a claim can be attached "+
				"to two guests only if it is ReadWriteMany and every attachment of it is readOnly", claim, o.Spec.GuestRef.Name, o.Name)
		}
		if pvc != nil && !slices.Contains(accessModes(pvc), corev1.ReadWriteMany) {
			return fmt.Sprintf("PersistentVolumeClaim %s is already attached to SwiftGuest %s by %s and is not ReadWriteMany",
				claim, o.Spec.GuestRef.Name, o.Name)
		}
	}
	return ""
}

func accessModes(pvc *corev1.PersistentVolumeClaim) []corev1.PersistentVolumeAccessMode {
	if len(pvc.Status.AccessModes) > 0 {
		return pvc.Status.AccessModes
	}
	return pvc.Spec.AccessModes
}

// SortAttachments orders attachments oldest first (creation time, then name),
// the order in which SiblingRefusal lets the earlier one win.
func SortAttachments(svas []swiftv1alpha1.SwiftVolumeAttachment) {
	sort.SliceStable(svas, func(i, j int) bool {
		a, b := svas[i].CreationTimestamp, svas[j].CreationTimestamp
		if !a.Equal(&b) {
			return a.Before(&b)
		}
		return svas[i].Name < svas[j].Name
	})
}

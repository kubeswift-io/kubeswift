package v1alpha1

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SwiftVolumeAttachment hot-plugs a Block PersistentVolumeClaim into a running
// SwiftGuest as a virtio-blk disk, without restarting it. The guest sees the
// disk at /dev/disk/by-id/virtio-<status.serial>. Deleting the attachment
// detaches the disk; the deletion waits until the guest no longer uses it.
// The attachment is not owned by the guest: when the guest is stopped or gone
// it waits (Pending) and the disk is plugged in again when the guest runs.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=sva
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Guest",type=string,JSONPath=`.spec.guestRef.name`
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=`.spec.pvcRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Serial",type=string,JSONPath=`.status.serial`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftVolumeAttachment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftVolumeAttachmentSpec   `json:"spec"`
	Status            SwiftVolumeAttachmentStatus `json:"status,omitempty"`
}

// SwiftVolumeAttachmentSpec is immutable: to change an attachment, delete it
// and create a new one.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; delete the attachment and create a new one"
type SwiftVolumeAttachmentSpec struct {
	// GuestRef is the SwiftGuest, in this namespace, to attach the volume to.
	// The guest must run on Cloud Hypervisor, have no GPU, and have
	// spec.guestAgent.enabled (the agent tells whether the guest still uses
	// the disk before it is detached).
	GuestRef corev1.LocalObjectReference `json:"guestRef"`

	// PVCRef is the PersistentVolumeClaim, in this namespace, to attach. It
	// must have volumeMode Block and be bound to a CSI PersistentVolume.
	PVCRef corev1.LocalObjectReference `json:"pvcRef"`

	// ReadOnly attaches the disk read-only. A claim may be attached to more
	// than one guest at a time only if it is ReadWriteMany and every
	// attachment of it is read-only.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// VolumeAttachmentPhase is the lifecycle phase of a SwiftVolumeAttachment.
// +kubebuilder:validation:Enum=Pending;Attaching;Attached;Detaching;Failed
type VolumeAttachmentPhase string

const (
	// VolumeAttachmentPending: waiting for something outside the attachment
	// (the guest to run, the claim to bind, a migration to finish). The
	// Ready condition's reason says what.
	VolumeAttachmentPending VolumeAttachmentPhase = "Pending"
	// VolumeAttachmentAttaching: the volume is being put on the guest's node
	// or plugged into the guest.
	VolumeAttachmentAttaching VolumeAttachmentPhase = "Attaching"
	// VolumeAttachmentAttached: the guest has the disk.
	VolumeAttachmentAttached VolumeAttachmentPhase = "Attached"
	// VolumeAttachmentDetaching: the attachment is being deleted.
	VolumeAttachmentDetaching VolumeAttachmentPhase = "Detaching"
	// VolumeAttachmentFailed: the launcher could not plug the disk in. The
	// Ready condition says why. The attachment is retried when the guest's
	// launcher changes; delete it to give up.
	VolumeAttachmentFailed VolumeAttachmentPhase = "Failed"
)

// SwiftVolumeAttachmentStatus is the observed state of a SwiftVolumeAttachment.
type SwiftVolumeAttachmentStatus struct {
	// Phase is the attachment's lifecycle phase.
	// +optional
	Phase VolumeAttachmentPhase `json:"phase,omitempty"`

	// Serial is the disk's virtio serial, as the guest sees it
	// (/dev/disk/by-id/virtio-<serial>). Derived from metadata.uid, so it is
	// unique and never changes.
	// +optional
	Serial string `json:"serial,omitempty"`

	// NodeName is the node the guest, and so the volume, is on.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// PVName is the PersistentVolume bound to spec.pvcRef when it was attached.
	// +optional
	PVName string `json:"pvName,omitempty"`

	// AttachmentPods are the pods that hold the volume on the guest's node.
	// The launcher uses the volume only through a pod listed here.
	// +optional
	AttachmentPods []VolumeAttachmentPod `json:"attachmentPods,omitempty"`

	// Conditions: Ready (the guest has the disk) and InUse (the guest still
	// uses it, which holds a detach).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the metadata.generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// VolumeAttachmentPod is one pod holding the volume on a node.
type VolumeAttachmentPod struct {
	// Name of the pod, in the attachment's namespace.
	Name string `json:"name"`
	// UID of the pod, as the controller created it.
	UID types.UID `json:"uid"`
	// NodeName is the node the pod is pinned to.
	NodeName string `json:"nodeName"`
}

// SwiftVolumeAttachmentList contains a list of SwiftVolumeAttachment.
// +kubebuilder:object:root=true
type SwiftVolumeAttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftVolumeAttachment `json:"items"`
}

// SwiftVolumeAttachment condition types and reasons.
const (
	// VolumeAttachmentConditionReady is True once the guest has the disk.
	VolumeAttachmentConditionReady = "Ready"
	// VolumeAttachmentConditionInUse is True while a detach is held because
	// the guest still mounts, holds open or swaps on the disk.
	VolumeAttachmentConditionInUse = "InUse"

	VolumeAttachmentReasonGuestNotFound       = "GuestNotFound"
	VolumeAttachmentReasonGuestNotRunning     = "GuestNotRunning"
	VolumeAttachmentReasonMigrationInProgress = "MigrationInProgress"
	VolumeAttachmentReasonLauncherUnsupported = "LauncherUnsupported"
	VolumeAttachmentReasonPVCNotFound         = "PVCNotFound"
	VolumeAttachmentReasonPVCNotBound         = "PVCNotBound"
	VolumeAttachmentReasonVolumeUnsupported   = "VolumeUnsupported"
	VolumeAttachmentReasonAttachingToNode     = "AttachingToNode"
	VolumeAttachmentReasonPlugging            = "Plugging"
	VolumeAttachmentReasonAttached            = "Attached"
	VolumeAttachmentReasonDetaching           = "Detaching"
	VolumeAttachmentReasonDetachBlocked       = "DetachBlocked"
	VolumeAttachmentReasonLauncherFailed      = "LauncherFailed"
	VolumeAttachmentReasonNotInUse            = "NotInUse"
)

const (
	// VolumeDetachFinalizer holds a SwiftVolumeAttachment until the guest
	// has released the disk and the attachment pods are gone.
	VolumeDetachFinalizer = "kubeswift.io/volume-detach"

	// AnnotationForceDetach on a SwiftVolumeAttachment, set to "true",
	// detaches the disk without asking the guest whether it still uses it.
	// For a guest whose agent no longer answers; data in use is lost.
	AnnotationForceDetach = "kubeswift.io/force-detach"

	// LabelVolumeAttachment names the SwiftVolumeAttachment on its
	// attachment pods.
	LabelVolumeAttachment = "kubeswift.io/volume-attachment"

	// MaxVolumeAttachmentsPerGuest caps the attachments of one guest. Cloud
	// Hypervisor has 31 PCI slots per segment and a guest already uses
	// several.
	MaxVolumeAttachmentsPerGuest = 16
)

// HotplugSerial is the virtio serial of an attachment's disk: "ks" and the
// first 18 hex digits of its UID. virtio-blk serials are at most 20 bytes.
func HotplugSerial(uid types.UID) string {
	hex := strings.ReplaceAll(string(uid), "-", "")
	if len(hex) > 18 {
		hex = hex[:18]
	}
	return "ks" + hex
}

// HotplugVolumeStatus is one hot-plugged volume as the guest's launcher
// reports it.
type HotplugVolumeStatus struct {
	// Name is the SwiftVolumeAttachment's name.
	Name string `json:"name"`
	// Serial is the disk's virtio serial.
	// +optional
	Serial string `json:"serial,omitempty"`
	// State is the launcher's state for the disk: Attaching, Attached,
	// Detaching, InUse or Failed.
	// +optional
	State string `json:"state,omitempty"`
	// Reason explains a Failed or InUse state.
	// +optional
	Reason string `json:"reason,omitempty"`
}

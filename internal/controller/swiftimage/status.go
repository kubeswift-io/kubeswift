package swiftimage

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
)

const (
	ConditionReady  = "Ready"
	ConditionFailed = "Failed"
	// ConditionSourceReady: a pvcClone source can be imported (exists, is
	// Bound, and no pod that has not finished uses it).
	ConditionSourceReady = "SourceReady"

	ReasonReady          = "Ready"
	ReasonImportFailed   = "ImportFailed"
	ReasonValidateFailed = "ValidateFailed"
	ReasonPrepareFailed  = "PrepareFailed"

	ReasonSourceNotFound = "SourceNotFound"
	ReasonSourceNotBound = "SourceNotBound"
	ReasonSourceInUse    = "SourceInUse"
	ReasonSourceReady    = "SourceReady"
)

// SetPhase updates status.phase.
func SetPhase(status *imagev1alpha1.SwiftImageStatus, phase imagev1alpha1.SwiftImagePhase) {
	status.Phase = phase
}

// SetPreparedArtifact sets status.preparedArtifact.
func SetPreparedArtifact(status *imagev1alpha1.SwiftImageStatus, pvcRef *imagev1alpha1.PVCObjectReference, format imagev1alpha1.DiskFormat, size *resource.Quantity) {
	status.PreparedArtifact = &imagev1alpha1.PreparedArtifactRef{
		PVCRef: pvcRef,
		Format: format,
		Size:   size,
	}
}

// SetReadyCondition sets Ready condition to True.
func SetReadyCondition(status *imagev1alpha1.SwiftImageStatus) {
	setCondition(status, ConditionReady, metav1.ConditionTrue, ReasonReady, "Image is ready")
}

// SetFailedCondition sets Failed condition with reason and message.
func SetFailedCondition(status *imagev1alpha1.SwiftImageStatus, reason, message string) {
	setCondition(status, ConditionFailed, metav1.ConditionTrue, reason, message)
}

func setCondition(status *imagev1alpha1.SwiftImageStatus, condType string, statusVal metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			// The transition time moves only on a transition: a wait retried
			// every few seconds keeps the time it started.
			if status.Conditions[i].Status != statusVal {
				status.Conditions[i].LastTransitionTime = now
			}
			status.Conditions[i].Status = statusVal
			status.Conditions[i].Reason = reason
			status.Conditions[i].Message = message
			return
		}
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             statusVal,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// SetSourceReadyCondition records whether a pvcClone source can be imported.
func SetSourceReadyCondition(status *imagev1alpha1.SwiftImageStatus, ready bool, reason, message string) {
	v := metav1.ConditionFalse
	if ready {
		v = metav1.ConditionTrue
	}
	setCondition(status, ConditionSourceReady, v, reason, message)
}

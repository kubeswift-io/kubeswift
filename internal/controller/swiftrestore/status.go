package swiftrestore

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// Condition reasons for SwiftRestore.
const (
	ReasonPending          = "Pending"
	ReasonDownloading      = "Downloading"
	ReasonRestoring        = "Restoring"
	ReasonResuming         = "Resuming"
	ReasonRestoreReady     = "RestoreReady"
	ReasonSnapshotNotReady = "SnapshotNotReady"
	ReasonSnapshotNotFound = "SnapshotNotFound"
	ReasonSourceGuestGone  = "SourceGuestGone"
	ReasonTargetConflict   = "TargetConflict"
	ReasonRestoreFailed    = "RestoreFailed"
)

func setPhase(status *snapshotv1alpha1.SwiftRestoreStatus, phase snapshotv1alpha1.SwiftRestorePhase) {
	status.Phase = phase
}

// setReadyCondition updates the Ready condition.
func setReadyCondition(status *snapshotv1alpha1.SwiftRestoreStatus, condStatus metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range status.Conditions {
		if status.Conditions[i].Type == snapshotv1alpha1.SwiftRestoreConditionReady {
			if status.Conditions[i].Status != condStatus ||
				status.Conditions[i].Reason != reason ||
				status.Conditions[i].Message != message {
				status.Conditions[i].Status = condStatus
				status.Conditions[i].Reason = reason
				status.Conditions[i].Message = message
				status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               snapshotv1alpha1.SwiftRestoreConditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

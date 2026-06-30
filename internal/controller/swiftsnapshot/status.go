package swiftsnapshot

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// Condition reasons for SwiftSnapshot.
const (
	ReasonPending            = "Pending"
	ReasonCapturing          = "Capturing"
	ReasonSnapshotReady      = "SnapshotReady"
	ReasonGuestNotFound      = "GuestNotFound"
	ReasonGuestNotReady      = "GuestNotReady"
	ReasonRootPVCNotFound    = "RootPVCNotFound"
	ReasonUnsupportedBackend = "UnsupportedBackend"
	ReasonSnapshotFailed     = "SnapshotFailed"
)

// setPhase updates status.phase.
func setPhase(status *snapshotv1alpha1.SwiftSnapshotStatus, phase snapshotv1alpha1.SwiftSnapshotPhase) {
	status.Phase = phase
}

// setReadyCondition sets the Ready condition with the given status/reason/message.
// SwiftSnapshot is required to expose Ready from day one.
func setReadyCondition(status *snapshotv1alpha1.SwiftSnapshotStatus, condStatus metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range status.Conditions {
		if status.Conditions[i].Type == snapshotv1alpha1.SwiftSnapshotConditionReady {
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
		Type:               snapshotv1alpha1.SwiftSnapshotConditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

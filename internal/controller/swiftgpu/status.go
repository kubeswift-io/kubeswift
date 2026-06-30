package swiftgpu

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// isGPUAllocated returns true when the GPUAllocated condition on the guest is True.
func isGPUAllocated(guest *swiftv1alpha1.SwiftGuest) bool {
	for _, c := range guest.Status.Conditions {
		if c.Type == swiftv1alpha1.ConditionGPUAllocated {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// hasGPUAllocatedReason returns true when the guest's GPUAllocated condition
// already carries the given reason — the transition gate for state-entry
// counters (count once per entry, not per retry tick).
func hasGPUAllocatedReason(guest *swiftv1alpha1.SwiftGuest, reason string) bool {
	for _, c := range guest.Status.Conditions {
		if c.Type == swiftv1alpha1.ConditionGPUAllocated {
			return c.Reason == reason
		}
	}
	return false
}

// setGPUAllocatedCondition upserts the GPUAllocated condition on status.
func setGPUAllocatedCondition(status *swiftv1alpha1.SwiftGuestStatus, ok bool, reason, message string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	cond := metav1.Condition{
		Type:               swiftv1alpha1.ConditionGPUAllocated,
		Status:             s,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	for i, c := range status.Conditions {
		if c.Type == swiftv1alpha1.ConditionGPUAllocated {
			status.Conditions[i] = cond
			return
		}
	}
	status.Conditions = append(status.Conditions, cond)
}

package swiftkernel

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kernelv1alpha1 "github.com/projectbeskar/kubeswift/api/kernel/v1alpha1"
)

const (
	ConditionReady  = "Ready"
	ConditionFailed = "Failed"

	ReasonReady         = "Ready"
	ReasonPullFailed    = "PullFailed"
	ReasonNoKernelNodes = "NoKernelNodes"
	ReasonPulling       = "Pulling"
)

// SetPhase updates status.phase.
func SetPhase(status *kernelv1alpha1.SwiftKernelStatus, phase kernelv1alpha1.SwiftKernelPhase) {
	status.Phase = phase
}

// SetReadyCondition sets Ready condition to True.
func SetReadyCondition(status *kernelv1alpha1.SwiftKernelStatus) {
	setCondition(status, ConditionReady, metav1.ConditionTrue, ReasonReady, "Kernel artifacts ready on all nodes")
}

// SetNoKernelNodesCondition sets Ready to False when no kernel-labeled nodes exist.
func SetNoKernelNodesCondition(status *kernelv1alpha1.SwiftKernelStatus) {
	setCondition(status, ConditionReady, metav1.ConditionFalse, ReasonNoKernelNodes, "No nodes labeled kubeswift.io/kernel-node=true found")
}

// SetNodesPullingCondition sets Ready to False while pulls are still in progress.
func SetNodesPullingCondition(status *kernelv1alpha1.SwiftKernelStatus) {
	setCondition(status, ConditionReady, metav1.ConditionFalse, ReasonPulling, "Kernel artifacts are being pulled to nodes")
}

// SetFailedCondition sets Failed condition with reason and message, optionally scoped to a node.
func SetFailedCondition(status *kernelv1alpha1.SwiftKernelStatus, reason, message string, failedNode string) {
	if failedNode != "" {
		message = fmt.Sprintf("node %s: %s", failedNode, message)
	}
	setCondition(status, ConditionFailed, metav1.ConditionTrue, reason, message)
}

func setCondition(status *kernelv1alpha1.SwiftKernelStatus, condType string, statusVal metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			status.Conditions[i].Status = statusVal
			status.Conditions[i].Reason = reason
			status.Conditions[i].Message = message
			status.Conditions[i].LastTransitionTime = now
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

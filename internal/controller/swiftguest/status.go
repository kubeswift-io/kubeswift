package swiftguest

import (
	"encoding/json"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

const (
	ConditionResolved     = "Resolved"
	ConditionPodScheduled = "PodScheduled"
)

// MapPodToStatus updates status from pod phase and conditions.
func MapPodToStatus(pod *corev1.Pod, status *swiftv1alpha1.SwiftGuestStatus) {
	if pod == nil {
		return
	}

	// Set network from pod annotation (guest IP discovered by swiftletd)
	if ip, ok := pod.Annotations[PodAnnotationGuestIP]; ok && ip != "" {
		if status.Network == nil {
			status.Network = &swiftv1alpha1.GuestNetworkStatus{}
		}
		status.Network.PrimaryIP = ip
		status.Network.Interface = "eth0"
		status.Network.Ready = true
	}

	// Set network interfaces from pod annotation (set by swiftletd lease poller)
	if raw, ok := pod.Annotations[PodAnnotationGuestInterfaces]; ok && raw != "" {
		var ifaces []swiftv1alpha1.GuestNetworkInterface
		if err := json.Unmarshal([]byte(raw), &ifaces); err == nil {
			if status.Network == nil {
				status.Network = &swiftv1alpha1.GuestNetworkStatus{}
			}
			status.Network.Interfaces = ifaces
		}
	}

	// Set runtime from pod annotation (set by swiftletd on socket ready)
	if pidStr, ok := pod.Annotations[PodAnnotationGuestRuntimePID]; ok && pidStr != "" {
		if pid, err := strconv.ParseInt(pidStr, 10, 64); err == nil {
			status.Runtime = &swiftv1alpha1.GuestRuntimeStatus{
				PID:        pid,
				Hypervisor: "cloud-hypervisor",
			}
		}
	}

	// Set console from pod annotation (set by swiftletd on socket ready)
	if socket, ok := pod.Annotations[PodAnnotationGuestSerialSocket]; ok && socket != "" {
		status.Console = &swiftv1alpha1.GuestConsoleStatus{
			SerialSocket: socket,
		}
	}

	// Set nodeName and podRef when scheduled
	if pod.Spec.NodeName != "" {
		status.NodeName = pod.Spec.NodeName
		status.PodRef = &corev1.ObjectReference{
			APIVersion: pod.APIVersion,
			Kind:       pod.Kind,
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			UID:        pod.UID,
		}
	} else {
		status.PodRef = nil
	}

	switch pod.Status.Phase {
	case corev1.PodRunning:
		status.Phase = swiftv1alpha1.SwiftGuestPhaseRunning
		SetPodScheduledCondition(status, pod, true, "")
	case corev1.PodFailed:
		status.Phase = swiftv1alpha1.SwiftGuestPhaseFailed
		reason, msg := podFailureReason(pod)
		SetPodScheduledCondition(status, pod, false, reason+": "+msg)
	case corev1.PodSucceeded:
		status.Phase = swiftv1alpha1.SwiftGuestPhaseStopped
		SetPodScheduledCondition(status, pod, true, "")
	case corev1.PodPending:
		unschedulable := findUnschedulableCondition(pod)
		if unschedulable != nil {
			status.Phase = swiftv1alpha1.SwiftGuestPhasePending
			SetPodScheduledCondition(status, pod, false, unschedulable.Reason+": "+unschedulable.Message)
		} else {
			status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
			SetPodScheduledCondition(status, pod, false, "Scheduling")
		}
	default:
		status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
		SetPodScheduledCondition(status, pod, false, "Scheduling")
	}
}

func podFailureReason(pod *corev1.Pod) (string, string) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			reason := string(cs.State.Terminated.Reason)
			if reason == "" {
				reason = "Failed"
			}
			return reason, cs.State.Terminated.Message
		}
	}
	reason := pod.Status.Reason
	if reason == "" {
		reason = "Failed"
	}
	return reason, ""
}

func findUnschedulableCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodScheduled &&
			pod.Status.Conditions[i].Status == corev1.ConditionFalse &&
			pod.Status.Conditions[i].Reason == corev1.PodReasonUnschedulable {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// SetResolvedCondition sets the Resolved condition.
func SetResolvedCondition(status *swiftv1alpha1.SwiftGuestStatus, ok bool, reason string) {
	cond := metav1.Condition{
		Type: ConditionResolved,
	}
	if ok {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Resolved"
		cond.Message = "Resolution succeeded"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ResolutionFailed"
		cond.Message = reason
	}
	setCondition(status, cond)
}

// SetPodScheduledCondition sets the PodScheduled condition.
func SetPodScheduledCondition(status *swiftv1alpha1.SwiftGuestStatus, pod *corev1.Pod, scheduled bool, message string) {
	cond := metav1.Condition{
		Type: ConditionPodScheduled,
	}
	if scheduled {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "PodScheduled"
		cond.Message = "Pod is scheduled and running"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PodNotScheduled"
		cond.Message = message
	}
	setCondition(status, cond)
}

func setCondition(status *swiftv1alpha1.SwiftGuestStatus, cond metav1.Condition) {
	cond.ObservedGeneration = 0 // Status has no generation; controller sets when updating
	now := metav1.Now()
	cond.LastTransitionTime = now

	found := false
	for i := range status.Conditions {
		if status.Conditions[i].Type == cond.Type {
			status.Conditions[i] = cond
			found = true
			break
		}
	}
	if !found {
		status.Conditions = append(status.Conditions, cond)
	}
}

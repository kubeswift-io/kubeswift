package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func TestMapPodToStatus_PendingScheduling(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest1", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(pod, status)

	if status.Phase != swiftv1alpha1.SwiftGuestPhaseScheduling {
		t.Errorf("phase = %v, want Scheduling", status.Phase)
	}
	if !hasCondition(status, ConditionPodScheduled, metav1.ConditionFalse) {
		t.Error("expected PodScheduled=False")
	}
}

func TestMapPodToStatus_PendingUnschedulable(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest1", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodScheduled,
					Status:  corev1.ConditionFalse,
					Reason:  corev1.PodReasonUnschedulable,
					Message: "0/1 nodes available: insufficient memory",
				},
			},
		},
	}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(pod, status)

	if status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("phase = %v, want Pending", status.Phase)
	}
	if !hasCondition(status, ConditionPodScheduled, metav1.ConditionFalse) {
		t.Error("expected PodScheduled=False")
	}
}

func TestMapPodToStatus_Running(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest1", Namespace: "default", UID: "pod-uid"},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(pod, status)

	if status.Phase != swiftv1alpha1.SwiftGuestPhaseRunning {
		t.Errorf("phase = %v, want Running", status.Phase)
	}
	if status.NodeName != "node-1" {
		t.Errorf("nodeName = %q, want node-1", status.NodeName)
	}
	if status.PodRef == nil || status.PodRef.Name != "guest1" {
		t.Errorf("podRef = %v, want name guest1", status.PodRef)
	}
	if !hasCondition(status, ConditionPodScheduled, metav1.ConditionTrue) {
		t.Error("expected PodScheduled=True")
	}
}

func TestMapPodToStatus_Failed(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest1", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:  "OOMKilled",
							Message: "out of memory",
						},
					},
				},
			},
		},
	}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(pod, status)

	if status.Phase != swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Errorf("phase = %v, want Failed", status.Phase)
	}
	if !hasCondition(status, ConditionPodScheduled, metav1.ConditionFalse) {
		t.Error("expected PodScheduled=False")
	}
}

func TestMapPodToStatus_Succeeded(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest1", Namespace: "default"},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
		},
	}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(pod, status)

	if status.Phase != swiftv1alpha1.SwiftGuestPhaseStopped {
		t.Errorf("phase = %v, want Stopped", status.Phase)
	}
	if !hasCondition(status, ConditionPodScheduled, metav1.ConditionTrue) {
		t.Error("expected PodScheduled=True")
	}
}

func TestMapPodToStatus_NilPod(t *testing.T) {
	status := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseRunning}
	MapPodToStatus(nil, status)
	// Should not modify status
	if status.Phase != swiftv1alpha1.SwiftGuestPhaseRunning {
		t.Errorf("phase = %v, want Running (unchanged)", status.Phase)
	}
}

func hasCondition(status *swiftv1alpha1.SwiftGuestStatus, condType string, condStatus metav1.ConditionStatus) bool {
	for _, c := range status.Conditions {
		if c.Type == condType && c.Status == condStatus {
			return true
		}
	}
	return false
}

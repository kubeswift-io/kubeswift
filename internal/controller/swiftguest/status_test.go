package swiftguest

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
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
		Spec:       corev1.PodSpec{NodeName: "node-1"},
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

func TestMapPodToStatus_WithGuestIPAnnotation(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "guest1",
			Namespace:   "default",
			Annotations: map[string]string{PodAnnotationGuestIP: "10.244.1.12"},
		},
		Spec:   corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(pod, status)

	if status.Network == nil {
		t.Fatal("status.Network = nil, want set")
	}
	if status.Network.PrimaryIP != "10.244.1.12" {
		t.Errorf("status.Network.PrimaryIP = %q, want 10.244.1.12", status.Network.PrimaryIP)
	}
	if status.Network.Interface != "eth0" {
		t.Errorf("status.Network.Interface = %q, want eth0", status.Network.Interface)
	}
	if !status.Network.Ready {
		t.Error("status.Network.Ready = false, want true")
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

// #527: the whole point of this condition is that "Running with no IP forever"
// used to be indistinguishable from "Running, still booting". These pin the
// three states apart.

func networkTestGuest(diskBoot, withSeed bool) *swiftv1alpha1.SwiftGuest {
	g := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
	}
	if diskBoot {
		g.Spec.ImageRef = &corev1.LocalObjectReference{Name: "img"}
	} else {
		g.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	}
	if withSeed {
		g.Spec.SeedProfileRef = &corev1.LocalObjectReference{Name: "seed"}
	}
	return g
}

func networkCond(status *swiftv1alpha1.SwiftGuestStatus) *metav1.Condition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == swiftv1alpha1.ConditionNetworkReady {
			return &status.Conditions[i]
		}
	}
	return nil
}

// A booting guest must NOT look broken. Without this guard the obvious
// implementation ("no IP => NetworkReady=False") would mark every guest failed
// for the first minute of its life.
func TestMapNetworkReadyCondition_SilentWhileStillBooting(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapNetworkReadyCondition(networkTestGuest(true, true), pod, status)
	if c := networkCond(status); c != nil {
		t.Errorf("no IP and no timeout report yet is not a failure; got %s/%s", c.Status, c.Reason)
	}
}

func TestMapNetworkReadyCondition_TrueOnceIPAcquired(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}}
	status := &swiftv1alpha1.SwiftGuestStatus{
		Network: &swiftv1alpha1.GuestNetworkStatus{PrimaryIP: "10.0.0.5"},
	}
	MapNetworkReadyCondition(networkTestGuest(true, true), pod, status)
	c := networkCond(status)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "IPAcquired" {
		t.Fatalf("want NetworkReady=True/IPAcquired; got %+v", c)
	}
}

// The condition must RECOVER. A lease can land after the poller already reported
// a timeout (late DHCP, or an operator fixing the guest in place); latching
// False forever would be a new silent lie in the opposite direction.
func TestMapNetworkReadyCondition_RecoversWhenLeaseArrivesLate(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "g", Namespace: "ns",
		Annotations: map[string]string{
			PodAnnotationNetworkUnready: `{"reason":"DHCPTimeout","afterSeconds":240}`,
		},
	}}
	status := &swiftv1alpha1.SwiftGuestStatus{
		Network: &swiftv1alpha1.GuestNetworkStatus{PrimaryIP: "10.0.0.5"},
	}
	MapNetworkReadyCondition(networkTestGuest(true, true), pod, status)
	c := networkCond(status)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("an IP must win over a stale timeout report; got %+v", c)
	}
}

// The message is the deliverable. A bare "no DHCP lease" sends the operator to
// the launcher logs and a serial console — which is exactly the cost this change
// exists to remove.
func TestMapNetworkReadyCondition_NamesTheMissingSeed(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "g", Namespace: "ns",
		Annotations: map[string]string{
			PodAnnotationNetworkUnready: `{"reason":"DHCPTimeout","afterSeconds":240}`,
		},
	}}

	// disk-boot, no seed: the cause is knowable from the spec, so say it.
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapNetworkReadyCondition(networkTestGuest(true, false), pod, status)
	c := networkCond(status)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "DHCPTimeout" {
		t.Fatalf("want NetworkReady=False/DHCPTimeout; got %+v", c)
	}
	if !strings.Contains(c.Message, "240s") {
		t.Errorf("message should carry how long it waited; got %q", c.Message)
	}
	if !strings.Contains(c.Message, "seedProfileRef") {
		t.Errorf("disk-boot with no seed MUST name seedProfileRef — that is the fix; got %q", c.Message)
	}

	// disk-boot WITH a seed: the seed hint would be wrong, so it must not appear.
	status = &swiftv1alpha1.SwiftGuestStatus{}
	MapNetworkReadyCondition(networkTestGuest(true, true), pod, status)
	if c := networkCond(status); c == nil || strings.Contains(c.Message, "seedProfileRef") {
		t.Errorf("a guest that HAS a seed must not be told to set one; got %+v", c)
	}

	// kernel-boot: no cloud-init involved, so the seed hint is irrelevant.
	status = &swiftv1alpha1.SwiftGuestStatus{}
	MapNetworkReadyCondition(networkTestGuest(false, false), pod, status)
	if c := networkCond(status); c == nil || strings.Contains(c.Message, "seedProfileRef") {
		t.Errorf("kernel-boot has no cloud-init; the seed hint must not appear; got %+v", c)
	}
}

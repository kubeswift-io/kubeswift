package swiftguest

import (
	"testing"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A real OVN-K pod-networks annotation: the "default" entry is infrastructure-locked
// (the cluster default, not the guest's IP); the UDN entry carries the guest's IP.
const ovnPodNetworksFixture = `{"default":{"ip_addresses":["10.244.1.36/24"],"role":"infrastructure-locked"},` +
	`"model-a/model-a-udn":{"ip_addresses":["10.50.0.26/16"],"role":"primary","gateway_ips":["10.50.0.1"]}}`

// Model A: swiftletd writes no guest-ip annotation (no apiserver), so the controller
// derives the guest IP from the OVN annotation (the non-default entry) and GuestRunning
// from launcher readiness.
func TestMapPodToStatus_ModelA_DerivesIPFromOVNAnnotation(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "g", Namespace: "model-a",
			Annotations: map[string]string{
				PodAnnotationPrimaryUDNIface: "ovn-udn1",
				OVNPodNetworksAnnotation:     ovnPodNetworksFixture,
			},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(pod, status)

	if status.Network == nil || status.Network.PrimaryIP != "10.50.0.26" {
		t.Fatalf("primaryIP = %+v, want 10.50.0.26 (the UDN entry, not the infra-locked default)", status.Network)
	}
	if status.Network.Interface != "ovn-udn1" {
		t.Errorf("interface = %q, want ovn-udn1", status.Network.Interface)
	}
	if gr := findCondition(status, "GuestRunning"); gr == nil || gr.Status != metav1.ConditionTrue {
		t.Errorf("GuestRunning = %v, want True (launcher ready)", gr)
	}
}

func TestMapPodToStatus_ModelA_GuestRunningFalseWhenNotReady(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "g", Namespace: "model-a",
			Annotations: map[string]string{
				PodAnnotationPrimaryUDNIface: "ovn-udn1",
				OVNPodNetworksAnnotation:     ovnPodNetworksFixture,
			},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(pod, status)
	if gr := findCondition(status, "GuestRunning"); gr == nil || gr.Status != metav1.ConditionFalse {
		t.Errorf("GuestRunning = %v, want False (launcher not ready)", gr)
	}
}

// Regression: a non-Model-A guest (no marker) is untouched — the controller derives
// neither the IP nor GuestRunning; swiftletd owns those via its annotations.
func TestMapPodToStatus_NonModelA_NoDerivation(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "g", Namespace: "default",
			Annotations: map[string]string{OVNPodNetworksAnnotation: ovnPodNetworksFixture},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(pod, status)
	if status.Network != nil && status.Network.PrimaryIP != "" {
		t.Errorf("non-Model-A primaryIP = %q, want empty (no marker)", status.Network.PrimaryIP)
	}
	if gr := findCondition(status, "GuestRunning"); gr != nil {
		t.Errorf("non-Model-A GuestRunning set by controller = %v, want nil (swiftletd owns it)", gr)
	}
}

func TestApplyPrimaryUDN_StampsMarkerAndProbe(t *testing.T) {
	rg := &resolved.ResolvedGuest{PrimaryUDNInterface: "ovn-udn1", Meta: resolved.Meta{Namespace: "model-a", Name: "g"}}
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Namespace: "model-a", Name: "g"}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "launcher"}}}}
	applyPrimaryUDN(pod, guest, rg)

	if pod.Annotations[PodAnnotationPrimaryUDNIface] != "ovn-udn1" {
		t.Errorf("marker = %q, want ovn-udn1", pod.Annotations[PodAnnotationPrimaryUDNIface])
	}
	rp := pod.Spec.Containers[0].ReadinessProbe
	if rp == nil || rp.Exec == nil {
		t.Fatal("expected a CH-socket readiness probe on the launcher")
	}
	want := "test -S " + RunDirPath + "/model-a-g/ch.sock"
	if got := rp.Exec.Command[len(rp.Exec.Command)-1]; got != want {
		t.Errorf("probe cmd = %q, want %q", got, want)
	}
}

// A Model A guest that already has a service-port readiness probe keeps it (stronger
// signal); applyPrimaryUDN does not overwrite it.
func TestApplyPrimaryUDN_KeepsServicePortProbe(t *testing.T) {
	rg := &resolved.ResolvedGuest{PrimaryUDNInterface: "ovn-udn1", Meta: resolved.Meta{Namespace: "model-a", Name: "g"}}
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Namespace: "model-a", Name: "g"}}
	existing := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{}}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "launcher", ReadinessProbe: existing}}}}
	applyPrimaryUDN(pod, guest, rg)
	if pod.Spec.Containers[0].ReadinessProbe != existing {
		t.Error("service-port readiness probe was overwritten")
	}
}

func TestApplyPrimaryUDN_NoopNonModelA(t *testing.T) {
	rg := &resolved.ResolvedGuest{Meta: resolved.Meta{Namespace: "default", Name: "g"}}
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "g"}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "launcher"}}}}
	applyPrimaryUDN(pod, guest, rg)
	if _, ok := pod.Annotations[PodAnnotationPrimaryUDNIface]; ok {
		t.Error("non-Model-A pod got the marker")
	}
	if pod.Spec.Containers[0].ReadinessProbe != nil {
		t.Error("non-Model-A pod got a readiness probe")
	}
}

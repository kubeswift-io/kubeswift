package gateway

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func TestGuestToProto(t *testing.T) {
	g := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vm-a",
			Namespace: "default",
			Labels:    map[string]string{"swift.kubeswift.io/pool": "web"},
		},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "ubuntu-noble"},
			GuestClassRef: corev1.LocalObjectReference{Name: "default"},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{
			Phase:    swiftv1alpha1.SwiftGuestPhaseRunning,
			NodeName: "miles",
			Runtime:  &swiftv1alpha1.GuestRuntimeStatus{Hypervisor: "cloud-hypervisor"},
			Network:  &swiftv1alpha1.GuestNetworkStatus{PrimaryIP: "192.168.99.11"},
			Conditions: []metav1.Condition{
				{Type: "GuestRunning", Status: metav1.ConditionTrue},
			},
		},
	}
	got := guestToProto("boba", g)
	if got.GetRef().GetCluster() != "boba" || got.GetRef().GetNamespace() != "default" || got.GetRef().GetName() != "vm-a" {
		t.Errorf("ref wrong: %+v", got.GetRef())
	}
	if got.Phase != "Running" {
		t.Errorf("Phase = %q", got.Phase)
	}
	if got.NodeName != "miles" {
		t.Errorf("NodeName = %q", got.NodeName)
	}
	if got.Hypervisor != "cloud-hypervisor" {
		t.Errorf("Hypervisor = %q", got.Hypervisor)
	}
	if got.PrimaryIp != "192.168.99.11" {
		t.Errorf("PrimaryIp = %q", got.PrimaryIp)
	}
	if got.GuestClass != "default" {
		t.Errorf("GuestClass = %q", got.GuestClass)
	}
	if got.BootSource != "image/ubuntu-noble" {
		t.Errorf("BootSource = %q", got.BootSource)
	}
	if !got.GuestRunning {
		t.Error("GuestRunning = false, want true")
	}
	if got.Labels["swift.kubeswift.io/pool"] != "web" {
		t.Errorf("labels not copied: %v", got.Labels)
	}
}

func TestBootSource(t *testing.T) {
	cases := []struct {
		name string
		spec swiftv1alpha1.SwiftGuestSpec
		want string
	}{
		{"image", swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}}, "image/img"},
		{"kernel", swiftv1alpha1.SwiftGuestSpec{KernelRef: &corev1.LocalObjectReference{Name: "k"}}, "kernel/k"},
		{"clone", swiftv1alpha1.SwiftGuestSpec{CloneFromSnapshot: &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "snap"}}}, "clone/snap"},
		{"none", swiftv1alpha1.SwiftGuestSpec{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &swiftv1alpha1.SwiftGuest{Spec: tc.spec}
			if got := bootSource(g); got != tc.want {
				t.Errorf("bootSource = %q, want %q", got, tc.want)
			}
		})
	}
}

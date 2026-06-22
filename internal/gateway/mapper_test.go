package gateway

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	fleetv1alpha1 "github.com/projectbeskar/kubeswift/api/fleet/v1alpha1"
)

func TestClusterToProto(t *testing.T) {
	now := metav1.NewTime(time.Now())
	c := &fleetv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "boba", Namespace: "kubeswift-system"},
		Spec:       fleetv1alpha1.ClusterSpec{Server: "https://10.0.0.1:6443"},
		Status: fleetv1alpha1.ClusterStatus{
			KubernetesVersion: "v1.34.3",
			GuestCount:        ptr.To(int32(7)),
			LastConnected:     &now,
			Conditions: []metav1.Condition{
				{Type: fleetv1alpha1.ClusterConditionReady, Status: metav1.ConditionTrue, LastTransitionTime: now},
				{Type: fleetv1alpha1.ClusterConditionReachable, Status: metav1.ConditionFalse},
			},
		},
	}
	got := clusterToProto(c)
	if got.Name != "boba" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.DisplayName != "boba" {
		t.Errorf("DisplayName fallback = %q, want boba", got.DisplayName)
	}
	if got.Server != "https://10.0.0.1:6443" {
		t.Errorf("Server = %q", got.Server)
	}
	if !got.Ready {
		t.Error("Ready = false, want true (Ready condition is True)")
	}
	if got.Reachable {
		t.Error("Reachable = true, want false (Reachable condition is False)")
	}
	if got.GuestCount != 7 {
		t.Errorf("GuestCount = %d, want 7", got.GuestCount)
	}
	if got.KubernetesVersion != "v1.34.3" {
		t.Errorf("KubernetesVersion = %q", got.KubernetesVersion)
	}
	if got.LastConnected == nil {
		t.Error("LastConnected = nil, want set")
	}
	if len(got.Conditions) != 2 {
		t.Errorf("Conditions = %d, want 2", len(got.Conditions))
	}
}

func TestClusterToProto_ExplicitDisplayNameAndNoCounts(t *testing.T) {
	c := &fleetv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "boba"},
		Spec:       fleetv1alpha1.ClusterSpec{DisplayName: "Boba (lab)"},
	}
	got := clusterToProto(c)
	if got.DisplayName != "Boba (lab)" {
		t.Errorf("DisplayName = %q, want Boba (lab)", got.DisplayName)
	}
	// No status: ready/reachable false, guestCount 0, lastConnected nil.
	if got.Ready || got.Reachable || got.GuestCount != 0 || got.LastConnected != nil {
		t.Errorf("empty-status mapping wrong: %+v", got)
	}
}

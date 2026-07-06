package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGroupVersionConstants(t *testing.T) {
	if GroupName != "fleet.kubeswift.io" {
		t.Errorf("GroupName = %q, want fleet.kubeswift.io", GroupName)
	}
	if Version != "v1alpha1" {
		t.Errorf("Version = %q, want v1alpha1", Version)
	}
}

func TestClusterConditionConstants(t *testing.T) {
	if ClusterConditionReady != "Ready" {
		t.Errorf("ClusterConditionReady = %q, want Ready", ClusterConditionReady)
	}
	if ClusterConditionReachable != "Reachable" {
		t.Errorf("ClusterConditionReachable = %q, want Reachable", ClusterConditionReachable)
	}
}

// TestClusterDeepCopy guards that the generated DeepCopy produces a fully
// independent object — pointer (GuestCount) and slice (Conditions) fields must
// not alias the original, or the gateway's per-cluster status writes would
// race the informer cache.
func TestClusterDeepCopy(t *testing.T) {
	count := int32(3)
	orig := &Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "boba", Namespace: "kubeswift-system"},
		Spec: ClusterSpec{
			Server:              "https://10.0.0.1:6443",
			CredentialSecretRef: &corev1.LocalObjectReference{Name: "boba-kubeconfig"},
			PrometheusEndpoint:  "http://prometheus.boba:9090",
			DisplayName:         "Boba (lab)",
		},
		Status: ClusterStatus{
			KubernetesVersion: "v1.34.3",
			GuestCount:        &count,
			Conditions: []metav1.Condition{
				{Type: ClusterConditionReady, Status: metav1.ConditionTrue},
			},
		},
	}

	cp := orig.DeepCopy()
	if cp == orig {
		t.Fatal("DeepCopy returned the same pointer")
	}
	*cp.Status.GuestCount = 99
	cp.Status.Conditions[0].Status = metav1.ConditionFalse
	cp.Spec.Server = "https://changed"

	if *orig.Status.GuestCount != 3 {
		t.Errorf("GuestCount leaked through DeepCopy: got %d, want 3", *orig.Status.GuestCount)
	}
	if orig.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("Conditions leaked through DeepCopy: got %v", orig.Status.Conditions[0].Status)
	}
	if orig.Spec.Server != "https://10.0.0.1:6443" {
		t.Errorf("Spec.Server leaked through DeepCopy: got %q", orig.Spec.Server)
	}
}

func TestClusterListDeepCopyObject(t *testing.T) {
	list := &ClusterList{Items: []Cluster{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}}}
	obj := list.DeepCopyObject()
	cp, ok := obj.(*ClusterList)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *ClusterList", obj)
	}
	if len(cp.Items) != 1 || cp.Items[0].Name != "a" {
		t.Errorf("ClusterList DeepCopyObject lost items: %+v", cp.Items)
	}
}

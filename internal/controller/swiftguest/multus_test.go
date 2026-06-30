package swiftguest

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func TestBuildMultusAnnotation_NoInterfaces(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: nil,
		},
	}
	got := BuildMultusAnnotation(guest)
	if got != "" {
		t.Errorf("BuildMultusAnnotation(nil interfaces) = %q, want empty", got)
	}
}

func TestBuildMultusAnnotation_PrimaryOnly(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt", NetworkRef: nil},
			},
		},
	}
	got := BuildMultusAnnotation(guest)
	if got != "" {
		t.Errorf("BuildMultusAnnotation(primary only) = %q, want empty", got)
	}
}

func TestBuildMultusAnnotation_OneSecondary(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt", NetworkRef: nil},
				{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{
					Name: "sriov-net",
				}},
			},
		},
	}
	got := BuildMultusAnnotation(guest)
	if got == "" {
		t.Fatal("BuildMultusAnnotation returned empty, want JSON")
	}

	var entries []multusNetworkEntry
	if err := json.Unmarshal([]byte(got), &entries); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Name != "sriov-net" {
		t.Errorf("entry name = %q, want sriov-net", entries[0].Name)
	}
	if entries[0].Interface != "net1" {
		t.Errorf("entry interface = %q, want net1", entries[0].Interface)
	}
	if entries[0].Namespace != "default" {
		t.Errorf("entry namespace = %q, want default", entries[0].Namespace)
	}
}

func TestBuildMultusAnnotation_MultipleSecondary(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "mgmt", NetworkRef: nil},
				{Name: "data1", NetworkRef: &swiftv1alpha1.NetworkReference{
					Name: "net-a",
				}},
				{Name: "data2", NetworkRef: &swiftv1alpha1.NetworkReference{
					Name: "net-b",
				}},
			},
		},
	}
	got := BuildMultusAnnotation(guest)
	if got == "" {
		t.Fatal("BuildMultusAnnotation returned empty, want JSON")
	}

	var entries []multusNetworkEntry
	if err := json.Unmarshal([]byte(got), &entries); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Interface != "net1" {
		t.Errorf("entries[0].Interface = %q, want net1", entries[0].Interface)
	}
	if entries[1].Interface != "net2" {
		t.Errorf("entries[1].Interface = %q, want net2", entries[1].Interface)
	}
}

func TestBuildMultusAnnotation_NamespaceDefault(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "myns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{
					Name: "sriov-net",
					// Namespace intentionally omitted
				}},
			},
		},
	}
	got := BuildMultusAnnotation(guest)

	var entries []multusNetworkEntry
	if err := json.Unmarshal([]byte(got), &entries); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Namespace != "myns" {
		t.Errorf("namespace = %q, want myns (guest namespace)", entries[0].Namespace)
	}
}

func TestBuildMultusAnnotation_ExplicitNamespace(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{
					Name:      "sriov-net",
					Namespace: "infra",
				}},
			},
		},
	}
	got := BuildMultusAnnotation(guest)

	var entries []multusNetworkEntry
	if err := json.Unmarshal([]byte(got), &entries); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Namespace != "infra" {
		t.Errorf("namespace = %q, want infra", entries[0].Namespace)
	}
}

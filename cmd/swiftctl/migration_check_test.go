package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:    corev1.NodeSystemInfo{Architecture: "amd64"},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("32Gi")},
		},
	}
}

func TestMigratePreflight_PrimaryOnNAD_NoBlockers(t *testing.T) {
	migrateTargetNode = "miles"
	migrateAllowIPChange = false
	defer func() { migrateTargetNode = ""; migrateAllowIPChange = false }()

	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			GuestClassRef: corev1.LocalObjectReference{Name: "cls"},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "app", Primary: true, NetworkRef: &swiftv1alpha1.NetworkReference{Name: "ovn-l2"}},
			},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseRunning, NodeName: "boba"},
	}
	class := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("4Gi")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(guest, class, readyNode("miles"), readyNode("boba")).Build()

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	err := runMigratePreflight(cmd, c, "g", "default", migrationv1alpha1.SwiftMigrationModeAuto)
	if err != nil {
		t.Fatalf("expected no blockers, got %v\n%s", err, buf.String())
	}
	s := buf.String()
	for _, want := range []string{"SwiftGuest default/g found", "is Ready", "primary IP rides a multi-node NAD", "no blockers"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestMigratePreflight_LiveWithSRIOV_IsBlocker(t *testing.T) {
	migrateTargetNode = "miles"
	defer func() { migrateTargetNode = "" }()

	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			GuestClassRef: corev1.LocalObjectReference{Name: "cls"},
			Interfaces:    []swiftv1alpha1.GuestInterface{{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV}},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseRunning, NodeName: "boba"},
	}
	class := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("4Gi")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(guest, class, readyNode("miles"), readyNode("boba")).Build()

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	err := runMigratePreflight(cmd, c, "g", "default", migrationv1alpha1.SwiftMigrationModeLive)
	if err == nil {
		t.Fatalf("expected a blocker for live+SR-IOV; output:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "not supported; use --preferred-mode offline") {
		t.Errorf("missing SR-IOV blocker message:\n%s", buf.String())
	}
}

func TestMigratePreflight_GuestNotFound(t *testing.T) {
	migrateTargetNode = "miles"
	defer func() { migrateTargetNode = "" }()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := runMigratePreflight(cmd, c, "nope", "default", migrationv1alpha1.SwiftMigrationModeAuto); err == nil {
		t.Fatal("expected error for missing guest")
	}
}

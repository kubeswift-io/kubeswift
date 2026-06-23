package actions

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func uGuest(ns, name, policy string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"guestClassRef": map[string]interface{}{"name": "default"},
		"imageRef":      map[string]interface{}{"name": "ubuntu-noble"},
	}
	if policy != "" {
		spec["runPolicy"] = policy
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "swift.kubeswift.io/v1alpha1",
		"kind":       "SwiftGuest",
		"metadata":   map[string]interface{}{"namespace": ns, "name": name},
		"spec":       spec,
	}}
}

func uPod(ns, name, guest string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"namespace": ns, "name": name,
			"labels": map[string]interface{}{GuestLabel: guest},
		},
	}}
}

func fakeDyn(objs ...*unstructured.Unstructured) dynamic.Interface {
	ro := make([]runtime.Object, len(objs))
	for i, o := range objs {
		ro[i] = o
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			SwiftGuestGVR:     "SwiftGuestList",
			PodGVR:            "PodList",
			SwiftMigrationGVR: "SwiftMigrationList",
		}, ro...)
}

func runPolicyOf(t *testing.T, dyn dynamic.Interface, ns, name string) string {
	t.Helper()
	u, err := dyn.Resource(SwiftGuestGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get guest: %v", err)
	}
	p, _, _ := unstructured.NestedString(u.Object, "spec", "runPolicy")
	return p
}

func podCount(t *testing.T, dyn dynamic.Interface, ns string) int {
	t.Helper()
	l, err := dyn.Resource(PodGVR).Namespace(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return len(l.Items)
}

// Start patches runPolicy=Running and leaves the launcher pod alone (the
// controller recreates a stopped one; a running one keeps running).
func TestStart_PatchesRunningKeepsPod(t *testing.T) {
	dyn := fakeDyn(uGuest("default", "vm-a", "Stopped"), uPod("default", "vm-a", "vm-a"))

	u, err := Start(context.Background(), dyn, "default", "vm-a")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if u.GetName() != "vm-a" {
		t.Errorf("Start returned %q, want vm-a", u.GetName())
	}
	if got := runPolicyOf(t, dyn, "default", "vm-a"); got != RunPolicyRunning {
		t.Errorf("runPolicy=%q, want Running", got)
	}
	if got := podCount(t, dyn, "default"); got != 1 {
		t.Errorf("Start deleted the launcher pod: %d pods, want 1", got)
	}
}

// Stop patches runPolicy=Stopped AND deletes the launcher pod — both halves are
// load-bearing (the stop guard is reactive; PR #267).
func TestStop_PatchesStoppedAndDeletesPod(t *testing.T) {
	dyn := fakeDyn(uGuest("default", "vm-a", "Running"), uPod("default", "vm-a", "vm-a"))

	if _, err := Stop(context.Background(), dyn, "default", "vm-a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := runPolicyOf(t, dyn, "default", "vm-a"); got != RunPolicyStopped {
		t.Errorf("runPolicy=%q, want Stopped", got)
	}
	if got := podCount(t, dyn, "default"); got != 0 {
		t.Errorf("Stop left %d launcher pods, want 0", got)
	}
}

// Stop selects the pod by the guest label, so it stops a live-migrated guest
// whose pod was renamed <guest>-mig-<uid>.
func TestStop_DeletesRenamedMigrationPod(t *testing.T) {
	dyn := fakeDyn(uGuest("default", "vm-a", "Running"), uPod("default", "vm-a-mig-abcd", "vm-a"))

	if _, err := Stop(context.Background(), dyn, "default", "vm-a"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := podCount(t, dyn, "default"); got != 0 {
		t.Errorf("Stop left %d launcher pods, want 0 (renamed pod not selected by label)", got)
	}
}

// Stop with no launcher pod is success — the VM is already stopped.
func TestStop_NoPodIsSuccess(t *testing.T) {
	dyn := fakeDyn(uGuest("default", "vm-a", "Running"))
	if _, err := Stop(context.Background(), dyn, "default", "vm-a"); err != nil {
		t.Fatalf("Stop with no pod should succeed: %v", err)
	}
	if got := runPolicyOf(t, dyn, "default", "vm-a"); got != RunPolicyStopped {
		t.Errorf("runPolicy=%q, want Stopped", got)
	}
}

func TestMigrate_CreatesSwiftMigration(t *testing.T) {
	dyn := fakeDyn(uGuest("default", "vm-a", "Running"))

	created, err := Migrate(context.Background(), dyn, MigrateParams{
		Namespace:     "default",
		GuestName:     "vm-a",
		TargetNode:    "miles",
		Mode:          "offline",
		AllowIPChange: true,
		Reason:        "test",
		Timeout:       10 * time.Minute,
		TTL:           time.Hour,
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	migs, err := dyn.Resource(SwiftMigrationGVR).Namespace("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(migs.Items) != 1 {
		t.Fatalf("want 1 SwiftMigration, got %d", len(migs.Items))
	}
	m := migs.Items[0]
	if m.GetName() != created.GetName() {
		t.Errorf("created name %q != listed %q", created.GetName(), m.GetName())
	}
	guestName, _, _ := unstructured.NestedString(m.Object, "spec", "guestRef", "name")
	node, _, _ := unstructured.NestedString(m.Object, "spec", "target", "nodeName")
	mode, _, _ := unstructured.NestedString(m.Object, "spec", "mode")
	allowIP, _, _ := unstructured.NestedBool(m.Object, "spec", "allowIPChange")
	reason, _, _ := unstructured.NestedString(m.Object, "spec", "reason")
	timeout, _, _ := unstructured.NestedString(m.Object, "spec", "timeout")
	ttl, _, _ := unstructured.NestedString(m.Object, "spec", "ttl")
	if guestName != "vm-a" || node != "miles" || mode != "offline" || !allowIP || reason != "test" {
		t.Errorf("spec wrong: guest=%q node=%q mode=%q allowIP=%v reason=%q", guestName, node, mode, allowIP, reason)
	}
	if timeout != "10m0s" || ttl != "1h0m0s" {
		t.Errorf("duration spec wrong: timeout=%q ttl=%q", timeout, ttl)
	}
}

// An empty mode resolves to auto; an empty Name+GenerateName defaults the
// generateName prefix; timeout/ttl are omitted (so the CRD default applies).
func TestMigrate_DefaultsModeAndName(t *testing.T) {
	dyn := fakeDyn(uGuest("default", "vm-a", "Running"))

	if _, err := Migrate(context.Background(), dyn, MigrateParams{
		Namespace:  "default",
		GuestName:  "vm-a",
		TargetNode: "miles",
	}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	migs, _ := dyn.Resource(SwiftMigrationGVR).Namespace("default").List(context.Background(), metav1.ListOptions{})
	if len(migs.Items) != 1 {
		t.Fatalf("want 1 SwiftMigration, got %d", len(migs.Items))
	}
	m := migs.Items[0]
	mode, _, _ := unstructured.NestedString(m.Object, "spec", "mode")
	if mode != "auto" {
		t.Errorf("empty mode should default to auto, got %q", mode)
	}
	if gn := m.GetGenerateName(); gn != "vm-a-migrate-" {
		t.Errorf("generateName=%q, want vm-a-migrate-", gn)
	}
	if _, found, _ := unstructured.NestedString(m.Object, "spec", "timeout"); found {
		t.Error("timeout should be omitted when zero (CRD default applies)")
	}
	if _, found, _ := unstructured.NestedString(m.Object, "spec", "ttl"); found {
		t.Error("ttl should be omitted when zero")
	}
}

// An explicit Name wins over the generateName default.
func TestMigrate_ExplicitName(t *testing.T) {
	dyn := fakeDyn(uGuest("default", "vm-a", "Running"))
	created, err := Migrate(context.Background(), dyn, MigrateParams{
		Namespace:  "default",
		GuestName:  "vm-a",
		TargetNode: "miles",
		Name:       "vm-a-rebalance",
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if created.GetName() != "vm-a-rebalance" {
		t.Errorf("explicit name not used: %q", created.GetName())
	}
}

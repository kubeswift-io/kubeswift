package podmode

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsandbox"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

const (
	ownerUID = "5a1b2c3d-0000-4000-8000-00000000a001"
	uid2     = "5a1b2c3d-0000-4000-8000-00000000b002"
	ownerAPI = "agents.x-k8s.io/v1beta1"
)

// owner is the agent-sandbox Sandbox sb-1 in mode.
func owner(mode string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(ownerAPI)
	u.SetKind(podmode.AgentSandboxKind)
	u.SetNamespace("tenant")
	u.SetName("sb-1")
	u.SetUID(ownerUID)
	_ = unstructured.SetNestedField(u.Object, mode, "spec", "operatingMode")
	return u
}

func setOwnerMode(t *testing.T, c client.Client, mode string) {
	t.Helper()
	u := owner("")
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(u.Object, mode, "spec", "operatingMode")
	if err := c.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

// ownedStandIn is a stand-in of agent-sandbox Sandbox sb-1, with UID id.
func ownedStandIn(t *testing.T, id string) *corev1.Pod {
	t.Helper()
	p := standIn(t)
	p.UID = types.UID(id)
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: ownerAPI, Kind: podmode.AgentSandboxKind,
		Name: "sb-1", UID: ownerUID, Controller: ptr.To(true)}}
	return p
}

func sandboxNamed(t *testing.T, c client.Client, name string) *sandboxv1alpha1.SwiftSandbox {
	t.Helper()
	var sb sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: pmNS, Name: name}, &sb); err != nil {
		t.Fatal(err)
	}
	return &sb
}

func setSandboxStatus(t *testing.T, c client.Client, name string, f func(*sandboxv1alpha1.SwiftSandboxStatus)) {
	t.Helper()
	sb := sandboxNamed(t, c, name)
	f(&sb.Status)
	if err := c.Status().Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
}

func deleteStandIn(t *testing.T, c client.Client) {
	t.Helper()
	if err := c.Delete(context.Background(), getStandIn(t, c)); err != nil {
		t.Fatal(err)
	}
}

func standInGone(c client.Client) bool {
	var pod corev1.Pod
	return apierrors.IsNotFound(c.Get(context.Background(), standInKey, &pod))
}

func sandboxExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var sb sandboxv1alpha1.SwiftSandbox
	err := c.Get(context.Background(), types.NamespacedName{Namespace: pmNS, Name: name}, &sb)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil && sb.DeletionTimestamp == nil
}

// suspended runs an agent-sandbox suspend to the end: the stand-in of
// Sandbox sb-1 is gone and its microVM sbKey is Suspended on node-a.
func suspended(t *testing.T) (*Reconciler, client.Client) {
	t.Helper()
	r, c, _ := setup(t, ownedStandIn(t, uid), owner("Running"))
	reconcileOnce(t, r, "tenant", "sb-1")
	if l := getStandIn(t, c).Labels[podmode.LabelSandbox]; l != sbKey {
		t.Fatalf("stand-in sandbox label %q", l)
	}
	setPhase(t, c, sandboxv1alpha1.SwiftSandboxRunning, nil)

	setOwnerMode(t, c, "Suspended")
	deleteStandIn(t, c)
	reconcileOnce(t, r, "tenant", "sb-1")
	sb := getSandbox(t, c)
	if !sb.Spec.Suspended() || sb.Labels[podmode.LabelOwnerUID] != ownerUID ||
		sb.Annotations[podmode.AnnotationOwner] != ownerAPI+"/sb-1" {
		t.Fatalf("sandbox not asked to suspend: mode %q labels %v annotations %v",
			sb.Spec.OperatingMode, sb.Labels, sb.Annotations)
	}
	if standInGone(c) {
		t.Fatal("stand-in released before its microVM was suspended")
	}
	if cond := readyCondition(getStandIn(t, c)); cond == nil || cond.Reason != reasonSuspending {
		t.Errorf("readiness %+v", cond)
	}

	setSandboxStatus(t, c, sbKey, func(s *sandboxv1alpha1.SwiftSandboxStatus) {
		s.Phase = sandboxv1alpha1.SwiftSandboxSuspended
		s.SuspendedState = &sandboxv1alpha1.SandboxSuspendedState{NodeName: "node-a"}
	})
	reconcileOnce(t, r, "tenant", "sb-1")
	if !standInGone(c) {
		t.Fatal("stand-in kept after its microVM was suspended")
	}
	// The stand-in is gone; its microVM stays while the Sandbox exists.
	if res := reconcileOnce(t, r, "tenant", "sb-1"); res.RequeueAfter != suspendRecheck {
		t.Errorf("no recheck of the owner: %+v", res)
	}
	if !sandboxExists(t, c, sbKey) {
		t.Fatal("suspended microVM deleted while its Sandbox exists")
	}
	return r, c
}

func TestAgentSandboxSuspendKeepsTheMicroVM(t *testing.T) {
	suspended(t)
}

func TestAgentSandboxResumeClaimsTheMicroVM(t *testing.T) {
	r, c := suspended(t)
	setOwnerMode(t, c, "Running")
	if err := c.Create(context.Background(), ownedStandIn(t, uid2)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")

	pod := getStandIn(t, c)
	if pod.Labels[podmode.LabelSandbox] != sbKey {
		t.Fatalf("stand-in sandbox label %q, want %s", pod.Labels[podmode.LabelSandbox], sbKey)
	}
	sb := getSandbox(t, c)
	if sb.Labels[podmode.LabelUID] != uid2 || sb.Spec.Suspended() {
		t.Fatalf("microVM not claimed and resumed: uid %q mode %q", sb.Labels[podmode.LabelUID], sb.Spec.OperatingMode)
	}
	if sandboxExists(t, c, SandboxPrefix+uid2) {
		t.Error("a new microVM was created for a resume")
	}
	if cond := readyCondition(pod); cond == nil || cond.Reason != reasonResuming {
		t.Errorf("readiness %+v", cond)
	}
	// The new stand-in's egress reaches the claimed microVM's launcher.
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: NetworkPolicyPrefix + uid2}, &np); err != nil {
		t.Fatal(err)
	}
	if got := np.Spec.Egress[len(np.Spec.Egress)-1].To[0].PodSelector.MatchLabels[swiftsandbox.SandboxLabelKey]; got != sbKey {
		t.Errorf("egress policy targets %q", got)
	}

	setSandboxStatus(t, c, sbKey, func(s *sandboxv1alpha1.SwiftSandboxStatus) {
		s.Phase = sandboxv1alpha1.SwiftSandboxResuming
	})
	reconcileOnce(t, r, "tenant", "sb-1")
	if cond := readyCondition(getStandIn(t, c)); cond == nil || cond.Reason != reasonResuming {
		t.Errorf("readiness while resuming %+v", cond)
	}

	if err := c.Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: sbKey + "-r", Namespace: pmNS, Labels: map[string]string{swiftsandbox.SandboxLabelKey: sbKey}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.2.9"},
	}); err != nil {
		t.Fatal(err)
	}
	setSandboxStatus(t, c, sbKey, func(s *sandboxv1alpha1.SwiftSandboxStatus) {
		s.Phase = sandboxv1alpha1.SwiftSandboxRunning
		s.SuspendedState = nil
	})
	reconcileOnce(t, r, "tenant", "sb-1")
	pod = getStandIn(t, c)
	if cond := readyCondition(pod); cond == nil || cond.Status != corev1.ConditionTrue {
		t.Errorf("not ready after the resume: %+v", cond)
	}
	if pod.Annotations[podmode.AnnotationTarget] != "10.1.2.9" {
		t.Errorf("target %q", pod.Annotations[podmode.AnnotationTarget])
	}

	// A real deletion of the resumed stand-in deletes the microVM.
	deleteStandIn(t, c)
	reconcileOnce(t, r, "tenant", "sb-1")
	if sandboxExists(t, c, sbKey) {
		t.Error("resumed microVM kept after its stand-in was deleted")
	}
}

func TestDeletionWithoutSuspendDeletesTheMicroVM(t *testing.T) {
	r, c, _ := setup(t, ownedStandIn(t, uid), owner("Running"))
	reconcileOnce(t, r, "tenant", "sb-1")
	setPhase(t, c, sandboxv1alpha1.SwiftSandboxRunning, nil)
	deleteStandIn(t, c)
	reconcileOnce(t, r, "tenant", "sb-1")
	if sandboxExists(t, c, sbKey) {
		t.Error("microVM kept although the Sandbox is not suspending")
	}
}

func TestSandboxDeletedWhileSuspended(t *testing.T) {
	r, c := suspended(t)
	if err := c.Delete(context.Background(), owner("")); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	if sandboxExists(t, c, sbKey) {
		t.Error("suspended microVM kept after its Sandbox was deleted")
	}
}

func TestSuspendFailedDeletesTheMicroVM(t *testing.T) {
	r, c, rec := setup(t, ownedStandIn(t, uid), owner("Suspended"))
	reconcileOnce(t, r, "tenant", "sb-1")
	setPhase(t, c, sandboxv1alpha1.SwiftSandboxRunning, nil)
	deleteStandIn(t, c)
	reconcileOnce(t, r, "tenant", "sb-1")
	sb := getSandbox(t, c)
	meta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{Type: sandboxv1alpha1.SwiftSandboxConditionSuspended,
		Status: metav1.ConditionFalse, Reason: "SuspendFailed", Message: "snapshot: disk full", ObservedGeneration: sb.Generation})
	if err := c.Status().Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	if sandboxExists(t, c, sbKey) {
		t.Error("microVM kept after its suspend failed")
	}
	if !hasEvent(rec, "MicroVMSuspendFailed") {
		t.Error("no MicroVMSuspendFailed event")
	}
}

func TestResumeOnAnotherNodeColdBoots(t *testing.T) {
	r, c := suspended(t)
	p := ownedStandIn(t, uid2)
	p.Spec.NodeName = "node-b"
	if err := c.Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b",
		Labels: map[string]string{corev1.LabelHostname: "host-b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	if sandboxExists(t, c, sbKey) {
		t.Error("a microVM whose state is on another node was kept")
	}
	if l := getStandIn(t, c).Labels[podmode.LabelSandbox]; l != SandboxPrefix+uid2 {
		t.Errorf("stand-in sandbox label %q after a cold boot", l)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	if !sandboxExists(t, c, SandboxPrefix+uid2) {
		t.Error("no new microVM after the cold boot")
	}
}

func TestSnapshotLostColdBoots(t *testing.T) {
	r, c := suspended(t)
	if err := c.Create(context.Background(), ownedStandIn(t, uid2)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	sb := getSandbox(t, c)
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxFailed
	meta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{Type: sandboxv1alpha1.SwiftSandboxConditionSuspended,
		Status: metav1.ConditionFalse, Reason: "SnapshotLost", Message: "the rootfs changed"})
	if err := c.Status().Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	if sandboxExists(t, c, sbKey) {
		t.Error("a microVM whose snapshot is lost was kept")
	}
	if a := getStandIn(t, c).Annotations[podmode.AnnotationExit]; a != "" {
		t.Errorf("a lost snapshot ended the Pod: exit %q", a)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	if !sandboxExists(t, c, SandboxPrefix+uid2) {
		t.Error("no new microVM after the snapshot was lost")
	}
}

func TestForgedSandboxLabelIgnored(t *testing.T) {
	other := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{
		Name: SandboxPrefix + "victim", Namespace: pmNS,
		Labels:      map[string]string{podmode.LabelUID: "victim", podmode.LabelStandInNamespace: "tenant"},
		Annotations: map[string]string{podmode.AnnotationStandIn: "tenant/victim"},
	}}
	p := standIn(t)
	p.Labels[podmode.LabelSandbox] = other.Name
	r, c, _ := setup(t, p, other)
	reconcileOnce(t, r, "tenant", "sb-1")
	if l := getStandIn(t, c).Labels[podmode.LabelSandbox]; l != sbKey {
		t.Errorf("stand-in sandbox label %q, want its own %s", l, sbKey)
	}
	if v := sandboxNamed(t, c, other.Name); v.Labels[podmode.LabelUID] != "victim" {
		t.Errorf("another stand-in's microVM changed: %v", v.Labels)
	}
	if !sandboxExists(t, c, sbKey) {
		t.Error("the stand-in got no microVM of its own")
	}
}

// hasEvent drains the recorder and reports whether an event had reason.
func hasEvent(rec *record.FakeRecorder, reason string) bool {
	found := false
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, " "+reason+" ") {
				found = true
			}
		default:
			return found
		}
	}
}

package swiftguest

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

func identityCloneGuest() *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name: "clone-a", Namespace: "default", UID: "clone-uid-12345678",
			Annotations: map[string]string{
				AnnotationCloneAgentEnabled:  "true",
				AnnotationRestoreMACRewrites: "52:54:00:11:22:33,52:54:00:44:55:66",
			},
		},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			CloneFromSnapshot: &swiftv1alpha1.CloneFromSnapshotSource{
				SnapshotRef: corev1.LocalObjectReference{Name: "snap"},
				Regenerate:  []swiftv1alpha1.CloneIdentityItem{swiftv1alpha1.CloneRegenMachineID, swiftv1alpha1.CloneRegenMACAddresses},
			},
		},
	}
}

func readyClonePod(annos map[string]string) *corev1.Pod {
	if annos == nil {
		annos = map[string]string{}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-a", Namespace: "default", Annotations: annos},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestEnsureCloneIdentityRegen_StampsActionFirstPass(t *testing.T) {
	g := identityCloneGuest()
	pod := readyClonePod(nil)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(g, pod).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	status := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseRunning}

	rq, err := r.ensureCloneIdentityRegen(context.Background(), g, pod, status)
	if err != nil {
		t.Fatalf("ensureCloneIdentityRegen: %v", err)
	}
	if rq != 3*time.Second {
		t.Errorf("expected 3s requeue while regenerating, got %v", rq)
	}
	// pod gained the identity-action annotations
	var got corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "clone-a"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[identityActionKey] != identityVerbRegenerate {
		t.Errorf("identity-action = %q, want regenerate", got.Annotations[identityActionKey])
	}
	if got.Annotations[identityActionIDKey] != cloneIdentityActionID(g) {
		t.Errorf("identity-action-id = %q, want %q", got.Annotations[identityActionIDKey], cloneIdentityActionID(g))
	}
	args := got.Annotations[identityActionArgsKey]
	for _, want := range []string{`"machineId"`, `"macAddresses"`, `"52:54:00:11:22:33"`, `"hostname":"clone-a"`, `"renewLease":true`} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	if cond := findCondition(status, ConditionCloneIdentityRegenerated); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("expected CloneIdentityRegenerated=False (Regenerating), got %+v", cond)
	}
}

func TestEnsureCloneIdentityRegen_Ready(t *testing.T) {
	g := identityCloneGuest()
	id := cloneIdentityActionID(g)
	pod := readyClonePod(map[string]string{
		identityActionIDKey:  id,
		identityStatusIDKey:  id,
		identityStatusKey:    "ready",
		identityStatusDetail: "regenerated [machineId macAddresses], ip=192.168.99.12",
	})
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(g, pod).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	status := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseRunning}

	rq, err := r.ensureCloneIdentityRegen(context.Background(), g, pod, status)
	if err != nil || rq != 0 {
		t.Fatalf("expected terminal (0 requeue), got rq=%v err=%v", rq, err)
	}
	cond := findCondition(status, ConditionCloneIdentityRegenerated)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected CloneIdentityRegenerated=True, got %+v", cond)
	}
}

func TestEnsureCloneIdentityRegen_GuestAgentUnreachable(t *testing.T) {
	g := identityCloneGuest()
	id := cloneIdentityActionID(g)
	pod := readyClonePod(map[string]string{
		identityActionIDKey:  id,
		identityStatusIDKey:  id,
		identityStatusKey:    "failed",
		identityStatusDetail: "GuestAgentUnreachable: vsock io: No such file or directory",
	})
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(g, pod).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	status := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseRunning}

	if _, err := r.ensureCloneIdentityRegen(context.Background(), g, pod, status); err != nil {
		t.Fatal(err)
	}
	cond := findCondition(status, ConditionCloneIdentityRegenerated)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "GuestAgentUnreachable" {
		t.Fatalf("expected CloneIdentityRegenerated=False/GuestAgentUnreachable, got %+v", cond)
	}
}

func TestEnsureCloneIdentityRegen_NoOpWhenNotAgentClone(t *testing.T) {
	// plain guest (not a clone)
	g := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"}}
	pod := readyClonePod(nil)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(g, pod).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	status := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseRunning}
	rq, err := r.ensureCloneIdentityRegen(context.Background(), g, pod, status)
	if err != nil || rq != 0 {
		t.Fatalf("plain guest must be a no-op, got rq=%v err=%v", rq, err)
	}
	if findCondition(status, ConditionCloneIdentityRegenerated) != nil {
		t.Error("must not set the condition for a non-clone")
	}

	// clone but agent not enabled (no AnnotationCloneAgentEnabled)
	g2 := identityCloneGuest()
	delete(g2.Annotations, AnnotationCloneAgentEnabled)
	status2 := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseRunning}
	if _, err := r.ensureCloneIdentityRegen(context.Background(), g2, readyClonePod(nil), status2); err != nil {
		t.Fatal(err)
	}
	if findCondition(status2, ConditionCloneIdentityRegenerated) != nil {
		t.Error("must not set the condition when the agent is not enabled")
	}
}

func TestCloneIdentityHelpers(t *testing.T) {
	g := identityCloneGuest()
	if a := cloneIdentityActionID(g); a != cloneIdentityActionID(g) || a != "clone-a-clone-ui-identity" {
		t.Errorf("actionID not stable/expected: %q", a)
	}
	if items := cloneRegenItems(g.Spec.CloneFromSnapshot); len(items) != 2 || items[0] != "machineId" {
		t.Errorf("cloneRegenItems = %v", items)
	}
	if items := cloneRegenItems(&swiftv1alpha1.CloneFromSnapshotSource{}); items != nil {
		t.Errorf("empty regenerate must map to nil (agent defaults all), got %v", items)
	}
	if mac := primaryCloneMAC(g); mac != "52:54:00:11:22:33" {
		t.Errorf("primaryCloneMAC = %q, want first CSV entry", mac)
	}
	if mac := primaryCloneMAC(&swiftv1alpha1.SwiftGuest{}); mac != "" {
		t.Errorf("primaryCloneMAC with no rewrites must be empty, got %q", mac)
	}
}

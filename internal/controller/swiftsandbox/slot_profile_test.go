package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func readySlot(name, profile string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns", UID: types.UID("uid-" + name),
			Labels:      map[string]string{PoolLabelKey: "p", SlotStateLabelKey: slotStateWarm},
			Annotations: map[string]string{SlotProfileAnnotation: profile},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: launcherName, Ready: true}},
		},
	}
}

func restrictedSandbox() *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "sb-uid"},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image:   "busybox:1",
			PoolRef: &corev1.LocalObjectReference{Name: "p"},
			// network.mode unset: restricted by default
			VerifyKeySecretRef: &sandboxv1alpha1.SecretObjectReference{Name: "cosign-pub"},
		},
	}
}

// A restricted, must-verify sandbox must never claim a slot booted open or
// unverified: checkout only injects a command, so the slot's network and
// image verification are what the workload gets.
func TestTryClaimWarmSlot_OnlyASlotWithTheSandboxsSettings(t *testing.T) {
	sb := restrictedSandbox()
	open := slotProfile("busybox:1", sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkOpen}, sb.Spec.VerifyKeySecretRef)
	unverified := slotProfile("busybox:1", sandboxv1alpha1.SandboxNetwork{}, nil)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(sb, readySlot("p-slot-open1", open), readySlot("p-slot-unver", unverified)).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}

	slot, err := r.tryClaimWarmSlot(context.Background(), sb)
	if err != nil {
		t.Fatal(err)
	}
	if slot != nil {
		t.Fatalf("claimed %s, whose network/verification differ from the sandbox's", slot.Name)
	}

	if err := c.Create(context.Background(), readySlot("p-slot-match", sandboxSlotProfile(sb))); err != nil {
		t.Fatal(err)
	}
	slot, err = r.tryClaimWarmSlot(context.Background(), sb)
	if err != nil || slot == nil || slot.Name != "p-slot-match" {
		t.Fatalf("want the matching slot claimed, got slot=%v err=%v", slot, err)
	}
}

// An unset network mode is the CRD default, restricted -- the same profile.
func TestSlotProfile_UnsetNetworkModeIsRestricted(t *testing.T) {
	if slotProfile("i", sandboxv1alpha1.SandboxNetwork{}, nil) !=
		slotProfile("i", sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkRestricted}, nil) {
		t.Error("unset and explicit restricted must be the same profile")
	}
}

// A pool edit (here open -> restricted) used to leave already-warm slots
// running the old settings until claimed. They are recycled now.
func TestPoolReconcile_RecyclesSlotsBootedUnderAnEarlierSpec(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "busybox:1", MinWarm: 1},
		Status: sandboxv1alpha1.SwiftSandboxPoolStatus{
			Rootfs: &sandboxv1alpha1.SandboxRootfsStatus{Digest: "sha256:deadbeef"},
		},
	}
	stale := readySlot("p-slot-stale", slotProfile("busybox:1", sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkOpen}, nil))
	current := readySlot("p-slot-curnt", poolSlotProfile(pool))
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool, stale, current).
		WithStatusSubresource(pool).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}}); err != nil {
		t.Fatal(err)
	}
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(stale), &p); !apierrors.IsNotFound(err) {
		t.Errorf("a slot booted under the old (open) spec should be recycled, err=%v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(current), &p); err != nil {
		t.Errorf("a slot matching the current spec must be kept: %v", err)
	}
}

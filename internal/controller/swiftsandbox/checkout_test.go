package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func TestPoolExecArgs(t *testing.T) {
	// command + args + cwd + the pool image env merged with spec.env (spec overrides
	// by key), no registry pull.
	sb := &sandboxv1alpha1.SwiftSandbox{Spec: sandboxv1alpha1.SwiftSandboxSpec{
		Command:    []string{"sh", "-c"},
		Args:       []string{"echo hi"},
		Env:        []corev1.EnvVar{{Name: "A", Value: "1"}},
		WorkingDir: "/tmp",
	}}
	argv, env, cwd := poolExecArgs(sb, []string{"PATH=/usr/bin", "A=image"})
	if strings.Join(argv, " ") != "sh -c echo hi" {
		t.Errorf("argv = %v", argv)
	}
	// image PATH kept; A overridden by spec.env; image order preserved.
	if strings.Join(env, ",") != "PATH=/usr/bin,A=1" || cwd != "/tmp" {
		t.Errorf("env=%v cwd=%q", env, cwd)
	}
	// No command -> nil argv (caller cold-falls-back to the image entrypoint).
	if a, _, _ := poolExecArgs(&sandboxv1alpha1.SwiftSandbox{}, nil); a != nil {
		t.Errorf("no-command argv should be nil, got %v", a)
	}
}

func TestNonControllerRefs(t *testing.T) {
	refs := []metav1.OwnerReference{
		{Name: "keep-nonctrl"},
		{Name: "pool", Controller: ptr.To(true)},
		{Name: "keep-explicit-false", Controller: ptr.To(false)},
	}
	out := nonControllerRefs(refs)
	if len(out) != 2 {
		t.Fatalf("expected 2 non-controller refs, got %d: %v", len(out), out)
	}
	for _, r := range out {
		if r.Controller != nil && *r.Controller {
			t.Errorf("controller ref survived: %v", r)
		}
	}
}

func checkoutTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	gv := schema.GroupVersion{Group: "sandbox.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gv,
		&sandboxv1alpha1.SwiftSandbox{}, &sandboxv1alpha1.SwiftSandboxList{},
		&sandboxv1alpha1.SwiftSandboxPool{}, &sandboxv1alpha1.SwiftSandboxPoolList{})
	metav1.AddToGroupVersion(s, gv)
	return s
}

// TestReconcileClaimedSlot_ExitMapsToTerminal is the key checkout behavior: a claimed
// slot's exec-status (mirroring the sandbox UID) with a non-zero exit code drives the
// sandbox to Failed, records the exit code, and destroys the consumed slot pod.
func TestReconcileClaimedSlot_ExitMapsToTerminal(t *testing.T) {
	s := checkoutTestScheme()
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "uid-1"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "alpine", PoolRef: &corev1.LocalObjectReference{Name: "pool"}},
		Status:     sandboxv1alpha1.SwiftSandboxStatus{PodRef: "pool-slot-abc"},
	}
	slot := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pool-slot-abc", Namespace: "ns",
		Annotations: map[string]string{
			annSandboxExecStatusID:     "uid-1",
			annSandboxExecStatus:       "complete",
			annSandboxExecStatusDetail: "7",
		},
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sb, slot).
		WithStatusSubresource(sb).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	if _, err := r.reconcileClaimedSlot(context.Background(), sb); err != nil {
		t.Fatalf("reconcileClaimedSlot: %v", err)
	}

	var got sandboxv1alpha1.SwiftSandbox
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &got)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.ExitCode == nil || *got.Status.ExitCode != 7 {
		t.Errorf("exitCode = %v, want 7", got.Status.ExitCode)
	}
	// the consumed slot pod is destroyed (the pool replenishes a fresh warm one).
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "pool-slot-abc"}, &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("slot pod should be deleted, got err=%v", err)
	}
}

// A status-id that does NOT mirror the sandbox UID is stale — the sandbox stays Running
// and the slot is not consumed.
func TestReconcileClaimedSlot_IgnoresStaleStatus(t *testing.T) {
	s := checkoutTestScheme()
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "uid-1"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{PoolRef: &corev1.LocalObjectReference{Name: "pool"}},
		Status:     sandboxv1alpha1.SwiftSandboxStatus{PodRef: "pool-slot-abc"},
	}
	slot := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pool-slot-abc", Namespace: "ns",
		Annotations: map[string]string{
			annSandboxExecStatusID: "someone-else", annSandboxExecStatus: "complete", annSandboxExecStatusDetail: "0",
		},
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sb, slot).WithStatusSubresource(sb).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.reconcileClaimedSlot(context.Background(), sb); err != nil {
		t.Fatalf("reconcileClaimedSlot: %v", err)
	}
	var got sandboxv1alpha1.SwiftSandbox
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &got)
	if got.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed || got.Status.Phase == sandboxv1alpha1.SwiftSandboxCompleted {
		t.Errorf("stale status must not terminalize; phase=%q", got.Status.Phase)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "pool-slot-abc"}, &corev1.Pod{}); err != nil {
		t.Errorf("slot pod should survive a stale status: %v", err)
	}
}

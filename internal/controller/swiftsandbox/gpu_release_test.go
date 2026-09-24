package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// allocatedGPUSandbox is a native-GPU sandbox holding worker-1's GPU, with its
// launcher pod in phase podPhase ("" = no pod).
func allocatedGPUSandbox(t *testing.T, podPhase corev1.PodPhase, mut func(*sandboxv1alpha1.SwiftSandbox)) (*SwiftSandboxReconciler, client.Client) {
	t.Helper()
	ctx := context.Background()
	sb := nativeGPUSandbox("gpu-sb", "default", "gtx")
	sb.UID = "sb-uid"
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(sb, oneGPUNode("worker-1"), pcieProfile("gtx", "default")).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}, &gpuv1alpha1.SwiftGPUNode{}).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}
	if ready, _, err := r.reconcileNativeGPU(ctx, sb); err != nil || !ready {
		t.Fatalf("allocate: ready=%v err=%v", ready, err)
	}
	if podPhase != "" {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu-sb", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "sandbox.kubeswift.io/v1alpha1", Kind: "SwiftSandbox",
					Name: "gpu-sb", UID: "sb-uid", Controller: ptrTrue()}}},
			Status: corev1.PodStatus{Phase: podPhase},
		}
		if err := c.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	if mut != nil {
		var cur sandboxv1alpha1.SwiftSandbox
		if err := c.Get(ctx, client.ObjectKey{Name: "gpu-sb", Namespace: "default"}, &cur); err != nil {
			t.Fatal(err)
		}
		mut(&cur)
		if err := c.Status().Update(ctx, &cur); err != nil {
			t.Fatal(err)
		}
	}
	return r, c
}

func ptrTrue() *bool { b := true; return &b }

func gpuHolder(t *testing.T, c client.Client) string {
	t.Helper()
	var n gpuv1alpha1.SwiftGPUNode
	if err := c.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &n); err != nil {
		t.Fatal(err)
	}
	return n.Status.GPUs[0].AllocatedTo
}

func reconcileSandbox(t *testing.T, r *SwiftSandboxReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "gpu-sb", Namespace: "default"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// Deleting a GPU sandbox released its GPU at once, while its launcher -- which
// background GC removes only after the sandbox is gone -- still held the VFIO
// group; the next consumer's bind then failed "Resource busy". The launcher is
// deleted first and the GPU released only once it is gone.
func TestSandboxDeletion_ReleasesGPUOnlyAfterTheLauncherIsGone(t *testing.T) {
	r, c := allocatedGPUSandbox(t, corev1.PodRunning, nil)
	ctx := context.Background()
	var sb sandboxv1alpha1.SwiftSandbox
	if err := c.Get(ctx, client.ObjectKey{Name: "gpu-sb", Namespace: "default"}, &sb); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, &sb); err != nil { // finalizer holds it
		t.Fatal(err)
	}

	if res := reconcileSandbox(t, r); res.RequeueAfter == 0 {
		t.Error("deletion should wait (requeue) while the launcher may hold the GPU")
	}
	if got := gpuHolder(t, c); got != "sandbox:default/gpu-sb" {
		t.Fatalf("GPU released while the launcher still ran (allocatedTo=%q)", got)
	}
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Name: "gpu-sb", Namespace: "default"}, &pod); !apierrors.IsNotFound(err) {
		t.Fatalf("the launcher should be deleted by the sandbox's deletion, got err=%v", err)
	}

	reconcileSandbox(t, r) // launcher gone now
	if got := gpuHolder(t, c); got != "" {
		t.Errorf("GPU should be released once the launcher is gone, allocatedTo=%q", got)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "gpu-sb", Namespace: "default"}, &sb); !apierrors.IsNotFound(err) {
		t.Errorf("sandbox should be gone once its GPU is released, err=%v", err)
	}
}

// A finished sandbox is kept for its TTL (or indefinitely), and it kept its
// GPU reserved the whole time. It is returned once the launcher has exited.
func TestFinishedSandbox_ReturnsItsGPU(t *testing.T) {
	r, c := allocatedGPUSandbox(t, corev1.PodSucceeded, func(sb *sandboxv1alpha1.SwiftSandbox) {
		sb.Status.Phase = sandboxv1alpha1.SwiftSandboxCompleted
		now := metav1.Now()
		sb.Status.TerminalAt = &now
	})
	reconcileSandbox(t, r)
	if got := gpuHolder(t, c); got != "" {
		t.Errorf("a finished sandbox still holds its GPU (allocatedTo=%q)", got)
	}
	var sb sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), client.ObjectKey{Name: "gpu-sb", Namespace: "default"}, &sb); err != nil {
		t.Fatal(err)
	}
	if sb.Status.GPU != nil {
		t.Errorf("status.gpu should be cleared after release, got %+v", sb.Status.GPU)
	}
}

// ...but not while its launcher is still running (e.g. just told to stop).
func TestFinishedSandbox_KeepsTheGPUWhileTheLauncherRuns(t *testing.T) {
	r, c := allocatedGPUSandbox(t, corev1.PodRunning, func(sb *sandboxv1alpha1.SwiftSandbox) {
		sb.Status.Phase = sandboxv1alpha1.SwiftSandboxFailed
	})
	if res := reconcileSandbox(t, r); res.RequeueAfter == 0 {
		t.Error("should re-check once the launcher exits")
	}
	if got := gpuHolder(t, c); got != "sandbox:default/gpu-sb" {
		t.Errorf("GPU released while the launcher still ran (allocatedTo=%q)", got)
	}
}

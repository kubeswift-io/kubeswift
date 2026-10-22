package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// Until export is wired, a sandbox that declares outputs fails up front: it
// never runs and silently exports nothing, cold or pooled.
func TestReconcile_OutputsNotSupportedYet(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		sb := plainSandbox("busybox:1")
		sb.UID = "sb-uid"
		sb.Spec.Outputs = []sandboxv1alpha1.SandboxOutput{{Name: "out", Path: "/output"}}
		if pooled {
			sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: "pool"}
		}
		r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
		reconcileSB(t, r, "sb")
		got := getSandbox(t, c, "sb")
		cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
		if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != sandboxv1alpha1.OutputReasonNotSupported {
			t.Errorf("pooled=%v: phase %s, GuestRunning %+v; want Failed/%s", pooled, got.Status.Phase, cond, sandboxv1alpha1.OutputReasonNotSupported)
		}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
			t.Errorf("pooled=%v: a launcher was created (err %v)", pooled, err)
		}
	}
}

// Invalid outputs are reported as such, before the not-supported gate.
func TestReconcile_InvalidOutputs(t *testing.T) {
	sb := plainSandbox("busybox:1")
	sb.Spec.Outputs = []sandboxv1alpha1.SandboxOutput{{Name: "out", Path: "/proc/x"}}
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	cond := apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if cond == nil || cond.Reason != "InvalidOutputs" {
		t.Errorf("GuestRunning %+v, want reason InvalidOutputs", cond)
	}
}

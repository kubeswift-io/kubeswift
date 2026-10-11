package swiftsandbox

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// ownLauncher makes sb the controller of pod, as createLaunch does: only
// such a pod is sb's cold launcher.
func ownLauncher(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) *corev1.Pod {
	if err := controllerutil.SetControllerReference(sb, pod, scheme.Scheme); err != nil {
		panic(err)
	}
	return pod
}

// previousLauncher is the pod a deleted sandbox named sb left behind: owned by
// the old sandbox's UID, not by the sandbox now carrying that name.
func previousLauncher(state string) *corev1.Pod {
	old := plainSandbox("busybox:1")
	old.UID = "old-uid"
	pod := ownLauncher(old, buildPod(old, "sandbox"))
	pod.UID = "old-pod-uid"
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: launcherName, Ready: true}}}
	switch state {
	case "terminating":
		pod.Finalizers = []string{"test.kubeswift.io/kubelet"}
		pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	case "finished":
		pod.Status.Phase = corev1.PodSucceeded
	}
	return pod
}

// #772: a sandbox re-created under the name of one whose launcher is still
// there must not take that pod over: not report its state, not fail when it
// exits, not touch its grant. It waits for the name, or reports a conflict for
// a live pod that is not its own.
func TestReconcile_SameNameDoesNotAdoptAnotherLauncher(t *testing.T) {
	for _, tc := range []struct {
		state  string
		pooled bool
		reason string
	}{
		{"terminating", false, reasonPreviousLauncherStopping},
		{"finished", false, reasonPreviousLauncherStopping},
		{"running", false, reasonLauncherPodConflict},
		{"terminating", true, reasonPreviousLauncherStopping},
	} {
		sb := plainSandbox("busybox:1")
		sb.UID = "new-uid"
		if tc.pooled {
			// A pool miss that fell back to the cold path.
			sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: "pool"}
			sb.Status.PodRef = sb.Name
		}
		r, c := sandboxReconciler(sb, previousLauncher(tc.state), readyKernel("default", defaultKernelProfile))
		reconcileSB(t, r, "sb")

		got := getSandbox(t, c, "sb")
		cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
		name := tc.state
		if tc.pooled {
			name += ", pooled"
		}
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tc.reason {
			t.Errorf("%s: Resolved = %+v, want False/%s", name, cond, tc.reason)
		}
		if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPending {
			t.Errorf("%s: phase %q, want Pending", name, got.Status.Phase)
		}
		for _, c := range got.Status.Conditions {
			if c.Type == sandboxv1alpha1.SwiftSandboxConditionGuestRunning || c.Type == sandboxv1alpha1.SwiftSandboxConditionWorkloadReady {
				t.Errorf("%s: %s reported from another sandbox's launcher", name, c.Type)
			}
		}
		var pod corev1.Pod
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil || pod.UID != "old-pod-uid" {
			t.Errorf("%s: previous launcher changed (uid %s, err %v)", name, pod.UID, err)
		} else if !metav1.IsControlledBy(&pod, &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{UID: "old-uid"}}) {
			t.Errorf("%s: previous launcher re-parented: %+v", name, pod.OwnerReferences)
		}
		var rb rbacv1.RoleBinding
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "swiftletd-scoped-sb"}, &rb); !apierrors.IsNotFound(err) {
			t.Errorf("%s: a grant was minted for the previous launcher's name (err %v)", name, err)
		}
	}
}

// Once the previous launcher is gone, the sandbox goes on to its own.
func TestReconcile_SameNameProceedsOnceThePreviousLauncherIsGone(t *testing.T) {
	sb := plainSandbox("busybox:1")
	sb.UID = "new-uid"
	old := previousLauncher("terminating")
	r, c := sandboxReconciler(sb, old, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")

	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Finalizers = nil
	if err := c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "sb")
	cond := apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
	if cond != nil && (cond.Reason == reasonPreviousLauncherStopping || cond.Reason == reasonLauncherPodConflict) {
		t.Errorf("still waiting for the name after the previous launcher is gone: %+v", cond)
	}
}

// The sandbox's own pod is its launcher, whatever its state.
func TestForeignPodReason(t *testing.T) {
	sb := plainSandbox("busybox:1")
	sb.UID = "uid"
	own := ownLauncher(sb, buildPod(sb, "sandbox"))
	own.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	if reason, _, _ := foreignPodReason(sb, own); reason != "" {
		t.Errorf("own terminating pod: reason %q, want none", reason)
	}
	orphan := buildPod(sb, "sandbox")
	if reason, _, _ := foreignPodReason(sb, orphan); reason != reasonLauncherPodConflict {
		t.Errorf("a pod with no controller: reason %q, want %s", reason, reasonLauncherPodConflict)
	}
}

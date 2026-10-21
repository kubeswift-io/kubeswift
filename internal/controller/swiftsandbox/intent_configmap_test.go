package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func intentEnv(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name != launcherName {
			continue
		}
		for _, e := range c.Env {
			if e.Name == swiftguest.EnvIntentSHA256 {
				return e.Value
			}
		}
	}
	return ""
}

// A ConfigMap named like the sandbox's intent that someone else created first
// used to be mounted as it was, so a tenant chose what the privileged launcher
// ran. It is now refused and left alone, and no launcher is created.
func TestReconcile_ForeignIntentConfigMapIsNeverUsed(t *testing.T) {
	ctx := context.Background()
	sb := plainSandbox(testImage(t))
	sb.UID = "sb-uid"
	forged := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: intentConfigMapName(sb), Namespace: "default"},
		Data:       map[string]string{swiftguest.IntentFile: `{"tenant":"supplied"}`},
	}
	r, c := sandboxReconciler(sb, forged, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")

	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonIntentConfigMapConflict {
		t.Errorf("Resolved = %+v, want False/%s", cond, reasonIntentConfigMapConflict)
	}
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPending {
		t.Errorf("phase %q, want Pending", got.Status.Phase)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("a launcher was created next to a foreign intent (err %v)", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(forged), &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data[swiftguest.IntentFile] != `{"tenant":"supplied"}` || len(cm.OwnerReferences) != 0 {
		t.Errorf("someone else's ConfigMap was modified or adopted: %+v", cm)
	}
}

// The launcher's intent is immutable, the sandbox's, and the pod carries its
// digest, so a ConfigMap changed or replaced afterwards is refused by swiftletd.
func TestReconcile_ColdLauncherPinsItsIntent(t *testing.T) {
	ctx := context.Background()
	sb := plainSandbox(testImage(t))
	sb.UID = "sb-uid"
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")

	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatalf("launcher pod: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: intentConfigMapName(sb)}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Immutable == nil || !*cm.Immutable {
		t.Error("intent ConfigMap is not immutable")
	}
	if !metav1.IsControlledBy(&cm, getSandbox(t, c, "sb")) {
		t.Errorf("intent ConfigMap is not the sandbox's: %+v", cm.OwnerReferences)
	}
	want := swiftguest.IntentDigest([]byte(cm.Data[swiftguest.IntentFile]))
	if got := intentEnv(&pod); got != want {
		t.Errorf("launcher %s = %q, want the intent's digest %q", swiftguest.EnvIntentSHA256, got, want)
	}
}

// An intent ConfigMap the sandbox owns but that holds an earlier intent (a
// launch that did not get as far as the pod) is replaced, not reused.
func TestReconcile_StaleOwnIntentIsReplaced(t *testing.T) {
	ctx := context.Background()
	sb := plainSandbox(testImage(t))
	sb.UID = "sb-uid"
	stale := ownIntent(sb, `{"stale":true}`)
	r, c := sandboxReconciler(sb, stale, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")

	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(stale), &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data[swiftguest.IntentFile] == `{"stale":true}` {
		t.Fatal("the stale intent was kept")
	}
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatalf("launcher pod: %v", err)
	}
	if intentEnv(&pod) != swiftguest.IntentDigest([]byte(cm.Data[swiftguest.IntentFile])) {
		t.Error("the launcher's digest is not the replacement intent's")
	}
}

func ownIntent(sb *sandboxv1alpha1.SwiftSandbox, data string) *corev1.ConfigMap {
	cm := buildIntentConfigMap(sb, []byte(data))
	if err := controllerutil.SetControllerReference(sb, cm, scheme.Scheme); err != nil {
		panic(err)
	}
	return cm
}

// swiftletd's refusal of an intent that is not its pod's is the reason.
func TestFinishTerminal_IntentRefused(t *testing.T) {
	sb := plainSandbox("busybox:1")
	sb.UID = "sb-uid"
	pod := ownLauncher(sb, buildPod(sb, "sandbox"))
	pod.Annotations = map[string]string{annIntentError: "IntentMismatch: the runtime intent has sha256 a but this launcher was built for b; not starting"}
	pod.Status.Phase = corev1.PodFailed
	r, c := sandboxReconciler(sb, pod, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != reasonIntentMismatch {
		t.Errorf("phase %s, GuestRunning %+v; want Failed/%s", got.Status.Phase, cond, reasonIntentMismatch)
	}
}

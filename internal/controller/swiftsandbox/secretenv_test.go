package swiftsandbox

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

const secretValue = "postgres://user:s3cr3t-value@db/app"

func dbSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
		Data:       map[string][]byte{"url": []byte(secretValue)},
	}
}

func fromSecret(name, secret, key string, optional bool) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key, Optional: ptr.To(optional)}}}
}

func secretEnvSandbox(image string) *sandboxv1alpha1.SwiftSandbox {
	sb := plainSandbox(image)
	sb.Spec.Env = []corev1.EnvVar{{Name: "MODE", Value: "prod"}, fromSecret("DATABASE_URL", "db", "url", false)}
	return sb
}

// A Secret-backed variable used to land as KEY= (empty). It is now not a
// value at all on the controller side: the reference travels instead.
func TestMergeEnv_SkipsSecretBackedVariables(t *testing.T) {
	got := mergeEnv([]string{"PATH=/bin"}, secretEnvSandbox("x").Spec.Env)
	if want := []string{"PATH=/bin", "MODE=prod"}; !reflect.DeepEqual(got, want) {
		t.Errorf("mergeEnv = %q, want %q", got, want)
	}
}

func TestBuild_SecretEnvReferencesAndMemoryVolume(t *testing.T) {
	sb := secretEnvSandbox("x")
	ri := buildIntent(sb, "sandbox", "/r.ext4", "", execSpec{Argv: []string{"/app"}, Env: []string{"MODE=prod"}}, false)
	want := []runtimeintent.SecretEnvRef{{Name: "DATABASE_URL", Secret: "db", Key: "url"}}
	if ri.SandboxExec == nil || !reflect.DeepEqual(ri.SandboxExec.SecretEnv, want) {
		t.Fatalf("intent secretEnv = %+v", ri.SandboxExec)
	}
	pod := buildPod(sb, "sandbox")
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == secretRunVolume {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.EmptyDir == nil || vol.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Errorf("want a memory-backed %s volume, got %+v", secretRunVolume, vol)
	}
	envOK := false
	for _, e := range pod.Spec.Containers[0].Env {
		envOK = envOK || (e.Name == "KUBESWIFT_SECRET_RUN_DIR" && e.Value == secretRunDir)
	}
	if !envOK {
		t.Error("the launcher must be told where the memory volume is")
	}
	if p := buildPod(plainSandbox("x"), "sandbox"); len(p.Spec.Volumes) == len(pod.Spec.Volumes) {
		t.Error("a sandbox without secret env needs no memory volume")
	}
}

func roleSecrets(t *testing.T, c client.Client, pod string) []string {
	t.Helper()
	var role rbacv1.Role
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: swiftguest.ScopedRoleNameFor(pod)}, &role); err != nil {
		t.Fatalf("scoped role: %v", err)
	}
	for _, r := range role.Rules {
		if len(r.Resources) == 1 && r.Resources[0] == "secrets" {
			return r.ResourceNames
		}
	}
	return nil
}

// A cold launch waits for a missing Secret or key, naming it; once it exists
// the launcher's own account may read exactly it, and the value appears in
// no object the controller writes.
func TestReconcile_SecretEnvColdLaunch(t *testing.T) {
	ctx := context.Background()
	sb := secretEnvSandbox(testImage(t))
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))

	reconcileSB(t, r, "sb")
	cond := apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
	if cond == nil || cond.Reason != "SecretNotFound" || !strings.Contains(cond.Message, "Secret db") {
		t.Fatalf("want Pending/SecretNotFound naming db, got %+v", cond)
	}

	wrongKey := dbSecret()
	wrongKey.Data = map[string][]byte{"other": []byte("x")}
	if err := c.Create(ctx, wrongKey); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "sb")
	cond = apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
	if cond == nil || cond.Reason != "SecretKeyNotFound" || !strings.Contains(cond.Message, "no key url") {
		t.Fatalf("want SecretKeyNotFound naming the key, got %+v", cond)
	}

	if err := c.Delete(ctx, wrongKey); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, dbSecret()); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "sb")
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatalf("launcher pod: %v", err)
	}
	if got := roleSecrets(t, c, "sb"); !reflect.DeepEqual(got, []string{"db"}) {
		t.Errorf("the launcher's grant on Secrets = %v, want exactly [db]", got)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: intentConfigMapName(sb)}, &cm); err != nil {
		t.Fatal(err)
	}
	for name, obj := range map[string]any{"intent ConfigMap": cm.Data, "launcher pod": pod, "sandbox": getSandbox(t, c, "sb")} {
		b, _ := json.Marshal(obj)
		if strings.Contains(string(b), "s3cr3t") {
			t.Errorf("the Secret's value appears in the %s", name)
		}
	}
	if !strings.Contains(strings.Join(mapValues(cm.Data), ""), `"secret":"db"`) {
		t.Errorf("the intent must carry the reference: %v", cm.Data)
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// An optional reference to a missing Secret does not hold the sandbox.
func TestReconcile_OptionalSecretEnvDoesNotWait(t *testing.T) {
	sb := plainSandbox(testImage(t))
	sb.Spec.Env = []corev1.EnvVar{fromSecret("TOKEN", "nope", "t", true)}
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Errorf("an optional missing Secret must not hold the launch: %v", err)
	}
}

// swiftletd could not read the Secret: the sandbox fails, naming it.
func TestReconcile_SecretErrorFailsTheSandbox(t *testing.T) {
	sb := secretEnvSandbox("busybox:1")
	pod := runningLauncher(sb, map[string]string{annSecretError: "secret env DATABASE_URL: cannot read Secret db: forbidden"})
	r, c := sandboxReconciler(sb, dbSecret(), pod)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "SecretUnavailable" {
		t.Errorf("want Failed/SecretUnavailable, got %s %+v", got.Status.Phase, cond)
	}
}

// A checkout grants the slot's own account exactly this sandbox's Secrets
// and hands swiftletd the reference, never the value; the pool's pass then
// leaves that grant alone.
func TestCheckout_SecretEnv(t *testing.T) {
	ctx := context.Background()
	pool := shapedPool()
	pool.Namespace = "default"
	slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
	slot.Namespace = "default"
	slot.Spec.ServiceAccountName = swiftguest.SandboxLauncherServiceAccountFor(slot.Name)
	sb := matchingSandbox()
	sb.Namespace = "default"
	sb.Spec.Env = []corev1.EnvVar{fromSecret("DATABASE_URL", "db", "url", false)}
	r, c := sandboxReconciler(pool, slot, sb, dbSecret())
	reconcileSB(t, r, "sb")

	if got := getSandbox(t, c, "sb"); got.Status.PodRef != slot.Name {
		t.Fatalf("want a checkout of %s, got %q (%s)", slot.Name, got.Status.PodRef, got.Status.Message)
	}
	if got := roleSecrets(t, c, slot.Name); !reflect.DeepEqual(got, []string{"db"}) {
		t.Errorf("slot grant on Secrets = %v, want [db]", got)
	}
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(slot), &p); err != nil {
		t.Fatal(err)
	}
	args := p.Annotations[annSandboxExecActionArgs]
	if !strings.Contains(args, `"secretEnv"`) || strings.Contains(args, "s3cr3t") {
		t.Errorf("action args must carry the reference and never the value: %s", args)
	}

	pr := &SwiftSandboxPoolReconciler{Client: c, APIReader: c, Scheme: r.Scheme, Recorder: r.Recorder}
	pool.Status.Rootfs = &sandboxv1alpha1.SandboxRootfsStatus{Digest: "sha256:deadbeef"}
	_ = c.Status().Update(ctx, pool)
	_, _ = pr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if got := roleSecrets(t, c, slot.Name); !reflect.DeepEqual(got, []string{"db"}) {
		t.Errorf("the pool's pass stripped the claimed slot's Secret grant: %v", got)
	}
}

// A sandbox with secret env never takes a slot running as the shared account.
func TestTryClaimWarmSlot_SecretEnvSkipsSharedAccountSlots(t *testing.T) {
	pool := shapedPool()
	slot := readySlot("p-slot-old00", poolSlotProfile(pool))
	slot.Spec.ServiceAccountName = swiftguest.SandboxLauncherServiceAccountName
	sb := matchingSandbox()
	sb.Spec.Env = []corev1.EnvVar{fromSecret("X", "db", "url", false)}
	r, _ := sandboxReconciler(slot, sb)
	got, err := r.tryClaimWarmSlot(context.Background(), sb, poolSlotProfile(pool))
	if err != nil || got != nil {
		t.Errorf("claimed %v (err %v); a shared-account slot must not get Secrets", got, err)
	}
}

// A claimed slot whose inject failed is injected on the next pass; it used
// to be adopted as if the workload were already there.
func TestCheckout_AdoptedSlotWithoutTheWorkloadIsInjected(t *testing.T) {
	ctx := context.Background()
	pool := shapedPool()
	pool.Namespace = "default"
	sb := matchingSandbox()
	sb.Namespace = "default"
	slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
	slot.Namespace = "default"
	slot.Labels[SlotStateLabelKey] = slotStateClaimed
	slot.Labels[SandboxLabelKey] = sb.Name
	r, c := sandboxReconciler(pool, slot, sb)
	reconcileSB(t, r, "sb")
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(slot), &p); err != nil {
		t.Fatal(err)
	}
	if p.Annotations[annSandboxExecActionID] != string(sb.UID) {
		t.Errorf("the adopted slot must get this sandbox's workload, action-id = %q", p.Annotations[annSandboxExecActionID])
	}
}

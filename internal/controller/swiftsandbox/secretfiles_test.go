package swiftsandbox

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

func registrySecret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-auth", Namespace: "default"},
		Data: map[string][]byte{"config.json": []byte(`{"auths":{"r":{"auth":"s3cr3t"}}}`), "ca.crt": []byte("CA")}}
}

func secretFilesSandbox(image string) *sandboxv1alpha1.SwiftSandbox {
	sb := plainSandbox(image)
	sb.Spec.SecretFiles = []sandboxv1alpha1.SandboxSecretFile{{
		SecretName: "registry-auth", Mode: ptr.To(int32(0o440)),
		Items: []sandboxv1alpha1.SandboxSecretFileItem{
			{Key: "config.json", Path: "/run/secrets/registry/config.json"},
			{Key: "ca.crt", Path: "/etc/ssl/app/ca.crt", Mode: ptr.To(int32(0o444))},
		},
	}}
	return sb
}

// Each item becomes one reference, with the mode resolved item > entry > 0400.
func TestSecretFileRefs_ResolveModes(t *testing.T) {
	sb := secretFilesSandbox("x")
	sb.Spec.SecretFiles = append(sb.Spec.SecretFiles, sandboxv1alpha1.SandboxSecretFile{
		SecretName: "tok", Optional: true, Items: []sandboxv1alpha1.SandboxSecretFileItem{{Key: "t", Path: "/run/t"}}})
	want := []runtimeintent.SecretFileRef{
		{Secret: "registry-auth", Key: "config.json", Path: "/run/secrets/registry/config.json", Mode: 0o440},
		{Secret: "registry-auth", Key: "ca.crt", Path: "/etc/ssl/app/ca.crt", Mode: 0o444},
		{Secret: "tok", Key: "t", Path: "/run/t", Mode: 0o400, Optional: true},
	}
	if got := secretFileRefs(sb); !reflect.DeepEqual(got, want) {
		t.Errorf("secretFileRefs = %+v", got)
	}
	if got := secretNames(sb); !reflect.DeepEqual(got, []string{"registry-auth", "tok"}) {
		t.Errorf("secretNames = %v", got)
	}
}

// A cold launch waits for a missing Secret, then carries references only,
// grants exactly the Secret, and keeps the config disk in memory.
func TestReconcile_SecretFilesColdLaunch(t *testing.T) {
	ctx := context.Background()
	sb := secretFilesSandbox(testImage(t))
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	cond := apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
	if cond == nil || cond.Reason != "SecretNotFound" || !strings.Contains(cond.Message, "secret file /run/secrets/registry/config.json") {
		t.Fatalf("want SecretNotFound naming the file, got %+v", cond)
	}
	if err := c.Create(ctx, registrySecret()); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "sb")
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatalf("launcher pod: %v", err)
	}
	if got := roleSecrets(t, c, "sb"); !reflect.DeepEqual(got, []string{"registry-auth"}) {
		t.Errorf("grant = %v", got)
	}
	mem := false
	for _, v := range pod.Spec.Volumes {
		mem = mem || (v.Name == secretRunVolume && v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory)
	}
	if !mem {
		t.Error("secret files need the memory-backed config disk volume")
	}
	var cm corev1.ConfigMap
	_ = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: intentConfigMapName(sb)}, &cm)
	b, _ := json.Marshal([]any{cm.Data, pod, getSandbox(t, c, "sb")})
	if strings.Contains(string(b), "s3cr3t") {
		t.Error("a secret file's content appears in an object the controller wrote")
	}
	if !strings.Contains(strings.Join(mapValues(cm.Data), ""), `"secretFiles"`) {
		t.Error("the intent must carry the file references")
	}
}

// The kernel's bridge lacks the files feature: the sandbox fails and says so.
func TestReconcile_KernelErrorFailsTheSandbox(t *testing.T) {
	sb := secretFilesSandbox("busybox:1")
	pod := runningLauncher(sb, map[string]string{annKernelError: "the sandbox kernel's bridge does not support secret files (feature 'files')"})
	r, c := sandboxReconciler(sb, registrySecret(), pod)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "KernelUnsupported" {
		t.Errorf("want Failed/KernelUnsupported, got %s %+v", got.Status.Phase, cond)
	}
}

// A checkout hands the file references over with the workload.
func TestCheckout_SecretFilesReferences(t *testing.T) {
	ctx := context.Background()
	pool := shapedPool()
	pool.Namespace = "default"
	slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
	slot.Namespace = "default"
	slot.Spec.ServiceAccountName = "kubeswift-sandbox-launcher-p-slot-aaaaa"
	sb := matchingSandbox()
	sb.Namespace = "default"
	sb.Spec.SecretFiles = secretFilesSandbox("x").Spec.SecretFiles
	r, c := sandboxReconciler(pool, slot, sb, registrySecret())
	reconcileSB(t, r, "sb")
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(slot), &p); err != nil {
		t.Fatal(err)
	}
	args := p.Annotations[annSandboxExecActionArgs]
	if !strings.Contains(args, `"secretFiles"`) || strings.Contains(args, "s3cr3t") {
		t.Errorf("action args must carry file references, never content: %s", args)
	}
	if got := roleSecrets(t, c, slot.Name); !reflect.DeepEqual(got, []string{"registry-auth"}) {
		t.Errorf("slot grant = %v", got)
	}
}

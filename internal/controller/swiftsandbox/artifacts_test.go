package swiftsandbox

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

// testArtifact pushes an ORAS-style artifact (non-image config, wasm layer)
// to an in-process registry and returns its tag reference and digest.
func testArtifact(t *testing.T) (string, string) {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	ref, _ := name.ParseReference(u.Host + "/team/hello-http:1")
	img := mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), "application/vnd.fermyon.spin.application.v1+config")
	img, err := mutate.Append(img, mutate.Addendum{Layer: static.NewLayer([]byte("\x00asm"), "application/vnd.wasm.content.layer.v1+wasm"),
		MediaType: "application/vnd.wasm.content.layer.v1+wasm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	return ref.String(), d.String()
}

func initNamed(pod *corev1.Pod, n string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == n {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

// A cold sandbox with an artifact records its digest, pulls exactly that
// digest in its own init container, shares the cache entry read-only, and asks
// the bridge to mount it.
func TestReconcile_ArtifactColdLaunch(t *testing.T) {
	ctx := context.Background()
	ref, digest := testArtifact(t)
	sb := plainSandbox(testImage(t))
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "app", Ref: ref, MountPath: "/run/artifacts/app"}}
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")

	got := getSandbox(t, c, "sb")
	if len(got.Status.Artifacts) != 1 || got.Status.Artifacts[0].Digest != digest || got.Status.Artifacts[0].MountPath != "/run/artifacts/app" {
		t.Fatalf("status.artifacts = %+v (phase %s: %s)", got.Status.Artifacts, got.Status.Phase, got.Status.Message)
	}
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatal(err)
	}
	ic := initNamed(&pod, "artifact-app")
	if ic == nil {
		t.Fatal("no artifact-app init container")
	}
	args := strings.Join(ic.Args, " ")
	repo := ref[:strings.LastIndex(ref, ":")]
	if !strings.Contains(args, "--image "+repo+"@"+digest) || !strings.Contains(args, "--mode oci") || ic.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("artifact init = %s (policy %s)", args, ic.TerminationMessagePolicy)
	}
	ro := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		ro = ro || (m.Name == artifactCacheVolume && m.ReadOnly)
	}
	if !ro {
		t.Error("the launcher must mount the artifact cache read-only")
	}
	var cm corev1.ConfigMap
	_ = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: intentConfigMapName(sb)}, &cm)
	var ri runtimeintent.RuntimeIntent
	for _, v := range cm.Data {
		_ = json.Unmarshal([]byte(v), &ri)
	}
	if ri.KernelBoot == nil || !strings.Contains(ri.KernelBoot.Cmdline, "kubeswift.mounts=sbxart0:/run/artifacts/app") {
		t.Errorf("cmdline = %+v", ri.KernelBoot)
	}
	found := false
	for _, fs := range ri.Filesystems {
		found = found || (fs.Tag == "sbxart0" && fs.ReadOnly && strings.HasSuffix(fs.SourcePath, strings.ReplaceAll(digest, ":", "-")+".oci"))
	}
	if !found {
		t.Errorf("filesystems = %+v, want sbxart0 read-only on the cached layout", ri.Filesystems)
	}
}

// An artifact that cannot be resolved fails the sandbox, naming it.
func TestReconcile_ArtifactResolveFailure(t *testing.T) {
	ref, _ := testArtifact(t)
	sb := plainSandbox(testImage(t))
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "app", Ref: strings.Replace(ref, "hello-http:1", "missing:1", 1), MountPath: "/a"}}
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "ArtifactResolveFailed" || !strings.Contains(got.Status.Message, "artifact app") {
		t.Errorf("got %s %+v %q", got.Status.Phase, cond, got.Status.Message)
	}
}

// A failed artifact init (pull or signature) fails the sandbox with its message.
func TestReconcile_ArtifactInitFailure(t *testing.T) {
	sb := plainSandbox("busybox:1")
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "app", Ref: "r.example/a@sha256:" + strings.Repeat("b", 64), MountPath: "/a"}}
	pod := buildPod(sb, "sandbox")
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
		{Name: "artifact-app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "sandbox-materialize: cosign verify: no matching signatures"}}}}}
	r, c := sandboxReconciler(sb, pod)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || !strings.Contains(got.Status.Message, "artifact app: sandbox-materialize: cosign verify") {
		t.Errorf("got %s %q", got.Status.Phase, got.Status.Message)
	}
}

// Its own pull Secret and cosign key reach the init container only.
func TestArtifactInits_SecretsAndKey(t *testing.T) {
	sb := plainSandbox("x")
	sb.Spec.ImagePullSecret = "default-pull"
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{
		{Name: "a", Ref: "r.example/a:1", MountPath: "/a", PullSecretRef: &corev1.LocalObjectReference{Name: "team-registry"},
			VerifyKeySecretRef: &sandboxv1alpha1.SecretObjectReference{Name: "team-cosign"}, Layout: "unpacked"},
		{Name: "b", Ref: "r.example/b:1", MountPath: "/b"},
	}
	sb.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{{Name: "a", Digest: "sha256:" + strings.Repeat("1", 64)}, {Name: "b", Digest: "sha256:" + strings.Repeat("2", 64)}}
	inits, vols := artifactInits(sb)
	if len(inits) != 2 {
		t.Fatalf("want 2 inits, got %d", len(inits))
	}
	a := strings.Join(inits[0].Args, " ")
	if !strings.Contains(a, "--mode tree") || !strings.Contains(a, "--pull-secret=/artifact-pull-0/config.json") || !strings.Contains(a, "--verify-key=/artifact-verify-0/cosign.pub") {
		t.Errorf("init a = %s", a)
	}
	if b := strings.Join(inits[1].Args, " "); !strings.Contains(b, "--pull-secret=/artifact-pull-1/config.json") || strings.Contains(b, "verify-key") {
		t.Errorf("init b = %s (falls back to imagePullSecret, no key)", b)
	}
	// Every artifact is cached with read-only modes an unprivileged guest
	// process can read.
	for _, c := range inits {
		if !strings.Contains(strings.Join(c.Args, " "), "--read-only-artifact") {
			t.Errorf("init %s lacks --read-only-artifact: %v", c.Name, c.Args)
		}
	}
	secrets := map[string]string{}
	for _, v := range vols {
		if v.Secret != nil {
			secrets[v.Name] = v.Secret.SecretName
		}
	}
	if secrets["artifact-pull-0"] != "team-registry" || secrets["artifact-verify-0"] != "team-cosign" || secrets["artifact-pull-1"] != "default-pull" {
		t.Errorf("secret volumes = %v", secrets)
	}
}

// Artifacts are not part of a slot's shape: any pool's slot can take them at
// checkout, so pools stay generic.
func TestSlotMismatches_ArtifactsAreNotShape(t *testing.T) {
	sb := matchingSandbox()
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "a", Ref: "r/a:1", MountPath: "/a"}}
	if got := slotMismatches(shapedPool(), sb); len(got) != 0 {
		t.Errorf("mismatches = %q", got)
	}
}

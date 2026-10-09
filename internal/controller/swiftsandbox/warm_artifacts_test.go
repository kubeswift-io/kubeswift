package swiftsandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

var testKey = []byte("-----BEGIN PUBLIC KEY-----\ntenant key\n-----END PUBLIC KEY-----\n")

func keySecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data: map[string][]byte{verifyKeyName: testKey}}
}

// warmSetup is a pool, one capable warm slot on node-a, and a sandbox asking
// for one signed artifact pushed to an in-process registry.
func warmSetup(t *testing.T) (*sandboxv1alpha1.SwiftSandboxPool, *corev1.Pod, *sandboxv1alpha1.SwiftSandbox, string, string) {
	t.Helper()
	ref, digest := testArtifact(t)
	pool := shapedPool()
	pool.Namespace = "default"
	slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
	slot.Namespace = "default"
	slot.Spec.NodeName = "node-a"
	slot.Annotations[bridgeFeaturesAnnotation] = "files mounts warm-mounts"
	sb := matchingSandbox()
	sb.Namespace = "default"
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "app", Ref: ref, MountPath: "/run/app",
		VerifyKeySecretRef: &sandboxv1alpha1.SecretObjectReference{Name: "app-key"}}}
	return pool, slot, sb, ref, digest
}

func actionArtifacts(t *testing.T, p *corev1.Pod) []warmArtifactArg {
	t.Helper()
	var args struct {
		Artifacts []warmArtifactArg `json:"artifacts"`
	}
	if err := json.Unmarshal([]byte(p.Annotations[annSandboxExecActionArgs]), &args); err != nil {
		t.Fatalf("action args: %v", err)
	}
	return args.Artifacts
}

// A sandbox with artifacts checks out a warm slot. The action lists each
// artifact by digest with the fingerprint of the sandbox's own key, and the
// sandbox records what it mounts.
func TestCheckout_ArtifactsTakeAWarmSlot(t *testing.T) {
	ctx := context.Background()
	pool, slot, sb, _, digest := warmSetup(t)
	r, c := sandboxReconciler(pool, slot, sb, keySecret("app-key"))
	reconcileSB(t, r, "sb")

	got := getSandbox(t, c, "sb")
	if got.Status.PodRef != slot.Name {
		t.Fatalf("want a checkout of %s, got %q (%s)", slot.Name, got.Status.PodRef, got.Status.Message)
	}
	if len(got.Status.Artifacts) != 1 || got.Status.Artifacts[0].Digest != digest {
		t.Errorf("status.artifacts = %+v", got.Status.Artifacts)
	}
	var p corev1.Pod
	_ = c.Get(ctx, client.ObjectKeyFromObject(slot), &p)
	arts := actionArtifacts(t, &p)
	want := warmArtifactArg{Name: "app", Digest: digest, Layout: "oci", MountPath: "/run/app", KeyFingerprint: materialize.KeyFingerprint(testKey)}
	if len(arts) != 1 || arts[0] != want {
		t.Errorf("action artifacts = %+v, want %+v", arts, want)
	}
	if strings.Contains(p.Annotations[annSandboxExecActionArgs], "tenant key") {
		t.Error("the key itself reached the slot")
	}
}

// A slot whose bridge cannot project artifacts (an older kernel, or a slot
// booted before staging shares) is never claimed for them.
func TestTryClaimWarmSlot_ArtifactsNeedWarmMounts(t *testing.T) {
	pool, slot, sb, _, _ := warmSetup(t)
	slot.Annotations[bridgeFeaturesAnnotation] = "files mounts"
	r, _ := sandboxReconciler(pool, slot, sb)
	got, err := r.tryClaimWarmSlot(context.Background(), sb, poolSlotProfile(pool))
	if err != nil || got != nil {
		t.Errorf("claimed %v (err %v) without warm-mounts", got, err)
	}
	sb.Spec.Artifacts = nil
	if got, _ := r.tryClaimWarmSlot(context.Background(), sb, poolSlotProfile(pool)); got == nil {
		t.Error("a sandbox without artifacts must still take that slot")
	}
}

// claimedWith returns sb checked out on slot, its first dispatch answered.
func claimedWith(sb *sandboxv1alpha1.SwiftSandbox, slot *corev1.Pod, digest, actionID, status, detail string) {
	now := metav1.Now()
	sb.Status.PodRef = slot.Name
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxRunning
	sb.Status.StartedAt = &now
	sb.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{{Name: "app", Digest: digest, MountPath: "/run/app"}}
	slot.Labels[SlotStateLabelKey] = slotStateClaimed
	slot.Annotations[annSandboxExecAction] = "run"
	slot.Annotations[annSandboxExecActionID] = actionID
	slot.Annotations[annSandboxExecActionArgs] = `{"argv":["/bin/true"]}`
	slot.Annotations[annSandboxExecStatusID] = actionID
	slot.Annotations[annSandboxExecStatus] = status
	slot.Annotations[annSandboxExecStatusDetail] = detail
}

// Artifacts missing on the slot's node: one fetch pod on that node, with the
// sandbox's own pull Secret and key, then the same action dispatched again.
// The slot is kept: its VM stays booted.
func TestClaimedSlot_ArtifactMissingFetchesThenDispatchesAgain(t *testing.T) {
	ctx := context.Background()
	_, slot, sb, ref, digest := warmSetup(t)
	sb.Spec.ImagePullSecret = "team-registry"
	claimedWith(sb, slot, digest, string(sb.UID), "failed", "ArtifactMissing: app")
	r, c := sandboxReconciler(slot, sb, keySecret("app-key"))
	reconcileSB(t, r, "sb")

	var fetch corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-artifacts"}, &fetch); err != nil {
		t.Fatalf("no fetch pod: %v (%s)", err, getSandbox(t, c, "sb").Status.Message)
	}
	if fetch.Spec.NodeName != "node-a" || len(fetch.Spec.Containers) != 1 || !metav1.IsControlledBy(&fetch, sb) {
		t.Fatalf("fetch pod = node %q, %d containers", fetch.Spec.NodeName, len(fetch.Spec.Containers))
	}
	args := strings.Join(fetch.Spec.Containers[0].Args, " ")
	repo := ref[:strings.LastIndex(ref, ":")]
	for _, want := range []string{"--image " + repo + "@" + digest, "--mode oci", "--read-only-artifact", "--verify-key=", "--pull-secret="} {
		if !strings.Contains(args, want) {
			t.Errorf("fetch args lack %q: %s", want, args)
		}
	}
	if *fetch.Spec.AutomountServiceAccountToken {
		t.Error("the fetch pod needs no API credentials")
	}
	if got := getSandbox(t, c, "sb"); got.Status.Phase != sandboxv1alpha1.SwiftSandboxRunning || !strings.Contains(got.Status.Message, "fetching artifacts app") {
		t.Errorf("while fetching: %s %q", got.Status.Phase, got.Status.Message)
	}

	fetch.Status.Phase = corev1.PodSucceeded
	if err := c.Status().Update(ctx, &fetch); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "sb")
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(slot), &p); err != nil {
		t.Fatalf("slot deleted: %v", err)
	}
	if p.Annotations[annSandboxExecActionID] != string(sb.UID)+fetchedActionSuffix {
		t.Fatalf("action-id = %q, want the second dispatch", p.Annotations[annSandboxExecActionID])
	}

	// The second dispatch completes.
	p.Annotations[annSandboxExecStatusID] = string(sb.UID) + fetchedActionSuffix
	p.Annotations[annSandboxExecStatus] = "complete"
	p.Annotations[annSandboxExecStatusDetail] = "0"
	if err := c.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "sb")
	if got := getSandbox(t, c, "sb"); got.Status.Phase != sandboxv1alpha1.SwiftSandboxCompleted {
		t.Errorf("phase = %s (%s)", got.Status.Phase, got.Status.Message)
	}
}

// A failed fetch (pull, signature, size) fails the sandbox with its reason.
func TestClaimedSlot_ArtifactFetchFailureFails(t *testing.T) {
	ctx := context.Background()
	_, slot, sb, _, digest := warmSetup(t)
	claimedWith(sb, slot, digest, string(sb.UID), "failed", "ArtifactMissing: app")
	r, c := sandboxReconciler(slot, sb, keySecret("app-key"))
	reconcileSB(t, r, "sb")
	var fetch corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-artifacts"}, &fetch); err != nil {
		t.Fatal(err)
	}
	fetch.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{Name: "artifact-app",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "sandbox-materialize: cosign verify: no matching signatures"}}}}}
	_ = c.Status().Update(ctx, &fetch)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "ArtifactMaterializeFailed" ||
		!strings.Contains(got.Status.Message, "artifact app: sandbox-materialize: cosign verify") {
		t.Errorf("got %s %+v", got.Status.Phase, cond)
	}
}

// Missing again after a fetch: fail, do not loop.
func TestClaimedSlot_SecondMissFails(t *testing.T) {
	_, slot, sb, _, digest := warmSetup(t)
	claimedWith(sb, slot, digest, string(sb.UID)+fetchedActionSuffix, "failed", "ArtifactMissing: app")
	r, c := sandboxReconciler(slot, sb)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "ArtifactMissing" {
		t.Errorf("got %s %+v", got.Status.Phase, cond)
	}
}

// Tenant B cannot use digest X because tenant A could and X is on the node:
// every checkout is authorized with the checking-out sandbox's own
// credentials, and a refusal fails it before any slot is claimed.
func TestCheckout_ArtifactAuthorizationIsPerCredentials(t *testing.T) {
	ctx := context.Background()
	pool, slot, sb, _, digest := warmSetup(t)
	sb.Spec.Artifacts[0].VerifyKeySecretRef = nil
	sb.Spec.ImagePullSecret = "tenant-b"
	secret := func(name, user string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"` + registryHost(sb.Spec.Artifacts[0].Ref) + `":{"username":"` + user + `","password":"p"}}}`)}}
	}
	var asked []string
	res := stubResolver(func(_ context.Context, _ resolveKind, o materialize.Options) resolveResult {
		cfg, _ := o.Auth.Authorization()
		asked = append(asked, cfg.Username)
		if cfg.Username == "alice" {
			return resolveResult{digest: digest}
		}
		return resolveResult{err: &transport.Error{StatusCode: http.StatusForbidden}}
	})
	r, c := sandboxReconciler(pool, slot, sb, secret("tenant-a", "alice"), secret("tenant-b", "bob"))
	r.resolver = res
	// Tenant A's answer is in the resolver.
	resolveArtifacts(ctx, c, &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{ImagePullSecret: "tenant-a", Artifacts: sb.Spec.Artifacts}}, r.lookup(sb))

	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "ArtifactResolveFailed" {
		t.Fatalf("got %s %+v", got.Status.Phase, cond)
	}
	if len(asked) != 2 || asked[1] != "bob" {
		t.Errorf("registry asked as %v, want alice then bob", asked)
	}
	var p corev1.Pod
	_ = c.Get(ctx, client.ObjectKeyFromObject(slot), &p)
	if p.Labels[SlotStateLabelKey] != slotStateWarm {
		t.Error("a slot was claimed for a refused sandbox")
	}
}

// A digest reference's authorization is kept 5 minutes, a tag's 30 seconds.
func TestResolver_DigestAuthorizationTTL(t *testing.T) {
	calls := map[string]int{}
	r := stubResolver(func(_ context.Context, _ resolveKind, o materialize.Options) resolveResult {
		calls[o.ImageRef]++
		return resolveResult{digest: "sha256:aa"}
	})
	now := time.Now()
	r.now = func() time.Time { return now }
	dig := "reg.example/a@sha256:" + strings.Repeat("a", 64)
	tag := "reg.example/a:1"
	for _, ref := range []string{dig, tag} {
		r.get(req(ref, nil), waiter{})
	}
	now = now.Add(time.Minute)
	for _, ref := range []string{dig, tag} {
		r.get(req(ref, nil), waiter{})
	}
	if calls[dig] != 1 || calls[tag] != 2 {
		t.Errorf("after 1 min: digest asked %d, tag %d; want 1, 2", calls[dig], calls[tag])
	}
	now = now.Add(5 * time.Minute)
	r.get(req(dig, nil), waiter{})
	if calls[dig] != 2 {
		t.Errorf("after 6 min the digest was not asked again")
	}
}

// Every warm slot boots with the staging share: empty, read-only in the
// guest, its source an emptyDir the launcher writes, next to the node cache
// mounted read-only.
func TestWarmSlot_HasAStagingShare(t *testing.T) {
	slot := (&SwiftSandboxPoolReconciler{}).slotTemplate(shapedPool(), "p-slot-x")
	intent := buildIntent(slot, "sandbox", "/rootfs", "", execSpec{}, true)
	found := false
	for _, fs := range intent.Filesystems {
		found = found || (fs == warmStageIntent() && fs.ReadOnly)
	}
	if !found || !strings.Contains(intent.KernelBoot.Cmdline, "kubeswift.stage=sbxstage") {
		t.Errorf("filesystems %+v cmdline %q", intent.Filesystems, intent.KernelBoot.Cmdline)
	}
	if cold := buildIntent(slot, "sandbox", "/rootfs", "", execSpec{}, false); strings.Contains(cold.KernelBoot.Cmdline, "kubeswift.stage") {
		t.Error("a cold sandbox got a staging share")
	}
	pod := buildPod(slot, "sandbox")
	addWarmStage(pod)
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		mounts[m.Name] = m
	}
	if m, ok := mounts[warmStageVolume]; !ok || m.ReadOnly || m.MountPath != warmStageDir {
		t.Errorf("stage mount = %+v", m)
	}
	if m, ok := mounts[artifactCacheVolume]; !ok || !m.ReadOnly {
		t.Errorf("cache mount = %+v, want read-only", m)
	}
}

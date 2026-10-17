package swiftsandbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/artifactpolicy"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func policyKey(t *testing.T) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// verificationPolicy governs every repository in namespaces labelled
// trust=platform, with one key and a fromNamespace claim.
func verificationPolicy(t *testing.T, opts ...func(*sandboxv1alpha1.SwiftArtifactVerificationPolicy)) *sandboxv1alpha1.SwiftArtifactVerificationPolicy {
	p := &sandboxv1alpha1.SwiftArtifactVerificationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "platform", UID: "policy-uid", Generation: 3},
		Spec: sandboxv1alpha1.SwiftArtifactVerificationPolicySpec{
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"trust": "platform"}},
			Repositories:      []string{"**"},
			Authorities:       []sandboxv1alpha1.ArtifactAuthority{{Name: "release", Key: &sandboxv1alpha1.KeyAuthority{PublicKey: policyKey(t)}}},
			Claims:            []sandboxv1alpha1.ArtifactClaimRequirement{{Key: "example.com/consumer", FromNamespace: true}},
		},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func trustedNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"trust": "platform"}}}
}

func pinnedRef(ref, digest string) string { return ref[:strings.LastIndex(ref, ":")] + "@" + digest }

func requirementsEnv(t *testing.T, c *corev1.Container) []materialize.Requirement {
	t.Helper()
	for _, e := range c.Env {
		if e.Name == materialize.RequirementsEnv {
			var reqs []materialize.Requirement
			if err := json.Unmarshal([]byte(e.Value), &reqs); err != nil {
				t.Fatal(err)
			}
			return reqs
		}
	}
	return nil
}

// Cold start: the governed artifact's materializer gets the policy's
// requirement in its pod spec, with the claim resolved for this namespace,
// and status names the policy.
func TestPolicy_ColdLaunchCarriesRequirements(t *testing.T) {
	ref, digest := testArtifact(t)
	sb := plainSandbox(testImage(t))
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "app", Ref: pinnedRef(ref, digest), MountPath: "/run/app"}}
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile), trustedNamespace(), verificationPolicy(t))
	reconcileSB(t, r, "sb")

	got := getSandbox(t, c, "sb")
	if len(got.Status.Artifacts) != 1 || !reflect.DeepEqual(got.Status.Artifacts[0].Policies, []sandboxv1alpha1.ArtifactPolicyReference{{Name: "platform", Generation: 3}}) {
		t.Fatalf("status.artifacts = %+v (%s: %s)", got.Status.Artifacts, got.Status.Phase, got.Status.Message)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatal(err)
	}
	ic := initNamed(&pod, "artifact-app")
	reqs := requirementsEnv(t, ic)
	if len(reqs) != 1 || reqs[0].UID != "policy-uid" || len(reqs[0].Keys) != 1 ||
		!reflect.DeepEqual(reqs[0].Claims, []materialize.RequiredClaim{{Key: "example.com/consumer", Values: []string{"default"}}}) {
		t.Fatalf("requirements = %+v", reqs)
	}
	if !hasEnv(ic, "HOME") || !podHasVolume(&pod, artifactCosignHomeName) {
		t.Error("cosign has no writable home")
	}
}

// No policy, no namespace label: nothing changes (v0.17 behaviour).
func TestPolicy_UngovernedIsUnchanged(t *testing.T) {
	ref, digest := testArtifact(t)
	sb := plainSandbox(testImage(t))
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "app", Ref: ref, MountPath: "/run/app"}}
	ns := trustedNamespace()
	ns.Labels = nil
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile), ns, verificationPolicy(t))
	reconcileSB(t, r, "sb")
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatal(err)
	}
	if reqs := requirementsEnv(t, initNamed(&pod, "artifact-app")); reqs != nil {
		t.Errorf("an ungoverned artifact got requirements %+v", reqs)
	}
	if got := getSandbox(t, c, "sb"); got.Status.Artifacts[0].Digest != digest || len(got.Status.Artifacts[0].Policies) != 0 {
		t.Errorf("status.artifacts = %+v", got.Status.Artifacts)
	}
}

// A policy refusal fails a required artifact before anything is fetched,
// and skips an optional one.
func TestPolicy_DeniedRequiredFailsOptionalSkips(t *testing.T) {
	ref, _ := testArtifact(t)
	// A tag under digestPinning: Required.
	sb := plainSandbox(testImage(t))
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "app", Ref: ref, MountPath: "/run/app"}}
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile), trustedNamespace(), verificationPolicy(t))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "ArtifactPolicyDenied" || !strings.Contains(got.Status.Message, artifactpolicy.DeniedTagReference) {
		t.Fatalf("phase %s cond %+v msg %s", got.Status.Phase, cond, got.Status.Message)
	}
	if len(got.Status.Artifacts) != 1 || got.Status.Artifacts[0].State != sandboxv1alpha1.SandboxArtifactFailed || got.Status.Artifacts[0].Reason != "ArtifactPolicyDenied" {
		t.Errorf("status.artifacts = %+v", got.Status.Artifacts)
	}
	if !strings.Contains(drainEvents(r), "ArtifactPolicyDenied") {
		t.Error("no ArtifactPolicyDenied Event")
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err == nil {
		t.Error("a launcher pod was created for a refused artifact")
	}

	opt := plainSandbox(testImage(t))
	opt.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "app", Ref: ref, MountPath: "/run/app", Optional: true}}
	r, c = sandboxReconciler(opt, readyKernel("default", defaultKernelProfile), trustedNamespace(), verificationPolicy(t))
	reconcileSB(t, r, "sb")
	got = getSandbox(t, c, "sb")
	if got.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed || len(got.Status.Artifacts) != 1 ||
		got.Status.Artifacts[0].State != sandboxv1alpha1.SandboxArtifactSkipped || got.Status.Artifacts[0].Reason != "PolicyDenied" {
		t.Fatalf("optional: phase %s, status.artifacts = %+v", got.Status.Phase, got.Status.Artifacts)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatal(err)
	}
	if initNamed(&pod, "artifact-app") != nil {
		t.Error("a skipped artifact is still materialized")
	}
	if !strings.Contains(drainEvents(r), "ArtifactPolicyDenied") {
		t.Error("no ArtifactPolicyDenied Event for the skip")
	}
}

// requirePlatformVerification with no policy anywhere: refused, not mounted
// with only the tenant's own checks.
func TestPolicy_RequirePlatformVerificationWithoutPolicy(t *testing.T) {
	ref, digest := testArtifact(t)
	sb := plainSandbox(testImage(t))
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "app", Ref: pinnedRef(ref, digest), MountPath: "/run/app", RequirePlatformVerification: true}}
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || !strings.Contains(got.Status.Message, artifactpolicy.DeniedNoMatchingPolicy) {
		t.Fatalf("phase %s msg %s", got.Status.Phase, got.Status.Message)
	}
}

// A materializer that found the platform key's signature without the
// claims fails the sandbox as ArtifactClaimMismatch.
func TestPolicy_FailureReasons(t *testing.T) {
	for msg, want := range map[string]string{
		"artifact app: ClaimMismatch: policy p: signed by authority a, but claim k is missing": "ArtifactClaimMismatch",
		"artifact app: VerificationFailed: policy p: authority a: no signature":                "ArtifactVerificationFailed",
		"artifact app: cosign verify x: exit status 1":                                         "ArtifactMaterializeFailed",
		"artifact app: ClaimMismatch without a policy":                                         "ArtifactMaterializeFailed",
	} {
		if got := policyFailureReason(msg, "ArtifactMaterializeFailed"); got != want {
			t.Errorf("%q -> %s, want %s", msg, got, want)
		}
	}
}

// Warm checkout: a governed artifact goes only to a launcher that enforces
// requirements, and its action carries the acceptable records.
func TestPolicy_WarmCheckoutNeedsAnEnforcingLauncher(t *testing.T) {
	pool, slot, sb, ref, digest := warmSetup(t)
	sb.Spec.Artifacts[0].Ref = pinnedRef(ref, digest)
	sb.Spec.Artifacts[0].VerifyKeySecretRef = nil
	policy := verificationPolicy(t)

	// The slot's launcher predates requirements: the checkout boots cold.
	r, c := sandboxReconciler(pool, slot, sb, trustedNamespace(), policy, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	if got := getSandbox(t, c, "sb"); got.Status.PodRef == slot.Name {
		t.Fatal("a governed artifact was sent to a launcher that ignores requirements")
	}
	if ev := drainEvents(r); !strings.Contains(ev, "platform verification policy") {
		t.Errorf("events %q do not say why the checkout went cold", ev)
	}

	slot.Annotations[launcherFeaturesAnnotation] = artifactRequirementsFeature
	r, c = sandboxReconciler(pool, slot, sb, trustedNamespace(), policy, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	if got := getSandbox(t, c, "sb"); got.Status.PodRef != slot.Name {
		t.Fatalf("not checked out: %s %s", got.Status.Phase, got.Status.Message)
	}
	var p corev1.Pod
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(slot), &p)
	arts := actionArtifacts(t, &p)
	d := artifactpolicy.Decide([]*artifactpolicy.Compiled{artifactpolicy.Compile(policy)}, "default", trustedNamespace().Labels,
		artifactpolicy.Artifact{Ref: pinnedRef(ref, digest)})
	want, _ := d.AcceptableRecords(digest)
	if len(arts) != 1 || !reflect.DeepEqual(arts[0].Requirements, want) || len(want) != 1 || len(want[0]) != 1 {
		t.Errorf("action requirements %v, want %v", arts, want)
	}
}

// The fetch pod a warm miss starts verifies under the policies too.
func TestPolicy_FetchPodCarriesRequirements(t *testing.T) {
	pool, slot, sb, ref, digest := warmSetup(t)
	sb.Spec.Artifacts[0].Ref = pinnedRef(ref, digest)
	sb.Spec.Artifacts[0].VerifyKeySecretRef = nil
	slot.Annotations[launcherFeaturesAnnotation] = artifactRequirementsFeature
	r, c := sandboxReconciler(pool, slot, sb, trustedNamespace(), verificationPolicy(t), readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	var s corev1.Pod
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(slot), &s)
	if _, _, _, err := r.ensureArtifactFetch(context.Background(), got, &s, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	var fp corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: artifactFetchPodName(got)}, &fp); err != nil {
		t.Fatal(err)
	}
	if reqs := requirementsEnv(t, &fp.Spec.Containers[0]); len(reqs) != 1 || reqs[0].UID != "policy-uid" {
		t.Errorf("fetch pod requirements = %+v", reqs)
	}
}

func drainEvents(r *SwiftSandboxReconciler) string {
	rec, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		return ""
	}
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}

func TestPolicyStatus(t *testing.T) {
	good := verificationPolicy(t)
	bad := verificationPolicy(t, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Name, p.UID = "broken", "broken-uid"
		p.Spec.Authorities[0].Key.PublicKey = "-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n"
	})
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"}}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(good, bad, trustedNamespace(), other).
		WithStatusSubresource(&sandboxv1alpha1.SwiftArtifactVerificationPolicy{}).Build()
	rec := record.NewFakeRecorder(10)
	r := &ArtifactPolicyStatusReconciler{Client: c, Scheme: scheme.Scheme, Recorder: rec}
	for _, n := range []string{"platform", "broken"} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: n}}); err != nil {
			t.Fatal(err)
		}
	}
	var p sandboxv1alpha1.SwiftArtifactVerificationPolicy
	_ = c.Get(context.Background(), types.NamespacedName{Name: "platform"}, &p)
	cond := apimeta.FindStatusCondition(p.Status.Conditions, sandboxv1alpha1.VerificationPolicyConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || p.Status.Namespaces != 1 || len(p.Status.Authorities) != 1 ||
		len(p.Status.Authorities[0].KeyID) != 64 || p.Status.Authorities[0].Algorithm != "ECDSA-P256" || p.Status.ObservedGeneration != 3 {
		t.Errorf("good policy status %+v", p.Status)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "broken"}, &p)
	cond = apimeta.FindStatusCondition(p.Status.Conditions, sandboxv1alpha1.VerificationPolicyConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != sandboxv1alpha1.VerificationPolicyReasonInvalidKey || !strings.Contains(cond.Message, "private key") {
		t.Errorf("broken policy condition %+v", cond)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "InvalidKey") {
			t.Errorf("event %s", e)
		}
	default:
		t.Error("no Event for an invalid policy")
	}
	// Reconciling again changes nothing and says nothing.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "broken"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-rec.Events:
		t.Errorf("repeated event %s", e)
	default:
	}
}

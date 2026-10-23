package swiftsandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// #843: after optional artifacts are skipped the workload is dispatched again
// as <uid>-<n>; its readiness verdict is this sandbox's, and only the verdict
// of the dispatch now on the slot counts.
func TestGateReadiness_RedispatchAfterSkip(t *testing.T) {
	sb := probedSandbox()
	sb.UID = types.UID("uid-b")
	cases := []struct {
		name, action, msg string
		want              bool
	}{
		{"second dispatch, optional artifact skipped", "uid-b-2", "workload uid-b-2: ok", true},
		{"third dispatch", "uid-b-3", "workload uid-b-3: ok", true},
		{"the first dispatch's verdict after a re-dispatch", "uid-b-2", "workload uid-b: ok", false},
		{"a later dispatch's verdict on an earlier action", "uid-b-2", "workload uid-b-3: ok", false},
		{"another sandbox's re-dispatch", "uid-a-2", "workload uid-a-2: ok", false},
		{"not a dispatch number", "uid-b-x", "workload uid-b-x: ok", false},
	}
	for _, tc := range cases {
		pod := gatedLauncher(sb, tc.action, &corev1.PodCondition{Type: WorkloadReadyGate, Status: corev1.ConditionTrue, Message: tc.msg})
		ready, reason, msg := workloadReadiness(sb, pod)
		if ready != tc.want {
			t.Errorf("%s: ready = %v (%s %q), want %v", tc.name, ready, reason, msg, tc.want)
		}
	}
}

// #843: the controller keeps, not clears, the launcher's True verdict for a
// re-dispatched workload, and WorkloadReady follows it.
func TestReconcile_RedispatchedWorkloadBecomesReady(t *testing.T) {
	sb := probedSandbox()
	sb.UID = types.UID("uid-b")
	pod := gatedLauncher(sb, "uid-b-2", &corev1.PodCondition{Type: WorkloadReadyGate, Status: corev1.ConditionTrue, Message: "workload uid-b-2: http GET 200"})
	r, c := sandboxReconciler(sb, pod)
	if err := r.syncWorkloadReady(context.Background(), sb, pod); err != nil {
		t.Fatal(err)
	}
	if gate, _ := podGate(t, c, "sb"); gate != corev1.ConditionTrue {
		t.Errorf("gate = %q: the re-dispatched workload's verdict was cleared", gate)
	}
	cond := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("WorkloadReady %+v, want True", cond)
	}
}

// credentialFixture resolves sb's artifacts against a fake API server holding
// objs, counting the registry requests made per reference.
func credentialFixture(t *testing.T, sb *sandboxv1alpha1.SwiftSandbox, objs ...client.Object) (artifactResolution, map[string]int) {
	t.Helper()
	var mu sync.Mutex
	asked := map[string]int{}
	r := stubResolver(func(_ context.Context, _ resolveKind, o materialize.Options) resolveResult {
		mu.Lock()
		asked[o.ImageRef]++
		mu.Unlock()
		return resolveResult{digest: "sha256:" + strings.Repeat("a", 64)}
	})
	r.timeout = 5 * time.Second
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	reqs, err := artifactRequests(context.Background(), c, sb)
	if err != nil {
		t.Fatal(err)
	}
	rec := &SwiftSandboxReconciler{resolver: r}
	res := resolveArtifactRequests(sb, reqs, rec.lookup(sb), skipUnresolvedOptional, time.Now(), nil)
	mu.Lock()
	defer mu.Unlock()
	out := map[string]int{}
	for k, v := range asked {
		out[k] = v
	}
	return res, out
}

func secretOf(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Data: data}
}

// #844: an optional artifact whose pull Secret or verify key cannot be used is
// skipped with the reason, and never asked of the registry (no anonymous
// stand-in). The same failure on a required artifact fails the sandbox.
func TestArtifactCredentials_OptionalSkipsRequiredFails(t *testing.T) {
	withPull := func(a sandboxv1alpha1.SandboxArtifact, secret string) sandboxv1alpha1.SandboxArtifact {
		a.PullSecretRef = &corev1.LocalObjectReference{Name: secret}
		return a
	}
	withKey := func(a sandboxv1alpha1.SandboxArtifact, secret string) sandboxv1alpha1.SandboxArtifact {
		a.VerifyKeySecretRef = &sandboxv1alpha1.SecretObjectReference{Name: secret}
		return a
	}
	notDocker := secretOf("not-docker", map[string][]byte{"x": []byte("y")})
	noKey := secretOf("no-key", map[string][]byte{"other": []byte("y")})
	for _, tc := range []struct {
		name     string
		artifact sandboxv1alpha1.SandboxArtifact
		objs     []client.Object
		reason   string
	}{
		{"pull Secret missing", withPull(art("cache", "reg.example/cache:1", true), "missing"), nil, reasonPullSecretInvalid},
		{"pull Secret not a docker config", withPull(art("cache", "reg.example/cache:1", true), "not-docker"), []client.Object{notDocker}, reasonPullSecretInvalid},
		{"verify key Secret missing", withKey(art("cache", "reg.example/cache:1", true), "missing-key"), nil, reasonVerifyKeyInvalid},
		{"verify key Secret without cosign.pub", withKey(art("cache", "reg.example/cache:1", true), "no-key"), []client.Object{noKey}, reasonVerifyKeyInvalid},
	} {
		// Optional: skipped, the other artifact still resolves, nothing asked.
		sb := optSandbox(tc.artifact, art("other", "reg.example/other:1", false))
		res, asked := credentialFixture(t, sb, tc.objs...)
		if res.err != nil || !res.ready {
			t.Fatalf("%s (optional): err %v ready %v", tc.name, res.err, res.ready)
		}
		if st, reason := stateOf(res, "cache"); st != sandboxv1alpha1.SandboxArtifactSkipped || reason != tc.reason {
			t.Errorf("%s (optional): %s/%s, want Skipped/%s", tc.name, st, reason, tc.reason)
		}
		if st, _ := stateOf(res, "other"); st != sandboxv1alpha1.SandboxArtifactResolved {
			t.Errorf("%s (optional): the other artifact is %s", tc.name, st)
		}
		if asked["reg.example/cache:1"] != 0 {
			t.Errorf("%s (optional): the registry was asked for it without its credentials", tc.name)
		}
		for _, s := range res.statuses {
			if s.Name == "cache" && (strings.Contains(s.Message, "{") || !strings.Contains(s.Message, "Secret")) {
				t.Errorf("%s (optional): message %q", tc.name, s.Message)
			}
		}

		// Required: the pull Secret failure fails the sandbox as in v0.19.1;
		// a required artifact's verify key is not checked here (unchanged).
		req := tc.artifact
		req.Optional = false
		sb = optSandbox(req)
		res, asked = credentialFixture(t, sb, tc.objs...)
		if tc.reason == reasonPullSecretInvalid {
			if res.err == nil || !registryRefused(res.err) || !strings.Contains(res.err.Error(), "artifact cache: pull secret") {
				t.Errorf("%s (required): err %v, want the v0.19.1 refusal", tc.name, res.err)
			}
			if len(res.failed) != 1 || res.failed[0].State != sandboxv1alpha1.SandboxArtifactFailed || res.failed[0].Reason != "ArtifactResolveFailed" {
				t.Errorf("%s (required): failed statuses %+v", tc.name, res.failed)
			}
			if asked["reg.example/cache:1"] != 0 {
				t.Errorf("%s (required): the registry was asked without its credentials", tc.name)
			}
		} else if res.err != nil {
			t.Errorf("%s (required): err %v, want unchanged (no check)", tc.name, res.err)
		}
	}
}

// #844 end to end on a cold sandbox: the optional artifact is skipped, a
// Warning Event says why, and the launcher is created without it. The same
// sandbox with the artifact required fails, as in v0.19.1.
func TestReconcile_ColdOptionalArtifactWithMissingPullSecret(t *testing.T) {
	image := testImage(t)
	for _, optional := range []bool{true, false} {
		sb := plainSandbox(image)
		a := art("cache", image, optional)
		a.Layout = "unpacked"
		a.PullSecretRef = &corev1.LocalObjectReference{Name: "missing"}
		sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{a}
		r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
		rec := record.NewFakeRecorder(20)
		r.Recorder = rec
		for i := 0; i < 3; i++ {
			reconcileSB(t, r, "sb")
		}
		got := getSandbox(t, c, "sb")
		var pod corev1.Pod
		podErr := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod)
		if !optional {
			cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
			if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "ArtifactResolveFailed" {
				t.Errorf("required: phase %s cond %+v, want Failed/ArtifactResolveFailed", got.Status.Phase, cond)
			}
			if podErr == nil {
				t.Error("required: a launcher was created")
			}
			continue
		}
		if got.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed {
			t.Fatalf("optional: the sandbox failed: %s", got.Status.Message)
		}
		if len(got.Status.Artifacts) != 1 || got.Status.Artifacts[0].State != sandboxv1alpha1.SandboxArtifactSkipped ||
			got.Status.Artifacts[0].Reason != reasonPullSecretInvalid {
			t.Errorf("optional: artifacts %+v", got.Status.Artifacts)
		}
		if podErr != nil {
			t.Fatalf("optional: no launcher: %v", podErr)
		}
		for _, ic := range pod.Spec.InitContainers {
			if strings.HasPrefix(ic.Name, artifactInitPrefix) {
				t.Errorf("optional: the skipped artifact is still materialized (%s)", ic.Name)
			}
		}
		events := drain(rec)
		if n := strings.Count(events, "ArtifactSkipped"); n != 1 {
			t.Errorf("optional: %d ArtifactSkipped events over 3 reconciles, want 1: %s", n, events)
		}
	}
}

func drain(rec *record.FakeRecorder) string {
	var b strings.Builder
	for {
		select {
		case e := <-rec.Events:
			b.WriteString(e + "\n")
		default:
			return b.String()
		}
	}
}

// A status that still says Dispatched for an artifact the slot's current
// dispatch no longer carries is never turned into Mounted: the workload ran
// without it.
func TestArtifactsMounted_OnlyWhatTheSlotDispatched(t *testing.T) {
	_, slot, sb, _, digest := warmSetup(t)
	claimedWith(sb, slot, digest, actionIDFor(sb, 2), "", "")
	sb.Status.Artifacts[0].State = sandboxv1alpha1.SandboxArtifactDispatched
	slot.Annotations[annSandboxExecActionArgs] = `{"argv":["/bin/true"]}`
	r, _ := sandboxReconciler(slot, sb)
	r.artifactsMounted(sb, slot)
	if s := sb.Status.Artifacts[0]; s.State == sandboxv1alpha1.SandboxArtifactMounted {
		t.Errorf("reported Mounted although the dispatch did not carry it: %+v", s)
	}
	slot.Annotations[annSandboxExecActionArgs] = `{"argv":["/bin/true"],"artifacts":[{"name":"app","digest":"` + digest + `","layout":"oci","mountPath":"/run/app"}]}`
	r.artifactsMounted(sb, slot)
	if s := sb.Status.Artifacts[0]; s.State != sandboxv1alpha1.SandboxArtifactMounted {
		t.Errorf("a dispatched artifact was not marked Mounted: %+v", s)
	}
}

// A re-dispatch whose status write fails leaves the slot as it was, so the
// skip is decided again instead of being lost while the workload runs
// without the artifact.
func TestRedispatch_StatusBeforeSlot(t *testing.T) {
	slot, sb, _ := optionalSetup(t, 0)
	objs := []client.Object{slot, sb, keySecret("app-key")}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			return apierrors.NewConflict(schema.GroupResource{Resource: "swiftsandboxes"}, obj.GetName(), errors.New("injected"))
		}}).Build()
	r := &SwiftSandboxReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(20)}
	_, _ = r.Reconcile(context.Background(), ctrlRequestFor("sb"))
	p := slotNow(t, c, slot)
	if p.Annotations[annSandboxExecActionID] != string(sb.UID) {
		t.Errorf("the slot was re-dispatched (%s) although the status write failed", p.Annotations[annSandboxExecActionID])
	}
}

func ctrlRequestFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: name}}
}

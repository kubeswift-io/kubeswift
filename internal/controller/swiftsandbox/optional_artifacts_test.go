package swiftsandbox

import (
	"context"
	"encoding/json"
	"errors"
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

// resolveWith runs resolveArtifactRequests with canned answers per ref:
// "ok" resolves, "refused" is a 403, "down" a transient error, "slow" never
// answers within the pass.
func resolveWith(t *testing.T, sb *sandboxv1alpha1.SwiftSandbox, wait optionalWait, now time.Time) artifactResolution {
	t.Helper()
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	r := stubResolver(func(_ context.Context, _ resolveKind, o materialize.Options) resolveResult {
		switch {
		case strings.Contains(o.ImageRef, "/refused"):
			return resolveResult{err: &transport.Error{StatusCode: http.StatusForbidden}}
		case strings.Contains(o.ImageRef, "/down"):
			return resolveResult{err: errors.New("connection refused")}
		case strings.Contains(o.ImageRef, "/slow"):
			<-gate
		}
		return resolveResult{digest: "sha256:" + strings.Repeat("a", 64)}
	})
	r.timeout = 5 * time.Second
	reqs, err := artifactRequests(context.Background(), nil, sb)
	if err != nil {
		t.Fatal(err)
	}
	rec := &SwiftSandboxReconciler{resolver: r}
	return resolveArtifactRequests(sb, reqs, rec.lookup(sb), wait, now)
}

func optSandbox(arts ...sandboxv1alpha1.SandboxArtifact) *sandboxv1alpha1.SwiftSandbox {
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default", UID: "uid-s",
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute))}}
	sb.Spec.Artifacts = arts
	return sb
}

func art(name, ref string, optional bool) sandboxv1alpha1.SandboxArtifact {
	return sandboxv1alpha1.SandboxArtifact{Name: name, Ref: ref, MountPath: "/" + name, Optional: optional}
}

func stateOf(res artifactResolution, name string) (sandboxv1alpha1.SandboxArtifactState, string) {
	for _, s := range res.statuses {
		if s.Name == name {
			return s.State, s.Reason
		}
	}
	return "", ""
}

// An optional artifact never fails or delays a required one; a required one
// behaves as it always has.
func TestResolve_OptionalClassification(t *testing.T) {
	now := time.Now()
	t.Run("optional refused is skipped, required resolved", func(t *testing.T) {
		res := resolveWith(t, optSandbox(art("req", "r.example/ok:1", false), art("opt", "r.example/refused:1", true)), skipUnresolvedOptional, now)
		if res.err != nil || !res.ready {
			t.Fatalf("res=%+v", res)
		}
		if st, why := stateOf(res, "opt"); st != sandboxv1alpha1.SandboxArtifactSkipped || why != "ResolveRefused" {
			t.Errorf("opt = %s %s", st, why)
		}
		if st, _ := stateOf(res, "req"); st != sandboxv1alpha1.SandboxArtifactResolved {
			t.Errorf("req = %s", st)
		}
	})
	t.Run("required refused still fails", func(t *testing.T) {
		res := resolveWith(t, optSandbox(art("req", "r.example/refused:1", false), art("opt", "r.example/ok:1", true)), skipUnresolvedOptional, now)
		if res.err == nil || !registryRefused(res.err) {
			t.Fatalf("res=%+v", res)
		}
	})
	t.Run("warm: optional unanswered is skipped, not waited for", func(t *testing.T) {
		res := resolveWith(t, optSandbox(art("opt", "r.example/slow:1", true)), skipUnresolvedOptional, now)
		if !res.ready {
			t.Fatal("waited for an optional artifact at checkout")
		}
		if st, why := stateOf(res, "opt"); st != sandboxv1alpha1.SandboxArtifactSkipped || why != "ResolvePending" {
			t.Errorf("opt = %s %s", st, why)
		}
	})
	t.Run("cold: optional transient is waited for until its deadline", func(t *testing.T) {
		sb := optSandbox(art("opt", "r.example/down:1", true))
		now := sb.CreationTimestamp.Time
		res := resolveWith(t, sb, waitUnresolvedOptional, now)
		if res.ready || res.err != nil || res.wakeAt.IsZero() || !res.wakeAt.Equal(artifactDeadline(sb, &sb.Spec.Artifacts[0])) {
			t.Fatalf("before the deadline: %+v", res)
		}
		res = resolveWith(t, sb, waitUnresolvedOptional, now.Add(time.Minute))
		if st, why := stateOf(res, "opt"); !res.ready || st != sandboxv1alpha1.SandboxArtifactSkipped || why != "RegistryUnavailable" {
			t.Errorf("after the deadline: %+v", res)
		}
	})
	t.Run("required unanswered keeps waiting", func(t *testing.T) {
		res := resolveWith(t, optSandbox(art("req", "r.example/slow:1", false)), skipUnresolvedOptional, now)
		if res.ready || res.err != nil {
			t.Fatalf("res=%+v", res)
		}
	})
}

// A skipped artifact is never mounted: no share, no init container, no
// action entry, whatever digest its status carries.
func TestSkippedArtifactsAreNotMounted(t *testing.T) {
	sb := optSandbox(art("a", "r.example/a:1", true), art("b", "r.example/b:1", false))
	sb.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{
		{Name: "a", Digest: "sha256:" + strings.Repeat("1", 64), MountPath: "/a", State: sandboxv1alpha1.SandboxArtifactSkipped, Reason: "VerificationFailed"},
		{Name: "b", Digest: "sha256:" + strings.Repeat("2", 64), MountPath: "/b"},
	}
	if got := resolvedArtifacts(sb); len(got) != 1 || got[0].spec.Name != "b" {
		t.Fatalf("resolved = %+v", got)
	}
	fs, mounts := artifactShares(sb)
	if len(fs) != 1 || strings.Contains(mounts, "/a") {
		t.Errorf("shares %+v mounts %q", fs, mounts)
	}
	inits, _ := artifactInits(sb, true)
	if len(inits) != 1 || inits[0].Name != artifactInitPrefix+"b" {
		t.Errorf("inits %+v", inits)
	}
}

// A required artifact's container is exactly v0.16's; an optional one's is
// bounded by its deadline, never fails the pod, and (cold) writes its token,
// which its share requires.
func TestArtifactInits_OptionalFlags(t *testing.T) {
	sb := optSandbox(art("req", "r.example/a:1", false), art("opt", "r.example/b:1", true))
	sb.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{
		{Name: "req", Digest: "sha256:" + strings.Repeat("1", 64), MountPath: "/req"},
		{Name: "opt", Digest: "sha256:" + strings.Repeat("2", 64), MountPath: "/opt"},
	}
	inits, vols := artifactInits(sb, true)
	req, opt := strings.Join(inits[0].Args, " "), strings.Join(inits[1].Args, " ")
	for _, f := range []string{"--optional", "--deadline", "--ok-file"} {
		if strings.Contains(req, f) {
			t.Errorf("required container has %s: %s", f, req)
		}
		if !strings.Contains(opt, f) {
			t.Errorf("optional container lacks %s: %s", f, opt)
		}
	}
	// A cold launcher's init container counts from when the pod is built,
	// never earlier than the artifact's own deadline.
	if d := deadlineArg(t, opt); d.Before(time.Now().Add(9*time.Second).Truncate(time.Second)) {
		t.Errorf("cold deadline %v counted from creation, not from the launch", d)
	}
	found := false
	for _, v := range vols {
		found = found || v.Name == artifactResultsVolume
	}
	if !found {
		t.Error("no results volume")
	}
	fs, _ := artifactShares(sb)
	if fs[0].OptionalToken != "" || fs[1].OptionalToken != artifactOKFile("opt") {
		t.Errorf("tokens %q %q", fs[0].OptionalToken, fs[1].OptionalToken)
	}
	// A fetch pod's optional container is bounded and never fails, but has no
	// token: a warm slot checks the node's markers.
	inits, _ = artifactInits(sb, false)
	if o := strings.Join(inits[1].Args, " "); !strings.Contains(o, "--optional") || strings.Contains(o, "--ok-file") ||
		!deadlineArg(t, o).Equal(artifactDeadline(sb, &sb.Spec.Artifacts[1]).Truncate(time.Second)) {
		t.Errorf("fetch container: %s", o)
	}
}

func TestActionAttempt(t *testing.T) {
	sb := optSandbox()
	for id, want := range map[string]int{"uid-s": 1, "uid-s-fetched": 2, "uid-s-2": 2, "uid-s-3": 3} {
		if n, ok := actionAttempt(id, sb); !ok || n != want {
			t.Errorf("%s = %d %v, want %d", id, n, ok, want)
		}
	}
	for _, id := range []string{"", "uid-x", "uid-s-1", "uid-s-02", "uid-s-x", "uid-s-2-3"} {
		if _, ok := actionAttempt(id, sb); ok {
			t.Errorf("%s accepted", id)
		}
	}
}

// optionalSetup is warmSetup with the artifact optional and a fetchTimeout.
func optionalSetup(t *testing.T, timeout time.Duration) (*corev1.Pod, *sandboxv1alpha1.SwiftSandbox, string) {
	_, slot, sb, _, digest := warmSetup(t)
	sb.CreationTimestamp = metav1.Now()
	sb.Spec.Artifacts[0].Optional = true
	sb.Spec.Artifacts[0].FetchTimeout = &metav1.Duration{Duration: timeout}
	claimedWith(sb, slot, digest, string(sb.UID), "failed", "ArtifactMissing: app")
	sb.Status.Artifacts[0].State = sandboxv1alpha1.SandboxArtifactDispatched
	return slot, sb, digest
}

func slotNow(t *testing.T, c client.Client, slot *corev1.Pod) *corev1.Pod {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(slot), &p); err != nil {
		t.Fatalf("slot: %v", err)
	}
	return &p
}

// fetchTimeout 0s: an optional artifact not on the node is skipped at once
// and the workload dispatched again without it. No fetch.
func TestOptional_NotOnNodeSkipsAtOnce(t *testing.T) {
	slot, sb, _ := optionalSetup(t, 0)
	r, c := sandboxReconciler(slot, sb, keySecret("app-key"))
	reconcileSB(t, r, "sb")
	p := slotNow(t, c, slot)
	if p.Annotations[annSandboxExecActionID] != actionIDFor(sb, 2) || len(actionArtifacts(t, p)) != 0 {
		t.Fatalf("not dispatched again without it: id %q args %s", p.Annotations[annSandboxExecActionID], p.Annotations[annSandboxExecActionArgs])
	}
	var fetch corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: artifactFetchPodName(sb)}, &fetch); err == nil {
		t.Error("a fetch pod was started for a prepared-only artifact")
	}
	got := getSandbox(t, c, "sb")
	if s := got.Status.Artifacts[0]; s.State != sandboxv1alpha1.SandboxArtifactSkipped || s.Reason != "NotOnNode" {
		t.Errorf("status %+v", s)
	}
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxRunning {
		t.Errorf("phase %s", got.Status.Phase)
	}
}

// Within its deadline an optional artifact is fetched (bounded, never failing
// the pod); a verification failure there skips it, never mounts it.
func TestOptional_FetchedVerificationFailureSkips(t *testing.T) {
	ctx := context.Background()
	slot, sb, _ := optionalSetup(t, time.Minute)
	r, c := sandboxReconciler(slot, sb, keySecret("app-key"))
	reconcileSB(t, r, "sb")
	var fetch corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: artifactFetchPodName(sb)}, &fetch); err != nil {
		t.Fatalf("no fetch pod: %v", err)
	}
	if a := strings.Join(fetch.Spec.Containers[0].Args, " "); !strings.Contains(a, "--optional") || !strings.Contains(a, "--deadline=") {
		t.Errorf("fetch args %s", a)
	}
	if fetch.Spec.ActiveDeadlineSeconds == nil {
		t.Error("fetch pod has no deadline")
	}
	if got := getSandbox(t, c, "sb"); got.Status.Artifacts[0].State != sandboxv1alpha1.SandboxArtifactFetching {
		t.Errorf("state %s", got.Status.Artifacts[0].State)
	}
	msg, _ := json.Marshal(materialize.Result{Skipped: &materialize.Skip{Reason: "VerificationFailed", Message: "no matching signatures"}})
	fetch.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "artifact-app",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: string(msg)}}}}}
	_ = c.Status().Update(ctx, &fetch)
	reconcileSB(t, r, "sb")
	p := slotNow(t, c, slot)
	if p.Annotations[annSandboxExecActionID] != actionIDFor(sb, 2) || len(actionArtifacts(t, p)) != 0 {
		t.Fatalf("not dispatched again without it: %q %s", p.Annotations[annSandboxExecActionID], p.Annotations[annSandboxExecActionArgs])
	}
	got := getSandbox(t, c, "sb")
	if s := got.Status.Artifacts[0]; s.State != sandboxv1alpha1.SandboxArtifactSkipped || s.Reason != "VerificationFailed" {
		t.Errorf("status %+v", s)
	}
}

// A fetch still running when the optional artifact's deadline passes is not
// waited for: dispatched again without it.
func TestOptional_DeadlinePassedWhileFetching(t *testing.T) {
	ctx := context.Background()
	slot, sb, _ := optionalSetup(t, time.Minute)
	r, c := sandboxReconciler(slot, sb, keySecret("app-key"))
	reconcileSB(t, r, "sb") // fetch starts
	got := getSandbox(t, c, "sb")
	got.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Minute)) // deadline passed
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "sb")
	p := slotNow(t, c, slot)
	if p.Annotations[annSandboxExecActionID] != actionIDFor(sb, 2) {
		t.Fatalf("still waiting: %q", p.Annotations[annSandboxExecActionID])
	}
	if s := getSandbox(t, c, "sb").Status.Artifacts[0]; s.State != sandboxv1alpha1.SandboxArtifactSkipped || s.Reason != "FetchTimeout" {
		t.Errorf("status %+v", s)
	}
}

// A fetched artifact is recorded as fetched, then mounted when the workload
// has run with it.
func TestOptional_FetchedThenMounted(t *testing.T) {
	ctx := context.Background()
	slot, sb, _ := optionalSetup(t, time.Minute)
	r, c := sandboxReconciler(slot, sb, keySecret("app-key"))
	reconcileSB(t, r, "sb")
	var fetch corev1.Pod
	_ = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: artifactFetchPodName(sb)}, &fetch)
	fetch.Status.Phase = corev1.PodSucceeded
	_ = c.Status().Update(ctx, &fetch)
	reconcileSB(t, r, "sb")
	p := slotNow(t, c, slot)
	if len(actionArtifacts(t, p)) != 1 {
		t.Fatalf("the fetched artifact was dropped: %s", p.Annotations[annSandboxExecActionArgs])
	}
	p.Annotations[annSandboxExecStatusID] = actionIDFor(sb, 2)
	p.Annotations[annSandboxExecStatus] = "complete"
	p.Annotations[annSandboxExecStatusDetail] = "0"
	_ = c.Update(ctx, p)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if s := got.Status.Artifacts[0]; got.Status.Phase != sandboxv1alpha1.SwiftSandboxCompleted ||
		s.State != sandboxv1alpha1.SandboxArtifactMounted || s.Reason != "Fetched" {
		t.Errorf("phase %s status %+v", got.Status.Phase, s)
	}
}

// A required artifact keeps v0.16's behaviour: fetched once; missing again,
// the sandbox fails ArtifactMissing (also under the new attempt ids).
func TestRequired_SecondMissFailsWithNumberedIDs(t *testing.T) {
	_, slot, sb, _, digest := warmSetup(t)
	claimedWith(sb, slot, digest, actionIDFor(sb, 2), "failed", "ArtifactMissing: app")
	r, c := sandboxReconciler(slot, sb)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || cond == nil || cond.Reason != "ArtifactMissing" {
		t.Errorf("got %s %+v", got.Status.Phase, cond)
	}
}

// Cold start: what the launcher shares is decided by each optional init
// container's own result.
func TestRecordColdArtifacts(t *testing.T) {
	sb := optSandbox(art("hit", "r.example/a:1", true), art("skip", "r.example/b:1", true))
	sb.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{
		{Name: "hit", Digest: "sha256:" + strings.Repeat("1", 64), MountPath: "/hit", State: sandboxv1alpha1.SandboxArtifactResolved},
		{Name: "skip", Digest: "sha256:" + strings.Repeat("2", 64), MountPath: "/skip", State: sandboxv1alpha1.SandboxArtifactResolved},
	}
	ok, _ := json.Marshal(materialize.Result{Digest: sb.Status.Artifacts[0].Digest, CacheHit: true})
	skip, _ := json.Marshal(materialize.Result{Skipped: &materialize.Skip{Reason: "FetchTimeout", Message: "deadline"}})
	pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: "artifact-hit", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: string(ok)}}},
		{Name: "artifact-skip", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: string(skip)}}},
	}}}
	r := &SwiftSandboxReconciler{}
	r.recordColdArtifacts(sb, pod)
	if s := sb.Status.Artifacts[0]; s.State != sandboxv1alpha1.SandboxArtifactMounted || s.Reason != "Cached" {
		t.Errorf("hit %+v", s)
	}
	if s := sb.Status.Artifacts[1]; s.State != sandboxv1alpha1.SandboxArtifactSkipped || s.Reason != "FetchTimeout" {
		t.Errorf("skip %+v", s)
	}
}

// No slot can mount at checkout: a sandbox whose artifacts are all optional
// still takes a warm slot, without them; a required one boots cold, as before.
func TestCheckout_OptionalOnlyTakesASlotWithoutWarmMounts(t *testing.T) {
	pool, slot, sb, _, _ := warmSetup(t)
	slot.Annotations[bridgeFeaturesAnnotation] = "files mounts"
	sb.Spec.Artifacts[0].Optional = true
	r, c := sandboxReconciler(pool, slot, sb, keySecret("app-key"))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.PodRef != slot.Name {
		t.Fatalf("did not take the slot: podRef %q (%s)", got.Status.PodRef, got.Status.Message)
	}
	if s := got.Status.Artifacts[0]; s.State != sandboxv1alpha1.SandboxArtifactSkipped || s.Reason != "KernelUnsupported" {
		t.Errorf("status %+v", s)
	}
	if arts := actionArtifacts(t, slotNow(t, c, slot)); len(arts) != 0 {
		t.Errorf("artifacts sent to a slot that cannot mount them: %+v", arts)
	}

	pool, slot, sb, _, _ = warmSetup(t)
	slot.Annotations[bridgeFeaturesAnnotation] = "files mounts"
	r, c = sandboxReconciler(pool, slot, sb, keySecret("app-key"))
	reconcileSB(t, r, "sb")
	if got := getSandbox(t, c, "sb"); got.Status.PodRef == slot.Name {
		t.Error("a required artifact was skipped to take a slot")
	}
}

// deadlineArg parses a container's --deadline.
func deadlineArg(t *testing.T, args string) time.Time {
	t.Helper()
	for _, a := range strings.Fields(args) {
		if v, ok := strings.CutPrefix(a, "--deadline="); ok {
			d, err := time.Parse(time.RFC3339, v)
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("no --deadline in %s", args)
	return time.Time{}
}

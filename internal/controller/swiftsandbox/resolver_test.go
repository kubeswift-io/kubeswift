package swiftsandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// stubResolver returns a resolver whose fetch is f, with short bounds.
func stubResolver(f func(ctx context.Context, kind resolveKind, opts materialize.Options) resolveResult) *registryResolver {
	r := newRegistryResolver()
	r.inlineWait = 50 * time.Millisecond
	r.timeout = 300 * time.Millisecond
	r.fetch = f
	return r
}

// hang blocks until the request's context ends, like a registry that never
// answers.
func hang(ctx context.Context, _ resolveKind, _ materialize.Options) resolveResult {
	<-ctx.Done()
	return resolveResult{err: ctx.Err()}
}

func sandboxWaiter(ch chan event.GenericEvent, name string) waiter {
	return waiter{ch: ch, obj: &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}}
}

func req(ref string, auth authn.Authenticator) resolveRequest {
	return newResolveRequest(resolveDigestKind, ref, materialize.ModeTree, "/cache", auth)
}

// A registry that never answers holds a caller for the inline wait only. The
// request ends at its timeout, and the waiter is enqueued with the error.
func TestResolver_HangingRegistryDoesNotHoldTheCaller(t *testing.T) {
	r := stubResolver(hang)
	ch := make(chan event.GenericEvent, 1)
	start := time.Now()
	if _, ok := r.get(req("reg.example/a:1", nil), sandboxWaiter(ch, "a")); ok {
		t.Fatal("a hanging request reported ready")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("caller held %v, want about the inline wait", d)
	}
	select {
	case ev := <-ch:
		if ev.Object.GetName() != "a" {
			t.Errorf("enqueued %s", ev.Object.GetName())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never enqueued after the request timed out")
	}
	res, ok := r.get(req("reg.example/a:1", nil), waiter{})
	if !ok || !errors.Is(res.err, context.DeadlineExceeded) {
		t.Errorf("result = %+v, %v; want the deadline error", res, ok)
	}
}

// A healthy registry answers within the inline wait: one pass, no event.
func TestResolver_FastAnswerIsInline(t *testing.T) {
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		return resolveResult{digest: "sha256:aa"}
	})
	res, ok := r.get(req("reg.example/a:1", nil), waiter{})
	if !ok || res.digest != "sha256:aa" {
		t.Fatalf("got %+v, %v", res, ok)
	}
}

// Concurrent askers for one reference and one set of credentials share a
// request; other credentials get their own, so one tenant's answer is never
// handed to another.
func TestResolver_SharesOnlyWithTheSameCredentials(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	r := stubResolver(func(_ context.Context, _ resolveKind, o materialize.Options) resolveResult {
		calls.Add(1)
		<-release
		cfg, _ := o.Auth.Authorization()
		return resolveResult{digest: "sha256:" + cfg.Username}
	})
	r.timeout = 5 * time.Second
	alice := authn.FromConfig(authn.AuthConfig{Username: "alice", Password: "a"})
	bob := authn.FromConfig(authn.AuthConfig{Username: "bob", Password: "b"})
	ch := make(chan event.GenericEvent, 4)
	r.get(req("reg.example/a:1", alice), sandboxWaiter(ch, "a1"))
	r.get(req("reg.example/a:1", alice), sandboxWaiter(ch, "a2"))
	r.get(req("reg.example/a:1", bob), sandboxWaiter(ch, "b1"))
	close(release)
	for i := 0; i < 3; i++ {
		<-ch
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("fetches = %d, want 2 (alice once, bob once)", n)
	}
	if res, _ := r.get(req("reg.example/a:1", bob), waiter{}); res.digest != "sha256:bob" {
		t.Errorf("bob got %q", res.digest)
	}
}

// A stalled registry delays only the references it serves: its request slots
// fill, and a request for another registry still completes at once.
func TestResolver_StalledRegistryIsIsolated(t *testing.T) {
	r := stubResolver(func(ctx context.Context, k resolveKind, o materialize.Options) resolveResult {
		if strings.HasPrefix(o.ImageRef, "stalled.example/") {
			return hang(ctx, k, o)
		}
		return resolveResult{digest: "sha256:ok"}
	})
	r.perHost = 2
	r.timeout = 500 * time.Millisecond
	for i := 0; i < 5; i++ {
		r.get(req(fmt.Sprintf("stalled.example/a:%d", i), nil), waiter{})
	}
	start := time.Now()
	res, ok := r.get(req("healthy.example/b:1", nil), waiter{})
	if !ok || res.digest != "sha256:ok" || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("healthy registry: %+v, %v after %v", res, ok, time.Since(start))
	}
	// The queued requests behind the full slots fail at their deadline,
	// naming the registry.
	time.Sleep(700 * time.Millisecond)
	res, _ = r.get(req("stalled.example/a:4", nil), waiter{})
	if res.err == nil {
		t.Fatal("a request queued behind a stalled registry succeeded")
	}
}

// An error is kept briefly, so a retry after the backoff asks again.
func TestResolver_RetryAfterAnErrorAsksAgain(t *testing.T) {
	var calls atomic.Int32
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		calls.Add(1)
		return resolveResult{err: errors.New("connection refused")}
	})
	now := time.Now()
	var mu sync.Mutex
	r.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r.get(req("reg.example/a:1", nil), waiter{})
	r.get(req("reg.example/a:1", nil), waiter{})
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 while the error is fresh", calls.Load())
	}
	mu.Lock()
	now = now.Add(registryBackoffBase)
	mu.Unlock()
	r.get(req("reg.example/a:1", nil), waiter{})
	if calls.Load() != 2 {
		t.Errorf("fetches = %d, want 2 after the backoff", calls.Load())
	}
}

func TestRegistryRefused(t *testing.T) {
	_, badName := name.ParseReference("UPPER/case:1")
	cases := []struct {
		err  error
		want bool
	}{
		{&transport.Error{StatusCode: http.StatusNotFound}, true},
		{&transport.Error{StatusCode: http.StatusUnauthorized}, true},
		{&transport.Error{StatusCode: http.StatusForbidden}, true},
		{fmt.Errorf("resolve: %w", &transport.Error{StatusCode: http.StatusTooManyRequests}), false},
		{&transport.Error{StatusCode: http.StatusServiceUnavailable}, false},
		{fmt.Errorf("parse: %w", badName), true},
		{refusedError{errors.New("pull secret missing")}, true},
		{context.DeadlineExceeded, false},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
	}
	for _, c := range cases {
		if got := registryRefused(c.err); got != c.want {
			t.Errorf("registryRefused(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestRegistryBackoff_DoublesAndCaps(t *testing.T) {
	var m sync.Map
	key := client.ObjectKey{Namespace: "default", Name: "x"}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := registryBackoff(&m, key); got != w {
			t.Errorf("failure %d: backoff %v, want %v", i+1, got, w)
		}
	}
	for i := 0; i < 100; i++ { // no overflow past the cap
		if got := registryBackoff(&m, key); got != registryBackoffMax {
			t.Fatalf("failure %d: backoff %v", 9+i, got)
		}
	}
}

// A sandbox whose registry never answers (a real listener that accepts and
// says nothing) leaves the reconcile within the inline wait, Pending and
// saying why, instead of holding the controller's only worker.
func TestReconcile_HangingRegistryReturnsPending(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	}()

	sb := plainSandbox(ln.Addr().String() + "/team/app:1")
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	r.resolver = newRegistryResolver()
	r.resolver.inlineWait = 100 * time.Millisecond
	r.resolver.timeout = 2 * time.Second
	start := time.Now()
	res := reconcileSB(t, r, "sb")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("reconcile held the worker %v", d)
	}
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPending || cond == nil || cond.Reason != "Resolving" || res.RequeueAfter == 0 {
		t.Fatalf("got %s %+v requeue %v", got.Status.Phase, cond, res.RequeueAfter)
	}
	assertNoLauncher(t, c, "sb")

	// The request times out: transient, so Pending with a retry, never Failed.
	time.Sleep(2500 * time.Millisecond)
	res = reconcileSB(t, r, "sb")
	got = getSandbox(t, c, "sb")
	cond = apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPending || cond == nil || cond.Reason != "RegistryUnavailable" ||
		!strings.Contains(cond.Message, "next attempt in") || res.RequeueAfter != registryBackoffBase {
		t.Fatalf("after timeout: %s %+v requeue %v", got.Status.Phase, cond, res.RequeueAfter)
	}
}

// fakeClock is a settable clock for the resolver.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

const digestRef = "reg.example/a@sha256:0123456789012345678901234567890123456789012345678901234567890123"

// waitFor polls cond for up to a second.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Within the authorization TTL a digest reference is answered without the
// registry: the warm checkout's prepared path.
func TestResolver_DigestHitWithinTTLAsksNoRegistry(t *testing.T) {
	var calls atomic.Int32
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		calls.Add(1)
		return resolveResult{digest: "sha256:aa"}
	})
	clk := &fakeClock{now: time.Now()}
	r.now = clk.Now
	r.get(req(digestRef, nil), waiter{})
	clk.Advance(time.Minute)
	if res, ok := r.get(req(digestRef, nil), waiter{}); !ok || res.digest != "sha256:aa" {
		t.Fatalf("got %+v, %v", res, ok)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("fetches = %d, want 1", n)
	}
}

// The defect this fixes: after the TTL a checkout waited for the registry.
// Past the refresh point a hit is still answered from the entry, at once, and
// the renewal runs in the background; the renewed entry then outlives the
// original TTL.
func TestResolver_RefreshAheadDoesNotBlockAndRenews(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		if calls.Add(1) > 1 {
			<-gate // the renewal: a slow registry
		}
		return resolveResult{digest: "sha256:aa"}
	})
	r.timeout = 5 * time.Second
	clk := &fakeClock{now: time.Now()}
	r.now = clk.Now
	r.get(req(digestRef, nil), waiter{})

	clk.Advance(3 * time.Minute) // past refreshAt (2m30s), before the TTL
	start := time.Now()
	res, ok := r.get(req(digestRef, nil), waiter{})
	if !ok || res.digest != "sha256:aa" {
		t.Fatalf("hit past the refresh point: %+v, %v", res, ok)
	}
	if d := time.Since(start); d > 30*time.Millisecond {
		t.Errorf("a hit past the refresh point waited %v for the registry", d)
	}
	waitFor(t, "the renewal to start", func() bool { return calls.Load() == 2 })
	// A second hit while the renewal runs starts no other.
	r.get(req(digestRef, nil), waiter{})
	close(gate)
	waitFor(t, "the renewal to land", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		e := r.entries[req(digestRef, nil).key]
		return e != nil && !e.refreshing && clk.Now().Sub(e.finished) == 0
	})
	clk.Advance(3 * time.Minute) // 6m after the first answer, 3m after the renewal started
	if _, ok := r.get(req(digestRef, nil), waiter{}); !ok {
		t.Fatal("renewed entry not used")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("fetches = %d, want 2 (first answer, one renewal)", n)
	}
}

// A renewal that fails for a transient reason does not extend the entry: it
// expires at the TTL of the last answer and the next lookup asks the registry.
// Stale authorization never becomes permanent.
func TestResolver_FailedRefreshNeverExtends(t *testing.T) {
	var calls atomic.Int32
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		if calls.Add(1) == 1 {
			return resolveResult{digest: "sha256:aa"}
		}
		return resolveResult{err: errors.New("connection refused")}
	})
	clk := &fakeClock{now: time.Now()}
	r.now = clk.Now
	r.get(req(digestRef, nil), waiter{})
	clk.Advance(3 * time.Minute)
	if res, ok := r.get(req(digestRef, nil), waiter{}); !ok || res.err != nil {
		t.Fatalf("still valid before the TTL: %+v, %v", res, ok)
	}
	waitFor(t, "the renewal to fail", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		e := r.entries[req(digestRef, nil).key]
		return e != nil && !e.refreshing && !e.refreshFailed.IsZero()
	})
	clk.Advance(2*time.Minute + time.Second) // past the first answer's TTL
	res, _ := r.get(req(digestRef, nil), waiter{})
	if res.err == nil {
		t.Fatalf("an authorization past its TTL was used after a failed renewal: %+v", res)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("fetches = %d, want 3 (answer, failed renewal, new lookup)", n)
	}
}

// A renewal refused by the registry replaces the entry at once: revoked
// credentials reach new sandboxes at the next renewal, not at the TTL.
func TestResolver_RefusedRefreshRevokes(t *testing.T) {
	var calls atomic.Int32
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		if calls.Add(1) == 1 {
			return resolveResult{digest: "sha256:aa"}
		}
		return resolveResult{err: &transport.Error{StatusCode: http.StatusUnauthorized}}
	})
	clk := &fakeClock{now: time.Now()}
	r.now = clk.Now
	r.get(req(digestRef, nil), waiter{})
	clk.Advance(3 * time.Minute)
	r.get(req(digestRef, nil), waiter{})
	waitFor(t, "the refusal to replace the entry", func() bool {
		res, _ := r.get(req(digestRef, nil), waiter{})
		return res.err != nil && registryRefused(res.err)
	})
}

// A tag is never renewed in the background: it can move, so its answer is
// kept only resolveSuccessTTL.
func TestResolver_TagIsNotRefreshed(t *testing.T) {
	var calls atomic.Int32
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		calls.Add(1)
		return resolveResult{digest: "sha256:aa"}
	})
	clk := &fakeClock{now: time.Now()}
	r.now = clk.Now
	r.get(req("reg.example/a:1", nil), waiter{})
	clk.Advance(20 * time.Second)
	r.get(req("reg.example/a:1", nil), waiter{})
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("fetches = %d, want 1: a tag hit started a renewal", n)
	}
}

// With renewal disabled (refresh 0) behaviour is v0.16's: hits until the TTL,
// then a new request.
func TestResolver_RefreshDisabled(t *testing.T) {
	var calls atomic.Int32
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		calls.Add(1)
		return resolveResult{digest: "sha256:aa"}
	})
	r.refreshAt = 0
	clk := &fakeClock{now: time.Now()}
	r.now = clk.Now
	r.get(req(digestRef, nil), waiter{})
	clk.Advance(4 * time.Minute)
	r.get(req(digestRef, nil), waiter{})
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("fetches = %d, want 1", n)
	}
	clk.Advance(time.Minute)
	r.get(req(digestRef, nil), waiter{})
	if n := calls.Load(); n != 2 {
		t.Errorf("fetches = %d, want 2 after the TTL", n)
	}
}

func TestConfigureAuthorization_RejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		ttl     time.Duration
		refresh float64
	}{{-time.Second, 0.5}, {0, 0.5}, {10 * time.Second, 0.5}, {time.Minute, -0.1}, {time.Minute, 1}} {
		if err := ConfigureAuthorization(tc.ttl, tc.refresh); err == nil {
			t.Errorf("ttl %s refresh %v accepted", tc.ttl, tc.refresh)
		}
	}
	if err := ConfigureAuthorization(DefaultAuthorizationTTL, DefaultAuthorizationRefresh); err != nil {
		t.Fatal(err)
	}
}

// Several artifacts resolve together: every request starts before any is
// waited for, so a pass costs one wait, not one per artifact. Before, each
// lookup waited for its own request before the next one started.
func TestResolveArtifacts_RequestsRunTogether(t *testing.T) {
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		time.Sleep(120 * time.Millisecond)
		return resolveResult{digest: "sha256:aa"}
	})
	r.inlineWait = 250 * time.Millisecond
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}}
	for i := 0; i < 4; i++ {
		sb.Spec.Artifacts = append(sb.Spec.Artifacts, sandboxv1alpha1.SandboxArtifact{
			Name: fmt.Sprintf("a%d", i), Ref: fmt.Sprintf("reg%d.example/a@sha256:%064d", i, i), MountPath: fmt.Sprintf("/a%d", i)})
	}
	rec := &SwiftSandboxReconciler{resolver: r}
	start := time.Now()
	arts, ready, err := resolveArtifacts(context.Background(), nil, sb, rec.lookup(sb))
	took := time.Since(start)
	if err != nil || !ready || len(arts) != 4 {
		t.Fatalf("arts=%v ready=%v err=%v", arts, ready, err)
	}
	if took > 300*time.Millisecond {
		t.Errorf("4 artifacts took %v; they ran one after another", took)
	}
}

// A renewal answered after the old entry expired (a slow registry) is still a
// fresh answer: it is installed, so the next lookup does not miss.
func TestResolver_LateRefreshIsInstalled(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		if calls.Add(1) > 1 {
			<-gate
		}
		return resolveResult{digest: "sha256:aa"}
	})
	r.timeout = 5 * time.Second
	clk := &fakeClock{now: time.Now()}
	r.now = clk.Now
	r.get(req(digestRef, nil), waiter{})
	clk.Advance(3 * time.Minute)
	r.get(req(digestRef, nil), waiter{}) // starts the renewal
	waitFor(t, "the renewal to start", func() bool { return calls.Load() == 2 })
	clk.Advance(3 * time.Minute) // the old entry expires while it runs
	r.mu.Lock()
	r.pruneLocked()
	r.mu.Unlock()
	close(gate)
	waitFor(t, "the late renewal to be installed", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		e := r.entries[req(digestRef, nil).key]
		return e != nil && !e.refreshing && e.result.err == nil && e.finished.Equal(clk.Now().Add(-3*time.Minute))
	})
	r.get(req(digestRef, nil), waiter{})
	if n := calls.Load(); n != 2 {
		t.Errorf("fetches = %d, want 2", n)
	}
}

// A refusal on a later artifact outranks a transient error on an earlier one:
// the sandbox fails rather than waiting on a retry that cannot help.
func TestResolveArtifacts_RefusalOutranksTransient(t *testing.T) {
	r := stubResolver(func(_ context.Context, _ resolveKind, o materialize.Options) resolveResult {
		if strings.HasPrefix(o.ImageRef, "down.example/") {
			return resolveResult{err: errors.New("connection refused")}
		}
		return resolveResult{err: &transport.Error{StatusCode: http.StatusForbidden}}
	})
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}}
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{
		{Name: "a", Ref: "down.example/a:1", MountPath: "/a"},
		{Name: "b", Ref: "deny.example/b:1", MountPath: "/b"},
	}
	rec := &SwiftSandboxReconciler{resolver: r}
	_, _, err := resolveArtifacts(context.Background(), nil, sb, rec.lookup(sb))
	if err == nil || !registryRefused(err) || !strings.Contains(err.Error(), "artifact b") {
		t.Fatalf("err = %v, want artifact b's refusal", err)
	}
}

// A request primed and then waited for in the same pass is one lookup.
func TestLookup_PrimedRequestCountedOnce(t *testing.T) {
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		return resolveResult{digest: "sha256:aa"}
	})
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}}
	rec := &SwiftSandboxReconciler{resolver: r}
	r.get(req(digestRef, nil), waiter{}) // cached
	hits := testutil.ToFloat64(metrics.RegistryLookupsTotal.WithLabelValues("hit"))
	lookup := rec.lookup(sb)
	prime(lookup, req(digestRef, nil))
	lookup(req(digestRef, nil))
	if d := testutil.ToFloat64(metrics.RegistryLookupsTotal.WithLabelValues("hit")) - hits; d != 1 {
		t.Errorf("hits counted %v, want 1", d)
	}
}

// An answer's age counts from when its request started: a slow registry does
// not stretch the authorization window by the request's duration.
func TestResolver_AgeCountsFromRequestStart(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	r := stubResolver(func(context.Context, resolveKind, materialize.Options) resolveResult {
		clk.Advance(20 * time.Second) // the registry takes 20s
		return resolveResult{digest: "sha256:aa"}
	})
	r.timeout = 5 * time.Second
	r.now = clk.Now
	start := clk.Now()
	r.get(req(digestRef, nil), waiter{})
	waitFor(t, "the answer", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		e := r.entries[req(digestRef, nil).key]
		return e != nil && e.result.digest != ""
	})
	r.mu.Lock()
	got := r.entries[req(digestRef, nil).key].finished
	r.mu.Unlock()
	if !got.Equal(start) {
		t.Errorf("answer dated %v after the request started, want the start", got.Sub(start))
	}
}

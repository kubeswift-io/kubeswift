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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
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

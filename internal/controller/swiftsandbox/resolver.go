package swiftsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// The registry resolver runs the controllers' registry requests (a sandbox's
// image, model and artifacts; a pool's image and model) off the reconcile
// workers.
//
// Each controller has one worker. A resolve used to run inside it, with no
// overall deadline: a registry that accepted a connection and never answered
// held that worker forever, and every sandbox in the cluster waited behind it.
// A registry that never completed TLS held it about 35 s per reference.
//
// Now a reconcile asks the resolver and waits at most inlineWait. A healthy
// registry answers within that, so the reconcile completes in one pass as
// before. Otherwise the request keeps running in the background, bounded by
// requestTimeout, and the reconcile returns; when the request finishes, every
// object waiting for it is enqueued again through its controller's channel.
//
// Isolation: at most perRegistryLimit requests run against one registry host
// at once, so a stalled registry delays only the references it serves. Two
// objects asking for the same reference with the same credentials share one
// request. Credentials are part of the key, so a result obtained with one
// tenant's credentials is never handed to another's request.
const (
	resolveInlineWait     = 250 * time.Millisecond
	resolveRequestTimeout = 30 * time.Second
	resolvePerRegistry    = 4
	// A finished result is kept this long, for the objects it notified to
	// read and for others asking at the same moment. An error is kept for
	// less than the shortest retry backoff, so a retry makes a new request.
	resolveSuccessTTL = 30 * time.Second
	resolveErrorTTL   = 4 * time.Second
	// resolveDigestAuthTTL keeps a success for a digest reference longer: the
	// content cannot change, so what the cached answer stands for is that the
	// registry let these credentials read this digest. A warm-pool checkout
	// within it asks no registry. Revoking the credentials at the registry
	// takes up to this long to apply to new sandboxes. A tag is never kept
	// past resolveSuccessTTL: it can move.
	resolveDigestAuthTTL = 5 * time.Minute
)

// resolveKind is what a request fetches.
type resolveKind string

const (
	// resolveImage fetches the manifest and config: digest and image config.
	resolveImageKind resolveKind = "image"
	// resolveDigest fetches the manifest of a single image: its digest.
	resolveDigestKind resolveKind = "digest"
	// resolveDescriptor fetches the manifest as pushed (an index stays an
	// index): its digest. What spec.artifacts layout oci records.
	resolveDescriptorKind resolveKind = "descriptor"
)

type resolveKey struct {
	kind resolveKind
	ref  string
	auth string // a hash of the credentials, never the credentials
}

type resolveRequest struct {
	key  resolveKey
	opts materialize.Options
}

type resolveResult struct {
	repository string
	digest     string
	config     materialize.ImageConfig
	err        error
}

// waiter is an object to enqueue when a request it asked for finishes.
type waiter struct {
	ch  chan<- event.GenericEvent
	obj client.Object
}

// waiterKey tells a pool and a sandbox of the same name apart.
type waiterKey struct {
	ch  chan<- event.GenericEvent
	key client.ObjectKey
}

type resolveEntry struct {
	done     chan struct{}
	result   resolveResult
	finished time.Time
	waiters  map[waiterKey]waiter
}

type registryResolver struct {
	mu      sync.Mutex
	entries map[resolveKey]*resolveEntry
	hosts   map[string]chan struct{}

	inlineWait time.Duration
	timeout    time.Duration
	perHost    int
	now        func() time.Time
	// fetch performs one request; replaced in tests.
	fetch func(ctx context.Context, kind resolveKind, opts materialize.Options) resolveResult
}

func newRegistryResolver() *registryResolver {
	return &registryResolver{
		entries:    map[resolveKey]*resolveEntry{},
		hosts:      map[string]chan struct{}{},
		inlineWait: resolveInlineWait,
		timeout:    resolveRequestTimeout,
		perHost:    resolvePerRegistry,
		now:        time.Now,
		fetch:      fetchRegistry,
	}
}

// newResolveRequest builds the request for ref with auth (nil = anonymous).
func newResolveRequest(kind resolveKind, ref string, mode materialize.Mode, cacheDir string, auth authn.Authenticator) resolveRequest {
	return resolveRequest{
		key:  resolveKey{kind: kind, ref: ref, auth: authKey(auth)},
		opts: materialize.Options{ImageRef: ref, Mode: mode, CacheDir: cacheDir, Auth: auth},
	}
}

// authKey identifies credentials without holding them.
func authKey(auth authn.Authenticator) string {
	if auth == nil || auth == authn.Anonymous {
		return "anonymous"
	}
	cfg, err := auth.Authorization()
	if err != nil {
		return fmt.Sprintf("unreadable-%p", auth) // never shared
	}
	b, _ := json.Marshal(cfg)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// get returns the result of req when it is ready within the inline wait. Not
// ready, it returns ok=false, and w is enqueued when the request finishes.
func (r *registryResolver) get(req resolveRequest, w waiter) (resolveResult, bool) {
	r.mu.Lock()
	r.pruneLocked()
	e, ok := r.entries[req.key]
	if !ok {
		e = &resolveEntry{done: make(chan struct{}), waiters: map[waiterKey]waiter{}}
		r.entries[req.key] = e
		go r.run(req, e)
	}
	select {
	case <-e.done:
		r.mu.Unlock()
		return e.result, true
	default:
	}
	if w.obj != nil {
		e.waiters[waiterKey{w.ch, client.ObjectKeyFromObject(w.obj)}] = w
	}
	r.mu.Unlock()

	t := time.NewTimer(r.inlineWait)
	defer t.Stop()
	select {
	case <-e.done:
		return e.result, true
	case <-t.C:
		return resolveResult{}, false
	}
}

func (r *registryResolver) run(req resolveRequest, e *resolveEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	var res resolveResult
	if release, err := r.acquire(ctx, registryHost(req.opts.ImageRef)); err != nil {
		res.err = err
	} else {
		res = r.fetch(ctx, req.key.kind, req.opts)
		release()
	}

	r.mu.Lock()
	e.result = res
	e.finished = r.now()
	waiters := e.waiters
	e.waiters = nil
	close(e.done)
	r.mu.Unlock()
	for _, w := range waiters {
		select {
		case w.ch <- event.GenericEvent{Object: w.obj}:
		default:
			// Full: the waiter's periodic requeue picks the result up.
		}
	}
}

// acquire takes one of host's request slots, or fails when ctx ends first.
func (r *registryResolver) acquire(ctx context.Context, host string) (func(), error) {
	r.mu.Lock()
	sem, ok := r.hosts[host]
	if !ok {
		sem = make(chan struct{}, r.perHost)
		r.hosts[host] = sem
	}
	r.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("registry %s: %d requests already waiting on it: %w", host, r.perHost, ctx.Err())
	}
}

func (r *registryResolver) pruneLocked() {
	now := r.now()
	for k, e := range r.entries {
		select {
		case <-e.done:
		default:
			continue
		}
		ttl := resolveSuccessTTL
		switch {
		case e.result.err != nil:
			ttl = resolveErrorTTL
		case isDigestRef(k.ref):
			ttl = resolveDigestAuthTTL
		}
		if now.Sub(e.finished) >= ttl {
			delete(r.entries, k)
		}
	}
}

// isDigestRef reports whether ref pins a digest (repo@sha256:...).
func isDigestRef(ref string) bool {
	r, err := name.ParseReference(ref)
	if err != nil {
		return false
	}
	_, ok := r.(name.Digest)
	return ok
}

func registryHost(ref string) string {
	r, err := name.ParseReference(ref)
	if err != nil {
		return ""
	}
	return r.Context().RegistryStr()
}

// fetchRegistry performs one request against the registry, bounded by ctx.
func fetchRegistry(ctx context.Context, kind resolveKind, opts materialize.Options) resolveResult {
	opts.Context = ctx
	switch kind {
	case resolveDescriptorKind:
		repo, digest, err := materialize.ResolveDescriptor(opts)
		return resolveResult{repository: repo, digest: digest, err: err}
	case resolveImageKind:
		img, digest, err := materialize.RemotePull(opts)
		if err != nil {
			return resolveResult{err: err}
		}
		cfg, err := materialize.ConfigFromImage(img)
		if err != nil {
			return resolveResult{err: err}
		}
		return resolveResult{digest: digest, config: cfg}
	default:
		repo, digest, err := materialize.Resolve(opts)
		return resolveResult{repository: repo, digest: digest, err: err}
	}
}

// registryRefused reports whether err is the registry's answer about the
// reference itself (it is malformed, missing, or these credentials may not
// read it). Retrying will not change that until the spec or the Secret does,
// so the object fails. Anything else (unreachable, timed out, overloaded, a
// 5xx or 429) is transient: the object waits and retries with backoff.
func registryRefused(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		switch terr.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return true
		}
		return false
	}
	var perr *name.ErrBadName
	var rerr refusedError
	return errors.As(err, &perr) || errors.As(err, &rerr)
}

// resolvedEventBuffer bounds each controller's queue of finished requests; a
// full queue drops the event and the waiter's periodic requeue reads the result.
const resolvedEventBuffer = 1024

// sharedResolver serves both controllers, so the per-registry limit holds
// across them.
var sharedResolver = newRegistryResolver()

// Registry retry backoff for one object: 5 s doubling to 5 min.
const (
	registryBackoffBase = 5 * time.Second
	registryBackoffMax  = 5 * time.Minute
	// registryPendingRecheck is the fallback requeue while a request runs, in
	// case its completion event is dropped. Longer than a request can take.
	registryPendingRecheck = resolveRequestTimeout + 5*time.Second
)

func registryBackoff(failures *sync.Map, key client.ObjectKey) time.Duration {
	n := 1
	if v, ok := failures.Load(key); ok {
		n = v.(int) + 1
	}
	failures.Store(key, n)
	d := registryBackoffBase << (n - 1)
	if n > 16 || d > registryBackoffMax {
		d = registryBackoffMax
	}
	return d
}

// refusedError marks an error retrying will not fix (a missing or invalid pull
// Secret), as registryRefused does for the registry's own refusals.
type refusedError struct{ error }

func (e refusedError) Unwrap() error { return e.error }

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

	"github.com/kubeswift-io/kubeswift/internal/metrics"
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
	// Requests running or queued at once, in all and per namespace. Past
	// either, a new request is refused at once as transient (the object
	// backs off), so one namespace's references, or a flood of distinct
	// hosts, cannot grow the controller's goroutines and connections without
	// bound or take every slot from other namespaces.
	resolveGlobalLimit    = 256
	resolveNamespaceLimit = 32
	// A finished result is kept this long, for the objects it notified to
	// read and for others asking at the same moment. An error is kept for
	// less than the shortest retry backoff, so a retry makes a new request.
	resolveSuccessTTL = 30 * time.Second
	resolveErrorTTL   = 4 * time.Second
	// DefaultAuthorizationTTL keeps a success for a digest reference longer:
	// the content cannot change, so what the cached answer stands for is that
	// the registry let these credentials read this digest. A warm-pool
	// checkout within it asks no registry. Revoking the credentials at the
	// registry takes up to this long to apply to new sandboxes. A tag is never
	// kept past resolveSuccessTTL: it can move.
	DefaultAuthorizationTTL = 5 * time.Minute
	// DefaultAuthorizationRefresh is the fraction of the authorization TTL
	// after which a lookup that hits a digest reference's entry also asks the
	// registry again, in the background. The hit is still answered from the
	// entry, so a reference in steady use never waits for the registry; the
	// entry is replaced only by a new answer, never extended without one.
	DefaultAuthorizationRefresh = 0.5
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
	// namespace is whose request this is, for the per-namespace limit.
	namespace string
	// deadline is when an unfinished request stops being waited for; zero
	// waits inlineWait from now. The lookups of one reconcile pass share one
	// deadline, so several references cost one wait, not one each.
	deadline time.Time
	// noWait starts the request (or finds its result) without waiting, so
	// every request of a pass can be running before the first is waited for.
	noWait bool
	// followUp marks the wait for a request this pass already started, so the
	// lookup is counted once.
	followUp bool
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
	// refreshing: a background request is renewing this entry. refreshFailed
	// is when the last one failed transiently (the next waits resolveErrorTTL).
	// Guarded by registryResolver.mu; result and finished never change once
	// done is closed (a renewal installs a new entry).
	refreshing    bool
	refreshFailed time.Time
}

type registryResolver struct {
	mu      sync.Mutex
	entries map[resolveKey]*resolveEntry
	hosts   map[string]*hostSlots
	// running counts requests not yet finished, in all and per namespace.
	running   int
	runningNS map[string]int

	globalLimit int
	nsLimit     int

	inlineWait time.Duration
	timeout    time.Duration
	perHost    int
	// authTTL is how long a success for a digest reference stands for the
	// registry's authorization (0: as long as a tag's); refreshAt is the age at
	// which a hit renews it in the background (0: never).
	authTTL   time.Duration
	refreshAt time.Duration
	now       func() time.Time
	// fetch performs one request; replaced in tests.
	fetch func(ctx context.Context, kind resolveKind, opts materialize.Options) resolveResult
}

func newRegistryResolver() *registryResolver {
	return &registryResolver{
		entries:     map[resolveKey]*resolveEntry{},
		hosts:       map[string]*hostSlots{},
		runningNS:   map[string]int{},
		globalLimit: resolveGlobalLimit,
		nsLimit:     resolveNamespaceLimit,
		inlineWait:  resolveInlineWait,
		timeout:     resolveRequestTimeout,
		perHost:     resolvePerRegistry,
		authTTL:     DefaultAuthorizationTTL,
		refreshAt:   time.Duration(float64(DefaultAuthorizationTTL) * DefaultAuthorizationRefresh),
		now:         time.Now,
		fetch:       fetchRegistry,
	}
}

// ConfigureAuthorization sets how long a digest reference's resolve stands for
// the registry's authorization, and the fraction of it after which a hit renews
// it in the background (0 disables renewal). Call before the controllers start.
func ConfigureAuthorization(ttl time.Duration, refresh float64) error {
	// A result is kept at least resolveSuccessTTL in any case (the objects it
	// notified read it from the cache), so a shorter window cannot be had.
	if ttl < resolveSuccessTTL {
		return fmt.Errorf("authorization TTL %s is below the minimum %s", ttl, resolveSuccessTTL)
	}
	if refresh < 0 || refresh >= 1 {
		return fmt.Errorf("authorization refresh %v must be at least 0 and below 1", refresh)
	}
	sharedResolver.mu.Lock()
	defer sharedResolver.mu.Unlock()
	sharedResolver.authTTL = ttl
	sharedResolver.refreshAt = time.Duration(float64(ttl) * refresh)
	return nil
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
		if !r.admitLocked(req.namespace) {
			err := fmt.Errorf("too many registry requests in flight (namespace %q: %d, all: %d); retrying later",
				req.namespace, r.runningNS[req.namespace], r.running)
			r.mu.Unlock()
			return resolveResult{err: err}, true
		}
		metrics.RegistryLookupsTotal.WithLabelValues("miss").Inc()
		e = &resolveEntry{done: make(chan struct{}), waiters: map[waiterKey]waiter{}}
		r.entries[req.key] = e
		go r.run(req, e)
	}
	select {
	case <-e.done:
		if ok && !req.followUp {
			r.hitLocked(req, e)
		}
		r.mu.Unlock()
		return e.result, true
	default:
	}
	if w.obj != nil {
		e.waiters[waiterKey{w.ch, client.ObjectKeyFromObject(w.obj)}] = w
	}
	r.mu.Unlock()
	if req.noWait {
		return resolveResult{}, false
	}

	deadline := req.deadline
	if deadline.IsZero() {
		deadline = r.now().Add(r.inlineWait)
	}
	wait := deadline.Sub(r.now())
	if wait <= 0 {
		select {
		case <-e.done:
			return e.result, true
		default:
			return resolveResult{}, false
		}
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-e.done:
		return e.result, true
	case <-t.C:
		return resolveResult{}, false
	}
}

// prime starts every request without waiting for any; a later lookup of the
// same request waits for it. It lets one reconcile pass have all of its
// references in flight at once instead of one after another.
func prime(lookup lookupFunc, reqs ...resolveRequest) {
	for _, req := range reqs {
		req.noWait = true
		lookup(req)
	}
}

// admitLocked counts a new request against the limits, or reports that one
// would exceed them.
func (r *registryResolver) admitLocked(namespace string) bool {
	if r.running >= r.globalLimit || r.runningNS[namespace] >= r.nsLimit {
		return false
	}
	r.running++
	r.runningNS[namespace]++
	return true
}

// hitLocked records a lookup answered from a finished entry and, when the
// entry is a digest reference's authorization old enough to renew, renews it
// in the background. The caller still gets e's result: it is valid until
// authTTL from when the registry gave it.
func (r *registryResolver) hitLocked(req resolveRequest, e *resolveEntry) {
	if e.result.err != nil || r.refreshAt <= 0 || !isDigestRef(req.key.ref) {
		metrics.RegistryLookupsTotal.WithLabelValues("hit").Inc()
		return
	}
	now := r.now()
	if e.refreshing || now.Sub(e.finished) < r.refreshAt || now.Sub(e.refreshFailed) < resolveErrorTTL {
		metrics.RegistryLookupsTotal.WithLabelValues("hit").Inc()
		return
	}
	if !r.admitLocked(req.namespace) {
		// At the limit: renew on a later hit. The entry still expires on time.
		metrics.RegistryLookupsTotal.WithLabelValues("hit").Inc()
		return
	}
	metrics.RegistryLookupsTotal.WithLabelValues("refresh").Inc()
	e.refreshing = true
	go r.refresh(req, e)
}

// fetchOnce runs one request, bounded by the request timeout and the host's
// slots.
func (r *registryResolver) fetchOnce(req resolveRequest) resolveResult {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	release, err := r.acquire(ctx, registryHost(req.opts.ImageRef))
	if err != nil {
		return resolveResult{err: err}
	}
	defer release()
	return r.fetch(ctx, req.key.kind, req.opts)
}

// releaseLocked undoes admitLocked.
func (r *registryResolver) releaseLocked(namespace string) {
	r.running--
	if r.runningNS[namespace]--; r.runningNS[namespace] <= 0 {
		delete(r.runningNS, namespace)
	}
}

func (r *registryResolver) run(req resolveRequest, e *resolveEntry) {
	start := r.now()
	res := r.fetchOnce(req)
	observeRequest("lookup", res, r.now().Sub(start))

	r.mu.Lock()
	r.releaseLocked(req.namespace)
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

// refresh asks the registry again for a finished entry's request. A success
// or a refusal replaces the entry (a refusal is the revocation reaching new
// sandboxes); anything else leaves the old one to expire at its own time, so
// an authorization is never extended without the registry's answer.
func (r *registryResolver) refresh(req resolveRequest, old *resolveEntry) {
	start := r.now()
	res := r.fetchOnce(req)
	observeRequest("refresh", res, r.now().Sub(start))

	r.mu.Lock()
	defer r.mu.Unlock()
	r.releaseLocked(req.namespace)
	old.refreshing = false
	if res.err != nil && !registryRefused(res.err) {
		old.refreshFailed = r.now()
		return
	}
	// A fresh answer is installed even if the old entry expired meanwhile (a
	// slow registry); only a newer request's entry is left alone.
	if cur, ok := r.entries[req.key]; ok && cur != old {
		return
	}
	e := &resolveEntry{done: make(chan struct{}), result: res, finished: r.now()}
	close(e.done)
	r.entries[req.key] = e
}

// observeRequest records one finished registry request.
func observeRequest(reason string, res resolveResult, took time.Duration) {
	outcome := "success"
	if res.err != nil {
		outcome = "unavailable"
		if registryRefused(res.err) {
			outcome = "refused"
		}
	}
	metrics.RegistryRequestsTotal.WithLabelValues(reason, outcome).Inc()
	metrics.RegistryRequestSeconds.WithLabelValues(reason).Observe(took.Seconds())
}

// hostSlots bounds the requests against one registry host. users counts the
// requests holding or waiting for a slot, so an idle host is forgotten.
type hostSlots struct {
	sem   chan struct{}
	users int
}

// acquire takes one of host's request slots, or fails when ctx ends first.
func (r *registryResolver) acquire(ctx context.Context, host string) (func(), error) {
	r.mu.Lock()
	h, ok := r.hosts[host]
	if !ok {
		h = &hostSlots{sem: make(chan struct{}, r.perHost)}
		r.hosts[host] = h
	}
	h.users++
	r.mu.Unlock()
	done := func() {
		r.mu.Lock()
		if h.users--; h.users == 0 {
			delete(r.hosts, host)
		}
		r.mu.Unlock()
	}
	select {
	case h.sem <- struct{}{}:
		return func() { <-h.sem; done() }, nil
	case <-ctx.Done():
		done()
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
		case isDigestRef(k.ref) && r.authTTL > ttl:
			ttl = r.authTTL
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

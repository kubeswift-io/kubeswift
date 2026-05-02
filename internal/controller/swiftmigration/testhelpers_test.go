package swiftmigration

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Verb names used as the second key in selectiveFailingClient's
// per-(type, verb) counter and failure-injection maps.
//
// The split between VerbPatch and VerbStatusPatch is load-bearing for
// Phase 3a Group B's cutover ordering tests: cutover step 1 patches
// SwiftGuest.status.podRef.name (a status patch); step 3 patches
// SwiftMigration.status.phase (also a status patch on a different
// type); step 2 deletes a Pod (VerbDelete on *v1.Pod). Tests need to
// inject a failure into one of these without affecting the others —
// hence per-(type, verb) granularity.
//
// VerbStatusUpdate is included for completeness (the status subresource
// also supports Update) even though Phase 1 / 3a controllers patch
// rather than update; future code that switches verbs should not need
// to revisit this enum.
const (
	VerbGet          = "get"
	VerbList         = "list"
	VerbCreate       = "create"
	VerbUpdate       = "update"
	VerbDelete       = "delete"
	VerbPatch        = "patch"
	VerbStatusUpdate = "status-update"
	VerbStatusPatch  = "status-patch"
)

// failureSpec carries one queued failure: how many remaining invocations
// should still fail before it dequeues, and what error to return.
//
// remaining is decremented on each matched call; when it reaches zero
// the spec is removed from the queue and subsequent calls of the same
// (type, verb) succeed (subject to any later spec the test queued).
type failureSpec struct {
	remaining int
	err       error
}

// selectiveFailingClient wraps client.Client to provide per-(type, verb)
// invocation counters and per-(type, verb) failure injection.
//
// Why this exists: the existing patchCountingClient (controller_test.go)
// counts ALL patches (metadata + status) into one counter. Phase 3a
// Group B's cutover tests need to assert behavior like "fail the first
// SwiftGuest.status patch, then succeed on retry, while
// SwiftMigration.status patches always succeed." Coarse counting can't
// distinguish those flows.
//
// Type identity: keyed by reflect.TypeOf(obj).String(), which yields
// stable strings like "*v1alpha1.SwiftGuest" and "*v1.Pod". This avoids
// scheme injection and works for all concrete object types tests
// instantiate. Helper typeKeyOf(obj) wraps the reflect call.
//
// Concurrency: all maps are guarded by mu. Tests are typically
// single-goroutine, but controller-runtime can invoke from multiple
// goroutines if a test races a watch event with a reconcile; the lock
// is cheap insurance.
//
// Verbs supported: Get, List, Create, Update, Delete, Patch, Status
// Update, Status Patch. DeleteAllOf is not modeled (the swiftmigration
// controller does not call it; future cutover tests do not need it).
//
// API:
//   - FailNext(typeKey, verb, count, err) — queue N failures of this
//     (type, verb). Multiple FailNext calls accumulate; failures fire
//     in queue order until exhausted.
//   - Count(typeKey, verb) — total successful + failed invocations of
//     this (type, verb). Both successful and failed calls increment;
//     tests asserting "this verb was attempted N times" want this
//     semantic, and tests asserting "this verb succeeded N times" can
//     subtract from the failure-injection record they queued.
//   - Reset() — clear all counters and queued failures.
//
// Backward compatibility: patchCountingClient stays in
// controller_test.go for its existing terminal-phase short-circuit
// tests; selectiveFailingClient is the new tool for Group B's cutover
// tests. Tests choose whichever fits.
type selectiveFailingClient struct {
	client.Client

	mu       sync.Mutex
	counts   map[string]map[string]int           // typeKey -> verb -> count
	failures map[string]map[string][]failureSpec // typeKey -> verb -> queued specs
}

// newSelectiveFailingClient wraps an existing client.Client (typically a
// fake.NewClientBuilder().Build() result) and returns the wrapper plus
// a no-op test-helper failure for compatibility with tests that don't
// need failure injection (they can ignore FailNext and just use Count).
func newSelectiveFailingClient(inner client.Client) *selectiveFailingClient {
	return &selectiveFailingClient{
		Client:   inner,
		counts:   make(map[string]map[string]int),
		failures: make(map[string]map[string][]failureSpec),
	}
}

// typeKeyOf returns the canonical key for an object's type, e.g.
// "*v1alpha1.SwiftGuest". Used by FailNext / Count callers to identify
// types without importing a GVK helper.
func typeKeyOf(obj runtime.Object) string {
	return reflect.TypeOf(obj).String()
}

// FailNext queues count failures of the (typeKey, verb) pair. Each
// matching invocation pops one queued failure and returns its err
// instead of dispatching to the inner client. After count failures
// have fired, subsequent matching invocations succeed normally (or
// fail based on the next queued spec, if any).
//
// Calling FailNext multiple times for the same (typeKey, verb)
// accumulates: FailNext(k, v, 2, errA) followed by FailNext(k, v, 1,
// errB) produces 2 errA failures, then 1 errB failure, then success.
func (c *selectiveFailingClient) FailNext(typeKey, verb string, count int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures[typeKey] == nil {
		c.failures[typeKey] = make(map[string][]failureSpec)
	}
	c.failures[typeKey][verb] = append(c.failures[typeKey][verb], failureSpec{
		remaining: count,
		err:       err,
	})
}

// Count returns the total invocations (successful + failed) of the
// (typeKey, verb) pair.
func (c *selectiveFailingClient) Count(typeKey, verb string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[typeKey] == nil {
		return 0
	}
	return c.counts[typeKey][verb]
}

// Reset clears all counters and queued failures. Useful in test setup
// to reuse one wrapper across multiple table-driven sub-cases.
func (c *selectiveFailingClient) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = make(map[string]map[string]int)
	c.failures = make(map[string]map[string][]failureSpec)
}

// recordAndMaybeFail increments the counter for (typeKey, verb) and
// dequeues the head failureSpec if one is queued; returns the err to
// surface (nil = pass-through to inner client).
//
// Caller MUST NOT hold c.mu when invoking; this function takes the
// lock internally.
func (c *selectiveFailingClient) recordAndMaybeFail(typeKey, verb string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.counts[typeKey] == nil {
		c.counts[typeKey] = make(map[string]int)
	}
	c.counts[typeKey][verb]++

	specs := c.failures[typeKey][verb]
	if len(specs) == 0 {
		return nil
	}
	head := &specs[0]
	head.remaining--
	err := head.err
	if head.remaining <= 0 {
		c.failures[typeKey][verb] = specs[1:]
	}
	return err
}

// --- client.Client / Reader / Writer overrides ---

func (c *selectiveFailingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.recordAndMaybeFail(typeKeyOf(obj), VerbGet); err != nil {
		return err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *selectiveFailingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.recordAndMaybeFail(typeKeyOf(list), VerbList); err != nil {
		return err
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *selectiveFailingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := c.recordAndMaybeFail(typeKeyOf(obj), VerbCreate); err != nil {
		return err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *selectiveFailingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := c.recordAndMaybeFail(typeKeyOf(obj), VerbUpdate); err != nil {
		return err
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *selectiveFailingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.recordAndMaybeFail(typeKeyOf(obj), VerbDelete); err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *selectiveFailingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if err := c.recordAndMaybeFail(typeKeyOf(obj), VerbPatch); err != nil {
		return err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// Status returns a SubResourceWriter that routes Update/Patch to the
// VerbStatusUpdate / VerbStatusPatch counters (NOT VerbUpdate /
// VerbPatch). This split lets tests fail "the cutover-step-1 status
// patch" without affecting metadata-level patches on the same type
// (e.g., finalizer adds).
func (c *selectiveFailingClient) Status() client.SubResourceWriter {
	return &selectiveStatusWriter{
		inner:  c.Client.Status(),
		parent: c,
	}
}

type selectiveStatusWriter struct {
	inner  client.SubResourceWriter
	parent *selectiveFailingClient
}

func (s *selectiveStatusWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	// Status subresource Create is not modeled (controllers don't use
	// it). Pass through without counting; if a future test needs it,
	// add VerbStatusCreate.
	return s.inner.Create(ctx, obj, subResource, opts...)
}

func (s *selectiveStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if err := s.parent.recordAndMaybeFail(typeKeyOf(obj), VerbStatusUpdate); err != nil {
		return err
	}
	return s.inner.Update(ctx, obj, opts...)
}

func (s *selectiveStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if err := s.parent.recordAndMaybeFail(typeKeyOf(obj), VerbStatusPatch); err != nil {
		return err
	}
	return s.inner.Patch(ctx, obj, patch, opts...)
}

func (s *selectiveStatusWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	// Status subresource Apply is not modeled (controllers don't use
	// SSA on status). Pass through without counting.
	return s.inner.Apply(ctx, obj, opts...)
}

// String renders the wrapper's accumulated state for debug printing in
// failing tests; useful in t.Logf to diagnose unexpected counter
// values.
func (c *selectiveFailingClient) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf("selectiveFailingClient{counts=%v, failures-queued=%v}", c.counts, c.failures)
}

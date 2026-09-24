package gateway

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

func objAt(name, rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetName(name)
	u.SetResourceVersion(rv)
	return u
}

// scriptedStarts hands out pre-built fake watches in order and records the
// ListOptions each start was called with.
type scriptedStarts struct {
	mu      sync.Mutex
	watches []*watch.FakeWatcher
	opts    []metav1.ListOptions
}

func (s *scriptedStarts) start(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts = append(s.opts, opts)
	i := len(s.opts) - 1
	if i < len(s.watches) {
		return s.watches[i], nil
	}
	// Past the script: a watch that never delivers (held until ctx ends).
	return watch.NewFake(), nil
}

func (s *scriptedStarts) calls() []metav1.ListOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]metav1.ListOptions(nil), s.opts...)
}

func shortRetry(t *testing.T) {
	t.Helper()
	oi, om := watchRetryInitial, watchRetryMax
	watchRetryInitial, watchRetryMax = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { watchRetryInitial, watchRetryMax = oi, om })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// A routine close (the apiserver's watch timeout) must re-establish the watch
// from the last resourceVersion seen, without reporting an error — previously
// the member's stream just ended and the UI froze on it.
func TestResilientWatch_RoutineCloseResumesFromLastRV(t *testing.T) {
	shortRetry(t)
	first, second := watch.NewFake(), watch.NewFake()
	s := &scriptedStarts{watches: []*watch.FakeWatcher{first, second}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var names []string
	var errs []error
	done := make(chan struct{})
	go func() {
		defer close(done)
		resilientWatch(ctx, s.start,
			func(e watch.Event) bool {
				mu.Lock()
				names = append(names, e.Object.(*unstructured.Unstructured).GetName())
				mu.Unlock()
				return true
			},
			func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() })
	}()

	first.Add(objAt("a", "5"))
	first.Action(watch.Bookmark, objAt("", "9"))
	first.Stop() // server-side close
	waitFor(t, func() bool { return len(s.calls()) >= 2 })
	second.Modify(objAt("a", "10"))
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(names) == 2 })
	cancel()
	<-done

	if got := s.calls()[1].ResourceVersion; got != "9" {
		t.Errorf("resumed from resourceVersion %q, want the last bookmark 9", got)
	}
	if !s.calls()[0].AllowWatchBookmarks {
		t.Error("watch should request bookmarks to keep the resume point fresh")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 0 {
		t.Errorf("a routine close must not be reported as an error, got %v", errs)
	}
}

// An expired resourceVersion (410) restarts from scratch and IS reported, since
// deletions inside the gap cannot be replayed.
func TestResilientWatch_ExpiredRVRestartsAndReports(t *testing.T) {
	shortRetry(t)
	first := watch.NewFake()
	s := &scriptedStarts{watches: []*watch.FakeWatcher{first}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var errs []error
	done := make(chan struct{})
	go func() {
		defer close(done)
		resilientWatch(ctx, s.start, func(watch.Event) bool { return true },
			func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() })
	}()

	first.Add(objAt("a", "5"))
	gone := apierrors.NewResourceExpired("too old resource version")
	gone.ErrStatus.Code = http.StatusGone
	first.Error(&gone.ErrStatus)
	waitFor(t, func() bool { return len(s.calls()) >= 2 })
	cancel()
	<-done

	if got := s.calls()[1].ResourceVersion; got != "" {
		t.Errorf("after 410 the watch must restart from scratch, got resourceVersion %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 1 {
		t.Fatalf("expected the 410 to be reported once, got %v", errs)
	}
}

// A start failure that retrying cannot fix is reported once and ends the
// watch; a transient one is retried.
func TestResilientWatch_StartFailures(t *testing.T) {
	shortRetry(t)
	gr := schema.GroupResource{Group: "swift.kubeswift.io", Resource: "swiftguests"}

	var perm []error
	resilientWatch(context.Background(),
		func(context.Context, metav1.ListOptions) (watch.Interface, error) {
			return nil, apierrors.NewForbidden(gr, "", nil)
		},
		func(watch.Event) bool { return true },
		func(err error) { perm = append(perm, err) })
	if len(perm) != 1 {
		t.Errorf("forbidden should be reported once and stop, got %d reports", len(perm))
	}

	attempts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resilientWatch(ctx,
		func(context.Context, metav1.ListOptions) (watch.Interface, error) {
			attempts++
			if attempts >= 3 {
				cancel()
			}
			return nil, apierrors.NewServiceUnavailable("apiserver restarting")
		},
		func(watch.Event) bool { return true },
		func(error) {})
	if attempts < 3 {
		t.Errorf("a transient start failure should be retried, got %d attempts", attempts)
	}
}

package gateway

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// Retry pacing for a member watch that failed to (re)start. Package vars so
// tests can shorten them.
var (
	watchRetryInitial = time.Second
	watchRetryMax     = 30 * time.Second
)

// startWatchFunc opens one member watch with the given list options.
type startWatchFunc func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)

// resilientWatch keeps a member watch running for the life of ctx.
//
// A single apiserver watch does not last: the server ends it after its watch
// timeout (typically 30-60 minutes), a connection can drop, and a watch whose
// resourceVersion has been compacted away ends with a 410 Gone error event. A
// multi-cluster stream that just returned on any of those kept running on the
// other members' watches, so the UI showed the affected member frozen at its
// last state with no indication — a silent failure.
//
// Instead:
//   - a routine close resumes from the last resourceVersion seen (bookmarks
//     keep it fresh), so no event in the gap is lost and nothing is reported;
//   - an expired resourceVersion (410) restarts from scratch — the new watch
//     replays every current object as ADDED — and is reported through onErr,
//     because deletions inside the gap cannot be replayed;
//   - a failure to start is reported and retried with capped backoff, except an
//     error that cannot heal by retrying (forbidden, unauthorized, not found,
//     bad request), which is reported once and ends the watch as before.
//
// onEvent receives ADDED/MODIFIED/DELETED events and returns false to stop.
func resilientWatch(ctx context.Context, start startWatchFunc, onEvent func(watch.Event) bool, onErr func(error)) {
	rv := ""
	backoff := watchRetryInitial
	for ctx.Err() == nil {
		w, err := start(ctx, metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true})
		if err != nil {
			onErr(err)
			if permanentWatchError(err) {
				return
			}
			if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
				rv = ""
			}
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		gotEvent, stop := drainWatch(ctx, w, &rv, onEvent, onErr)
		w.Stop()
		if stop {
			return
		}
		if gotEvent {
			backoff = watchRetryInitial
			continue
		}
		// Closed without delivering anything: pace the reconnect so a watch
		// that ends immediately cannot spin.
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// drainWatch forwards events until the watch ends. It reports whether any
// event arrived and whether the caller should stop entirely.
func drainWatch(ctx context.Context, w watch.Interface, rv *string, onEvent func(watch.Event) bool, onErr func(error)) (gotEvent, stop bool) {
	for {
		select {
		case <-ctx.Done():
			return gotEvent, true
		case e, ok := <-w.ResultChan():
			if !ok {
				return gotEvent, false
			}
			gotEvent = true
			switch e.Type {
			case watch.Error:
				err := apierrors.FromObject(e.Object)
				if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
					*rv = ""
					onErr(fmt.Errorf("watch interrupted (resourceVersion expired); resynced from current state, so deletions during the gap may not be shown: %w", err))
				} else {
					onErr(err)
				}
				return gotEvent, false
			case watch.Bookmark:
				if v := objectResourceVersion(e); v != "" {
					*rv = v
				}
			default:
				if v := objectResourceVersion(e); v != "" {
					*rv = v
				}
				if !onEvent(e) {
					return gotEvent, true
				}
			}
		}
	}
}

func objectResourceVersion(e watch.Event) string {
	if e.Object == nil {
		return ""
	}
	a, err := meta.Accessor(e.Object)
	if err != nil {
		return ""
	}
	return a.GetResourceVersion()
}

// permanentWatchError is an error retrying cannot fix without an operator
// change (RBAC, a missing CRD, a malformed request).
func permanentWatchError(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) ||
		apierrors.IsNotFound(err) || apierrors.IsBadRequest(err) ||
		apierrors.IsMethodNotSupported(err)
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > watchRetryMax {
		d = watchRetryMax
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

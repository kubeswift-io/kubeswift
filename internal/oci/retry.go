package oci

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry/remote/errcode"
)

// Rate limits are the one registry failure that is transient by construction:
// the server is telling you to slow down, not that the request was wrong. A
// chunk push is also content-addressed and therefore idempotent, so retrying it
// cannot corrupt anything — at worst the blob is already there and the retry is
// a no-op.
//
// Without this, publishing a 30 GiB sparse golden image (~8.5 GiB real, ~135
// sequential blob PUTs at the 64 MiB default) reliably trips GitHub's secondary
// rate limit partway through and aborts the whole publish. The operator then
// restarts from zero — which is doubly wasteful, because the chunks already
// uploaded ARE reused on the next attempt.

const (
	// maxRetries bounds the wait. With baseDelay 2s and a 60s cap the worst
	// case is roughly five minutes of backoff, which matches what GitHub means
	// by "wait a few minutes" without hanging a CI job indefinitely.
	maxRetries = 8
	maxDelay   = 60 * time.Second
)

// baseDelay is a var, not a const, purely so tests can shrink the clock. A test
// that actually waited the real backoff would take minutes.
var baseDelay = 2 * time.Second

// RetryNotifyFunc is called before each backoff sleep. A multi-minute wait with
// no output is indistinguishable from a hang, so callers that have a terminal
// should print something.
type RetryNotifyFunc func(attempt int, delay time.Duration, err error)

// isRateLimited reports whether err is a registry rate limit worth retrying.
//
// It deliberately does NOT treat every 4xx as retryable. A plain 403 is
// permission denied and retrying it just wastes five minutes before failing
// with the same message — so 403 counts only when the body actually says rate
// limit, which is how GitHub reports its *secondary* limit. 429 is
// unambiguous and always counts.
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}

	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.StatusCode {
		case 429:
			return true
		case 403:
			// GitHub's secondary rate limit arrives as 403 with an explanatory
			// body. A 403 without that text is a real authorization failure.
			return mentionsRateLimit(err.Error())
		default:
			return false
		}
	}

	// Fallback for clients that do not surface a typed response. Matching text
	// is weaker than matching a status code, so it is the last resort rather
	// than the primary check.
	return mentionsRateLimit(err.Error())
}

func mentionsRateLimit(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "rate limit") ||
		strings.Contains(l, "too many requests")
}

// backoffFor returns the delay before attempt n (0-based), exponential with
// full jitter. Jitter matters here because a rate limit is usually hit by
// several concurrent publishes; retrying them all on the same schedule just
// reproduces the burst that caused it.
func backoffFor(attempt int, rnd *rand.Rand) time.Duration {
	d := baseDelay << attempt
	if d > maxDelay || d <= 0 { // <=0 guards the shift overflowing
		d = maxDelay
	}
	if rnd == nil {
		return d
	}
	// Full jitter: uniform in [d/2, d].
	half := d / 2
	return half + time.Duration(rnd.Int63n(int64(half)+1))
}

// withRateLimitRetry runs op, retrying while it fails with a rate limit.
// Non-rate-limit errors return immediately — a 404 or a bad digest will not fix
// itself, and retrying it hides the real failure behind minutes of waiting.
func withRateLimitRetry(ctx context.Context, notify RetryNotifyFunc, rnd *rand.Rand, op func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = op()
		if err == nil {
			return nil
		}
		if !isRateLimited(err) || attempt >= maxRetries {
			return err
		}
		delay := backoffFor(attempt, rnd)
		if notify != nil {
			notify(attempt+1, delay, err)
		}
		select {
		case <-ctx.Done():
			// Report the cause, not just "context cancelled" — otherwise a
			// publish killed during a backoff looks like an unrelated failure.
			return fmt.Errorf("%w (last registry error: %v)", ctx.Err(), err)
		case <-time.After(delay):
		}
	}
}

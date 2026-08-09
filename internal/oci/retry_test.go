package oci

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"testing"
	"time"

	"oras.land/oras-go/v2/registry/remote/errcode"
)

func rateLimit429() error {
	u, _ := url.Parse("https://ghcr.io/v2/acme/images/blobs/upload/abc")
	return &errcode.ErrorResponse{Method: "PUT", URL: u, StatusCode: 429}
}

// GitHub's *secondary* rate limit arrives as a 403 whose body explains itself.
func secondaryRateLimit403() error {
	u, _ := url.Parse("https://ghcr.io/v2/acme/images/blobs/upload/abc")
	return fmt.Errorf("PUT %q: %w", u.String(), &errcode.ErrorResponse{
		Method: "PUT", URL: u, StatusCode: 403,
		Errors: errcode.Errors{{
			Code:    "DENIED",
			Message: "You have exceeded a secondary rate limit. Please wait a few minutes before you try again.",
		}},
	})
}

func forbidden403() error {
	u, _ := url.Parse("https://ghcr.io/v2/acme/images/blobs/upload/abc")
	return &errcode.ErrorResponse{
		Method: "PUT", URL: u, StatusCode: 403,
		Errors: errcode.Errors{{Code: "DENIED", Message: "requested access to the resource is denied"}},
	}
}

func TestIsRateLimited(t *testing.T) {
	if !isRateLimited(rateLimit429()) {
		t.Error("429 must be retryable")
	}
	if !isRateLimited(secondaryRateLimit403()) {
		t.Error("403 whose body says secondary rate limit must be retryable — this is the #452 failure")
	}
	// The important negative: a plain 403 is permission denied. Retrying it
	// burns minutes of backoff and then fails with the same message.
	if isRateLimited(forbidden403()) {
		t.Error("a plain 403 (access denied) must NOT be retried")
	}
	if isRateLimited(nil) {
		t.Error("nil is not an error")
	}
	if isRateLimited(errors.New("connection reset by peer")) {
		t.Error("an unrelated error must not be treated as a rate limit")
	}
}

func TestWithRateLimitRetrySucceedsAfterTransientLimits(t *testing.T) {
	calls := 0
	rnd := rand.New(rand.NewSource(1))
	// Shrink the clock: the real backoff starts at 2s, and a test that waited
	// it out would take minutes for no extra confidence.
	orig := baseDelay
	baseDelay = time.Millisecond
	defer func() { baseDelay = orig }()

	err := withRateLimitRetry(context.Background(), nil, rnd, func() error {
		calls++
		if calls < 3 {
			return rateLimit429()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after transient limits, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

func TestWithRateLimitRetryDoesNotRetryPermanentErrors(t *testing.T) {
	calls := 0
	err := withRateLimitRetry(context.Background(), nil, nil, func() error {
		calls++
		return forbidden403()
	})
	if err == nil {
		t.Fatal("expected the 403 to propagate")
	}
	if calls != 1 {
		t.Fatalf("a permanent error must fail on the first attempt, got %d attempts", calls)
	}
}

func TestWithRateLimitRetryRespectsContextCancellation(t *testing.T) {
	orig := baseDelay
	baseDelay = 50 * time.Millisecond
	defer func() { baseDelay = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := withRateLimitRetry(ctx, nil, nil, func() error { return rateLimit429() })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the context error, got %v", err)
	}
	// The registry error must still be visible: otherwise a publish killed
	// during a backoff reports only "deadline exceeded" and the operator has no
	// idea a rate limit was the reason.
	if !strings.Contains(err.Error(), "429") && !mentionsRateLimit(err.Error()) {
		t.Errorf("cancellation error should carry the last registry error, got %q", err)
	}
}

func TestBackoffIsBoundedAndJittered(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	for attempt := 0; attempt < 20; attempt++ {
		d := backoffFor(attempt, rnd)
		if d <= 0 {
			t.Fatalf("attempt %d: non-positive delay %v (shift overflow?)", attempt, d)
		}
		if d > maxDelay {
			t.Fatalf("attempt %d: delay %v exceeds cap %v", attempt, d, maxDelay)
		}
	}
}

package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The failure this guards against (#460) is invisible by construction: the
// manager stays Ready and reconciles nothing. These pin the three outcomes.

func TestCacheSyncOutcome_SyncedIsFine(t *testing.T) {
	err := cacheSyncOutcome(context.Background(), time.Second, func(context.Context) bool { return true })
	if err != nil {
		t.Fatalf("err = %v, want nil when the cache syncs", err)
	}
}

func TestCacheSyncOutcome_NeverSyncsIsAnError(t *testing.T) {
	// Mirrors a forbidden LIST: the reflector retries until the deadline.
	err := cacheSyncOutcome(context.Background(), 20*time.Millisecond, func(ctx context.Context) bool {
		<-ctx.Done()
		return false
	})
	if err == nil {
		t.Fatal("err = nil; an unsynced cache must be fatal, not a silent idle process")
	}
	// The message is the whole point — it is the only thing the operator sees.
	for _, want := range []string{"is forbidden", "list+watch", "newer than its chart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q, got: %v", want, err)
		}
	}
}

// SIGTERM or a lost leader election during start-up cancels the parent context.
// That is a shutdown, and reporting it as a cache fault would turn every clean
// restart into a scary error.
func TestCacheSyncOutcome_ShutdownIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := cacheSyncOutcome(ctx, time.Minute, func(ctx context.Context) bool {
		<-ctx.Done()
		return false
	})
	if err != nil {
		t.Fatalf("err = %v, want nil: a cancelled parent context is a shutdown", err)
	}
}

package oci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignArgs(t *testing.T) {
	const (
		repo   = "ghcr.io/org/vm-snapshots"
		digest = "sha256:abc123"
		key    = "/oras-signing-key/cosign.key"
	)

	secure := SignArgs(repo, digest, key, false)
	joined := strings.Join(secure, " ")
	for _, want := range []string{
		"sign",
		"--key " + key,
		"--tlog-upload=false",
		"--yes",
		repo + "@" + digest,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("secure args missing %q; got: %s", want, joined)
		}
	}
	// Default tag-based attachment — NOT oci-1-1 referrer mode (cosign verify
	// can't discover a referrer-mode signature). Guards against regressing to it.
	if strings.Contains(joined, "registry-referrers-mode") {
		t.Errorf("must use cosign's default tag-based sig (no referrers mode); got: %s", joined)
	}
	if strings.Contains(joined, "--allow-http-registry") {
		t.Errorf("secure args must not carry --allow-http-registry; got: %s", joined)
	}
	// The digest reference must be last (cosign positional arg).
	if secure[len(secure)-1] != repo+"@"+digest {
		t.Errorf("digest ref must be the final arg; got %q", secure[len(secure)-1])
	}

	insecure := SignArgs(repo, digest, key, true)
	if !strings.Contains(strings.Join(insecure, " "), "--allow-http-registry") {
		t.Errorf("insecure args must carry --allow-http-registry; got: %v", insecure)
	}
	if insecure[len(insecure)-1] != repo+"@"+digest {
		t.Errorf("digest ref must remain the final arg even with --allow-http-registry; got %q", insecure[len(insecure)-1])
	}
}

func TestSign_MissingKeyFails(t *testing.T) {
	// A signing key that does not exist must fail before ever invoking cosign
	// (strict: no silent unsigned success).
	called := false
	orig := CosignRun
	CosignRun = func(_ context.Context, _ []string) error { called = true; return nil }
	defer func() { CosignRun = orig }()

	err := Sign(context.Background(), "ghcr.io/org/s", "sha256:d", "/nonexistent/cosign.key", false)
	if err == nil {
		t.Fatal("expected an error for a missing signing key")
	}
	if called {
		t.Error("cosign must not be invoked when the key is unreadable")
	}
}

func TestSign_InvokesCosignWithDigest(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "cosign.key")
	if err := os.WriteFile(key, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotArgs []string
	orig := CosignRun
	CosignRun = func(_ context.Context, args []string) error { gotArgs = args; return nil }
	defer func() { CosignRun = orig }()

	if err := Sign(context.Background(), "ghcr.io/org/s", "sha256:deadbeef", key, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "sign" {
		t.Fatalf("expected a cosign sign invocation; got %v", gotArgs)
	}
	if gotArgs[len(gotArgs)-1] != "ghcr.io/org/s@sha256:deadbeef" {
		t.Errorf("expected the digest ref as the final arg; got %q", gotArgs[len(gotArgs)-1])
	}
}

func TestVerifyArgs(t *testing.T) {
	args := VerifyArgs("ghcr.io/org/vm-images", "sha256:abc123", "/verify-key/cosign.pub")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"verify",
		"--key /verify-key/cosign.pub",
		"--insecure-ignore-tlog=true", // signed with --tlog-upload=false, so no Rekor entry to require
		"ghcr.io/org/vm-images@sha256:abc123",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("verify args missing %q; got: %s", want, joined)
		}
	}
	if args[0] != "verify" {
		t.Errorf("first arg must be verify; got %q", args[0])
	}
	if args[len(args)-1] != "ghcr.io/org/vm-images@sha256:abc123" {
		t.Errorf("digest ref must be the final arg; got %q", args[len(args)-1])
	}
	// cosign verify is HTTPS-only — no plaintext-registry flag exists on verify.
	if strings.Contains(joined, "allow-http") {
		t.Errorf("verify must not carry an http flag; got: %s", joined)
	}
}

func TestVerify_MissingKeyFails(t *testing.T) {
	called := false
	orig := CosignRun
	CosignRun = func(_ context.Context, _ []string) error { called = true; return nil }
	defer func() { CosignRun = orig }()

	if err := Verify(context.Background(), "ghcr.io/org/s", "sha256:d", "/nonexistent/cosign.pub"); err == nil {
		t.Fatal("expected an error for a missing verify key")
	}
	if called {
		t.Error("cosign must not be invoked when the key is unreadable")
	}
}

func TestVerify_InvokesCosignAndPropagatesFailure(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "cosign.pub")
	if err := os.WriteFile(key, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotArgs []string
	orig := CosignRun
	CosignRun = func(_ context.Context, args []string) error { gotArgs = args; return nil }
	defer func() { CosignRun = orig }()

	if err := Verify(context.Background(), "ghcr.io/org/s", "sha256:deadbeef", key); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "verify" {
		t.Fatalf("expected a cosign verify invocation; got %v", gotArgs)
	}
	if gotArgs[len(gotArgs)-1] != "ghcr.io/org/s@sha256:deadbeef" {
		t.Errorf("expected the digest ref as the final arg; got %q", gotArgs[len(gotArgs)-1])
	}

	// A verify failure (no/invalid signature) MUST propagate — fail loud, never
	// import an unverified disk.
	CosignRun = func(_ context.Context, _ []string) error { return fmt.Errorf("no matching signatures") }
	if err := Verify(context.Background(), "ghcr.io/org/s", "sha256:d", key); err == nil {
		t.Fatal("a cosign verify failure must propagate")
	}
}

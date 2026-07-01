package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCosignSignArgs(t *testing.T) {
	const (
		repo   = "ghcr.io/org/vm-snapshots"
		digest = "sha256:abc123"
		key    = "/oras-signing-key/cosign.key"
	)

	secure := cosignSignArgs(repo, digest, key, false)
	joined := strings.Join(secure, " ")
	for _, want := range []string{
		"sign",
		"--key " + key,
		"--registry-referrers-mode oci-1-1",
		"--tlog-upload=false",
		"--yes",
		repo + "@" + digest,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("secure args missing %q; got: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--allow-http-registry") {
		t.Errorf("secure args must not carry --allow-http-registry; got: %s", joined)
	}
	// The digest reference must be last (cosign positional arg).
	if secure[len(secure)-1] != repo+"@"+digest {
		t.Errorf("digest ref must be the final arg; got %q", secure[len(secure)-1])
	}

	insecure := cosignSignArgs(repo, digest, key, true)
	if !strings.Contains(strings.Join(insecure, " "), "--allow-http-registry") {
		t.Errorf("insecure args must carry --allow-http-registry; got: %v", insecure)
	}
	if insecure[len(insecure)-1] != repo+"@"+digest {
		t.Errorf("digest ref must remain the final arg even with --allow-http-registry; got %q", insecure[len(insecure)-1])
	}
}

func TestSignArtifact_MissingKeyFails(t *testing.T) {
	// A signing key that does not exist must fail before ever invoking cosign
	// (strict: no silent unsigned success).
	called := false
	orig := cosignRun
	cosignRun = func(_ context.Context, _ []string) error { called = true; return nil }
	defer func() { cosignRun = orig }()

	err := signArtifact(context.Background(), "ghcr.io/org/s", "sha256:d", "/nonexistent/cosign.key", false)
	if err == nil {
		t.Fatal("expected an error for a missing signing key")
	}
	if called {
		t.Error("cosign must not be invoked when the key is unreadable")
	}
}

func TestSignArtifact_InvokesCosignWithDigest(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "cosign.key")
	if err := os.WriteFile(key, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotArgs []string
	orig := cosignRun
	cosignRun = func(_ context.Context, args []string) error { gotArgs = args; return nil }
	defer func() { cosignRun = orig }()

	if err := signArtifact(context.Background(), "ghcr.io/org/s", "sha256:deadbeef", key, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "sign" {
		t.Fatalf("expected a cosign sign invocation; got %v", gotArgs)
	}
	if gotArgs[len(gotArgs)-1] != "ghcr.io/org/s@sha256:deadbeef" {
		t.Errorf("expected the digest ref as the final arg; got %q", gotArgs[len(gotArgs)-1])
	}
}

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

	secure := SignArgs(repo, digest, key, false, "")
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

	insecure := SignArgs(repo, digest, key, true, "")
	if !strings.Contains(strings.Join(insecure, " "), "--allow-http-registry") {
		t.Errorf("insecure args must carry --allow-http-registry; got: %v", insecure)
	}
	if insecure[len(insecure)-1] != repo+"@"+digest {
		t.Errorf("digest ref must remain the final arg even with --allow-http-registry; got %q", insecure[len(insecure)-1])
	}
}

// TestSignArgs_OfflineIsAlwaysExpressed is the leak guard. Every argv this
// builds must say "do not use a transparency log" in the dialect of whichever
// cosign will run it. An argv carrying NEITHER form still signs successfully on
// 3.x — and publishes the artifact digest to the public Rekor (#486). That is
// the failure this test exists to make impossible.
func TestSignArgs_OfflineIsAlwaysExpressed(t *testing.T) {
	const repo, digest, key = "ghcr.io/org/x", "sha256:abc", "/k/cosign.key"

	for _, tc := range []struct {
		name          string
		signingConfig string
		want, notWant string
	}{
		{"cosign 2.x: the flag", "", "--tlog-upload=false", "--signing-config"},
		{"cosign 3.x: a no-tlog signing-config", "/tmp/sc.json", "--signing-config /tmp/sc.json", "--tlog-upload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(SignArgs(repo, digest, key, false, tc.signingConfig), " ")
			if !strings.Contains(got, tc.want) {
				t.Errorf("missing %q; got: %s", tc.want, got)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("must not mix dialects — found %q; got: %s", tc.notWant, got)
			}
		})
	}
}

func TestCosignMajor(t *testing.T) {
	for _, tc := range []struct {
		out     string
		want    int
		wantErr bool
	}{
		{"GitVersion:    v2.6.5\n", 2, false},
		{"GitVersion:    v3.1.3\n", 3, false},
		{"GitVersion:    3.1.3\n", 3, false},
		{"some banner with no version", 0, true},
	} {
		got, err := cosignMajor(tc.out)
		if tc.wantErr {
			if err == nil {
				t.Errorf("cosignMajor(%q) should have failed", tc.out)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("cosignMajor(%q) = %d, %v; want %d", tc.out, got, err, tc.want)
		}
	}
}

// On cosign 3.x, Sign must pass a signing-config. If it ever calls cosign
// without one, the signature silently lands in the public transparency log.
func TestSign_Cosign3PassesSigningConfig(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "cosign.key")
	if err := os.WriteFile(keyPath, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	origRun, origVer := CosignRun, CosignVersionOutput
	defer func() { CosignRun, CosignVersionOutput = origRun, origVer }()
	CosignVersionOutput = func(context.Context) (string, error) { return "GitVersion:    v3.1.3\n", nil }

	var got []string
	CosignRun = func(_ context.Context, args []string) error { got = args; return nil }

	if err := Sign(context.Background(), "ghcr.io/org/x", "sha256:abc", keyPath, false); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--signing-config") {
		t.Errorf("cosign 3.x must be given --signing-config, else the digest goes to the public Rekor; got: %s", joined)
	}
	if strings.Contains(joined, "--tlog-upload") {
		t.Errorf("cosign 3.x rejects --tlog-upload at runtime; got: %s", joined)
	}

	// And the file handed over must actually declare no transparency log.
	idx := -1
	for i, a := range got {
		if a == "--signing-config" {
			idx = i + 1
		}
	}
	if idx < 0 || idx >= len(got) {
		t.Fatal("--signing-config had no path argument")
	}
	// Sign removes the temp file on return, so assert on the embedded source.
	if !strings.Contains(string(offlineSigningConfig), `"rekorTlogConfig": {}`) {
		t.Errorf("embedded signing-config must declare NO tlog services; got: %s", offlineSigningConfig)
	}
}

func TestSign_Cosign2UsesFlag(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "cosign.key")
	if err := os.WriteFile(keyPath, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	origRun, origVer := CosignRun, CosignVersionOutput
	defer func() { CosignRun, CosignVersionOutput = origRun, origVer }()
	CosignVersionOutput = func(context.Context) (string, error) { return "GitVersion:    v2.6.5\n", nil }

	var got []string
	CosignRun = func(_ context.Context, args []string) error { got = args; return nil }
	if err := Sign(context.Background(), "ghcr.io/org/x", "sha256:abc", keyPath, false); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !strings.Contains(strings.Join(got, " "), "--tlog-upload=false") {
		t.Errorf("cosign 2.x must keep --tlog-upload=false; got: %v", got)
	}
}

// An unreadable/unparseable cosign must fail the sign, not proceed blind.
func TestSign_UnknownCosignVersionFails(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "cosign.key")
	if err := os.WriteFile(keyPath, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	origRun, origVer := CosignRun, CosignVersionOutput
	defer func() { CosignRun, CosignVersionOutput = origRun, origVer }()
	CosignVersionOutput = func(context.Context) (string, error) { return "no version here", nil }

	called := false
	CosignRun = func(context.Context, []string) error { called = true; return nil }
	if err := Sign(context.Background(), "ghcr.io/org/x", "sha256:abc", keyPath, false); err == nil {
		t.Error("Sign must fail when the cosign version cannot be determined")
	}
	if called {
		t.Error("cosign must not be invoked when its version is unknown")
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
	orig, origVer := CosignRun, CosignVersionOutput
	CosignRun = func(_ context.Context, args []string) error { gotArgs = args; return nil }
	// Stub the version probe too: Sign now asks cosign which major it is, and a
	// unit test must not depend on a cosign existing on the machine running it.
	CosignVersionOutput = func(context.Context) (string, error) { return "GitVersion:    v2.6.5\n", nil }
	defer func() { CosignRun, CosignVersionOutput = orig, origVer }()

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

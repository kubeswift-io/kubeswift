package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// cosignRun executes the cosign CLI. A package var so tests can capture the args
// without a real cosign binary. COSIGN_PASSWORD is expected in the process env
// (the controller sets it from the signing-key Secret).
var cosignRun = func(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "cosign", args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cosignSignArgs builds the argv for signing the artifact digest offline (no
// Rekor). Uses cosign's DEFAULT tag-based attachment (a `sha256-<digest>.sig`
// tag), NOT `--registry-referrers-mode=oci-1-1`: cluster validation showed
// `cosign verify` has no referrer-discovery flag and cannot verify an
// oci-1-1-referrer signature, whereas the tag-based signature verifies with a
// plain `cosign verify --key`. The tag form is also the most registry-portable
// (GHCR/ECR/Harbor all support it). insecure adds --allow-http-registry for a
// plaintext registry (the sig still lands; cosign VERIFY over plaintext is
// unsupported — see the design note).
func cosignSignArgs(repository, digest, keyPath string, insecure bool) []string {
	args := []string{
		"sign",
		"--key", keyPath,
		"--tlog-upload=false",
		"--yes",
	}
	if insecure {
		args = append(args, "--allow-http-registry")
	}
	return append(args, repository+"@"+digest)
}

// signArtifact cosign-signs Repository@Digest. Strict: any error is returned so
// the Job fails (the snapshot then Fails — never an unsigned artifact left
// marked signed).
func signArtifact(ctx context.Context, repository, digest, keyPath string, insecure bool) error {
	if _, err := os.Stat(keyPath); err != nil {
		return fmt.Errorf("signing key %q not readable: %w", keyPath, err)
	}
	if err := cosignRun(ctx, cosignSignArgs(repository, digest, keyPath, insecure)); err != nil {
		return fmt.Errorf("cosign sign %s@%s: %w", repository, digest, err)
	}
	return nil
}

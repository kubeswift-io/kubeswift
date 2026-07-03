package oci

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// CosignRun executes the cosign CLI. A package var so tests can capture the args
// without a real cosign binary. COSIGN_PASSWORD is expected in the process env
// (the in-cluster Job sets it from the signing-key Secret; a client-side
// `swiftctl image publish` inherits it from the operator's shell).
var CosignRun = func(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "cosign", args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SignArgs builds the argv for signing the artifact digest offline (no Rekor).
// Uses cosign's DEFAULT tag-based attachment (a `sha256-<digest>.sig` tag), NOT
// `--registry-referrers-mode=oci-1-1`: cluster validation showed `cosign verify`
// has no referrer-discovery flag and cannot verify an oci-1-1-referrer
// signature, whereas the tag-based signature verifies with a plain
// `cosign verify --key`. The tag form is also the most registry-portable
// (GHCR/ECR/Harbor all support it). insecure adds --allow-http-registry for a
// plaintext registry (the sig still lands; cosign VERIFY over plaintext is
// unsupported — see docs/design/oras-provenance-signing.md).
func SignArgs(repository, digest, keyPath string, insecure bool) []string {
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

// Sign cosign-signs Repository@Digest. Strict: any error is returned so the
// caller fails loudly (the Job / publish command fails) — never an unsigned
// artifact left marked signed.
func Sign(ctx context.Context, repository, digest, keyPath string, insecure bool) error {
	if _, err := os.Stat(keyPath); err != nil {
		return fmt.Errorf("signing key %q not readable: %w", keyPath, err)
	}
	if err := CosignRun(ctx, SignArgs(repository, digest, keyPath, insecure)); err != nil {
		return fmt.Errorf("cosign sign %s@%s: %w", repository, digest, err)
	}
	return nil
}

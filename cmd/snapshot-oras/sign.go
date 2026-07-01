package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// cosignRun executes the cosign CLI. A package var so tests can capture the args
// without a real cosign binary. COSIGN_PASSWORD is expected in the process env
// (the controller sets it from the signing-key Secret); COSIGN_EXPERIMENTAL=1 is
// required for --registry-referrers-mode=oci-1-1 on cosign v2.x.
var cosignRun = func(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "cosign", args...)
	cmd.Env = append(os.Environ(), "COSIGN_EXPERIMENTAL=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cosignSignArgs builds the argv for signing the artifact digest as an OCI 1.1
// referrer, offline (no Rekor). insecure adds --allow-http-registry for a
// plaintext registry (the referrer still lands; cosign VERIFY over plaintext is
// unsupported — see the design note).
func cosignSignArgs(repository, digest, keyPath string, insecure bool) []string {
	args := []string{
		"sign",
		"--key", keyPath,
		"--registry-referrers-mode", "oci-1-1",
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

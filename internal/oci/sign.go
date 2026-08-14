package oci

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
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

// CosignVersionOutput returns `cosign version` output. A package var for the
// same reason as CosignRun.
var CosignVersionOutput = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "cosign", "version").CombinedOutput()
	return string(out), err
}

// offlineSigningConfig is a Sigstore signing-config declaring NO transparency-log
// and NO timestamp-authority services. cosign 3.x needs it to sign offline.
//
// Embedded rather than shipped as a file in the image, because both signers must
// have it: the in-cluster snapshot-oras Job AND `swiftctl image publish` running
// on an operator's own machine, where there is nothing to install.
//
//go:embed signing-config.json
var offlineSigningConfig []byte

var cosignVersionRE = regexp.MustCompile(`GitVersion:\s*v?(\d+)\.`)

// cosignMajor extracts the major version from `cosign version` output.
func cosignMajor(out string) (int, error) {
	m := cosignVersionRE.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("could not parse a version from cosign output")
	}
	return strconv.Atoi(m[1])
}

// SignArgs builds the argv for signing the artifact digest OFFLINE — no Rekor
// entry, so the artifact digest is never published to a transparency log.
//
// The two cosign majors express "offline" differently, and getting this wrong is
// not a build error but a silent information leak, so both forms are spelled out:
//
//   - 2.x takes `--tlog-upload=false`.
//   - 3.x REJECTS that flag at runtime and wants a signing-config with no
//     transparency-log service. signingConfigPath supplies one.
//
// Passing neither is what happens if you simply drop the flag on 3.x, and cosign
// then uploads to the PUBLIC Rekor while still exiting 0 — measured, see #486.
// Sign() therefore refuses to call this without one of the two.
//
// Uses cosign's DEFAULT tag-based attachment, NOT `--registry-referrers-mode=
// oci-1-1`: cluster validation showed `cosign verify` has no referrer-discovery
// flag and cannot verify an oci-1-1-referrer signature. The two majors write
// different tags (2.x `sha256-<digest>.sig`, 3.x `sha256-<digest>` holding a
// Sigstore bundle), but both verifiers read both forms — proven by
// hack/cosign-interop.sh, which is the gate on changing any of this.
//
// insecure adds --allow-http-registry for a plaintext registry (the sig still
// lands; cosign VERIFY over plaintext is unsupported).
func SignArgs(repository, digest, keyPath string, insecure bool, signingConfigPath string) []string {
	args := []string{
		"sign",
		"--key", keyPath,
		"--yes",
	}
	if signingConfigPath != "" {
		args = append(args, "--signing-config", signingConfigPath)
	} else {
		args = append(args, "--tlog-upload=false")
	}
	if insecure {
		args = append(args, "--allow-http-registry")
	}
	return append(args, repository+"@"+digest)
}

// Sign cosign-signs Repository@Digest offline. Strict: any error is returned so
// the caller fails loudly (the Job / publish command fails) — never an unsigned
// artifact left marked signed, and never a signature that quietly went to a
// public transparency log.
func Sign(ctx context.Context, repository, digest, keyPath string, insecure bool) error {
	if _, err := os.Stat(keyPath); err != nil {
		return fmt.Errorf("signing key %q not readable: %w", keyPath, err)
	}

	// Which cosign is on PATH decides how "offline" has to be spelled. This is
	// resolved at run time rather than pinned at build time because the signer
	// is not always ours: `swiftctl image publish` uses whatever the operator
	// has installed.
	verOut, err := CosignVersionOutput(ctx)
	if err != nil {
		return fmt.Errorf("cosign version: %w (is cosign installed?)", err)
	}
	major, err := cosignMajor(verOut)
	if err != nil {
		return fmt.Errorf("cosign version: %w; got: %q", err, verOut)
	}

	signingConfig := ""
	if major >= 3 {
		path, cleanup, err := writeOfflineSigningConfig()
		if err != nil {
			// Refuse rather than fall back to no flag. On 3.x that fallback
			// signs successfully AND publishes the digest to the public Rekor.
			return fmt.Errorf(
				"cosign %d.x needs an offline signing-config and one could not be written (%w); "+
					"refusing to sign, because signing without it would publish %s@%s to the public transparency log",
				major, err, repository, digest)
		}
		defer cleanup()
		signingConfig = path
	}

	if err := CosignRun(ctx, SignArgs(repository, digest, keyPath, insecure, signingConfig)); err != nil {
		return fmt.Errorf("cosign sign %s@%s: %w", repository, digest, err)
	}
	return nil
}

// writeOfflineSigningConfig materializes the embedded signing-config to a temp
// file, since cosign takes a path. The caller must invoke cleanup.
func writeOfflineSigningConfig() (string, func(), error) {
	f, err := os.CreateTemp("", "kubeswift-signing-config-*.json")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.Write(offlineSigningConfig); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

// VerifyArgs builds the argv for verifying a public-key signature against
// Repository@Digest. `--insecure-ignore-tlog=true` is required because Sign uses
// `--tlog-upload=false` (no Rekor entry), so verify must not demand a tlog entry.
// There is no plaintext-registry option: cosign verify speaks HTTPS only and does
// NOT honor `--allow-http-registry` on the registry ping — an insecure registry
// is rejected at admission before this runs.
func VerifyArgs(repository, digest, keyPath string) []string {
	return []string{
		"verify",
		"--key", keyPath,
		"--insecure-ignore-tlog=true",
		repository + "@" + digest,
	}
}

// Verify checks a cosign public-key signature on Repository@Digest. Strict: any
// error (unreadable key, missing/invalid signature) is returned so the caller
// fails loudly — a golden disk whose signature does not verify is never trusted.
func Verify(ctx context.Context, repository, digest, keyPath string) error {
	if _, err := os.Stat(keyPath); err != nil {
		return fmt.Errorf("verify key %q not readable: %w", keyPath, err)
	}
	if err := CosignRun(ctx, VerifyArgs(repository, digest, keyPath)); err != nil {
		return fmt.Errorf("cosign verify %s@%s: %w", repository, digest, err)
	}
	return nil
}

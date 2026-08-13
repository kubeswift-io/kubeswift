//go:build interop

// Signature-interoperability harness for #486.
//
// WHAT THIS GATES. `snapshot-oras` signs artifacts and `sandbox-materialize`
// verifies them, each with a vendored cosign binary. Changing how we sign — a
// cosign major bump, or replacing the binary with the sigstore Go library —
// risks a signature that the other side cannot read. That failure is silent in
// the worst way: a golden image that stops verifying at boot, or a
// SwiftSnapshot that verifies today and not after an upgrade. Signatures made
// by v2.6.5 already exist in users' registries and must keep verifying.
//
// So before any of that work lands, we need to be able to answer: does a
// signature produced by implementation A verify with implementation B? This
// runs that matrix against a real registry.
//
// WHY IT LIVES HERE, not in hack/ as a shell script: it must exercise the argv
// that `SignArgs`/`VerifyArgs` actually produce. A hand-written `cosign sign`
// in a shell script would test cosign, not us — and the flags are the whole
// point (`--tlog-upload=false` on sign, `--insecure-ignore-tlog` on verify).
// Being in-package also lets it swap implementations by overriding the
// `CosignRun` var, rather than manipulating PATH.
//
// NOT RUN BY DEFAULT. Build-tagged `interop`: it needs a live registry, a
// keypair, and cosign binaries on disk. Drive it with hack/cosign-interop.sh,
// which provisions all three.
package oci

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// implementation is one signer/verifier under test — today a cosign binary,
// later a library-backed in-process implementation. The matrix does not care
// which, as long as it can sign and verify given our argv.
type implementation struct {
	name string
	path string // cosign binary; empty means "in-process" (not yet used)
}

// runWith returns a CosignRun that shells out to a specific binary. Everything
// else — the arguments, the environment contract, the error handling — is the
// production path untouched.
func runWith(bin string) func(context.Context, []string) error {
	return func(ctx context.Context, args []string) error {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w\n%s", bin, strings.Join(args, " "), err, out)
		}
		return nil
	}
}

func envOrSkip(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set — run via hack/cosign-interop.sh", key)
	}
	return v
}

// TestSignatureInterop signs one artifact with every implementation and then
// verifies each signature with every implementation, printing the full matrix.
//
// A cell can fail for two very different reasons and the distinction matters:
// the signer could not produce a signature at all (its own diagonal cell fails
// too), or it produced one this verifier cannot read (only the off-diagonal
// fails). The diagonal is therefore the control: an implementation that cannot
// verify its OWN signature means the harness or the environment is broken, not
// the interop.
func TestSignatureInterop(t *testing.T) {
	registry := envOrSkip(t, "KUBESWIFT_INTEROP_REGISTRY") // host:port
	digest := envOrSkip(t, "KUBESWIFT_INTEROP_DIGEST")     // sha256:... of a pushed artifact
	keyPath := envOrSkip(t, "KUBESWIFT_INTEROP_KEY")       // private key, for signing
	pubPath := envOrSkip(t, "KUBESWIFT_INTEROP_PUBKEY")    // public key, for verifying
	binList := envOrSkip(t, "KUBESWIFT_INTEROP_COSIGNS")   // name=path,name=path

	var impls []implementation
	for _, spec := range strings.Split(binList, ",") {
		name, path, ok := strings.Cut(strings.TrimSpace(spec), "=")
		if !ok {
			t.Fatalf("KUBESWIFT_INTEROP_COSIGNS entry %q is not name=path", spec)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("implementation %q: %v", name, err)
		}
		impls = append(impls, implementation{name: name, path: path})
	}
	if len(impls) < 1 {
		t.Fatal("no implementations to test")
	}

	orig := CosignRun
	t.Cleanup(func() { CosignRun = orig })

	// Each signer gets its own repository so signatures cannot be confused with
	// one another — cosign stores them at a digest-derived tag, so two signers
	// signing the same digest in one repo would both write the same tag and the
	// second would merely append.
	signed := map[string]string{} // impl name -> repository it signed into
	for _, s := range impls {
		repo := fmt.Sprintf("%s/interop-%s/test", registry, strings.ToLower(s.name))
		t.Run("sign/"+s.name, func(t *testing.T) {
			CosignRun = runWith(s.path)
			if err := Sign(context.Background(), repo, digest, keyPath, false); err != nil {
				t.Errorf("%s could not sign: %v", s.name, err)
				return
			}
			signed[s.name] = repo
		})
	}

	type cell struct{ signer, verifier, result string }
	var results []cell

	for _, s := range impls {
		repo, ok := signed[s.name]
		if !ok {
			for _, v := range impls {
				results = append(results, cell{s.name, v.name, "SIGN-FAILED"})
			}
			continue
		}
		for _, v := range impls {
			name := fmt.Sprintf("verify/%s-signed/%s-verifies", s.name, v.name)
			res := "ok"
			t.Run(name, func(t *testing.T) {
				CosignRun = runWith(v.path)
				if err := Verify(context.Background(), repo, digest, pubPath); err != nil {
					res = "FAIL"
					// Every cell must pass for a migration to be safe: existing
					// signatures must verify with the new implementation, and a
					// fleet mid-upgrade has old verifiers reading new signatures.
					// So both cases fail — the message says which kind it is.
					if s.name == v.name {
						t.Errorf("%s cannot verify its OWN signature — harness or environment is broken, not interop: %v", s.name, err)
					} else {
						t.Errorf("INTEROP GAP: %s-signed does not verify with %s: %v", s.name, v.name, err)
					}
				}
			})
			results = append(results, cell{s.name, v.name, res})
		}
	}

	t.Log("signature interop matrix (rows = signer, columns = verifier):")
	for _, c := range results {
		t.Logf("  %-10s signed  ->  %-10s verifies  :  %s", c.signer, c.verifier, c.result)
	}

	// NEGATIVE CONTROL. Everything above is a green light, and a green light
	// that cannot turn red is not evidence. Verifying the same artifact with an
	// unrelated public key MUST fail; if it passes, this harness would have
	// certified an interop that was never actually checked — which is a worse
	// failure than any gap it might report.
	wrongPub := os.Getenv("KUBESWIFT_INTEROP_WRONG_PUBKEY")
	if wrongPub == "" {
		t.Error("negative control not configured (KUBESWIFT_INTEROP_WRONG_PUBKEY) — the matrix above is unproven")
		return
	}
	for _, v := range impls {
		repo, ok := signed[impls[0].name]
		if !ok {
			continue
		}
		t.Run("negative-control/"+v.name, func(t *testing.T) {
			CosignRun = runWith(v.path)
			if err := Verify(context.Background(), repo, digest, wrongPub); err == nil {
				t.Errorf("%s verified a signature against the WRONG public key — the harness is not checking anything", v.name)
			} else {
				t.Logf("  %-10s correctly rejects a foreign key", v.name)
			}
		})
	}
}

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
// It also answers a second question the exit code cannot: was the signature
// made OFFLINE? See assertNoTransparencyLogEntry — on cosign 3.x a signature
// can succeed while publishing the artifact digest to the public Rekor, which
// for a private snapshot is a disclosure. Every signer is checked for that.
//
// WHY IT LIVES HERE, not in hack/ as a shell script: it must exercise the argv
// that `SignArgs`/`VerifyArgs` actually produce. A hand-written `cosign sign`
// in a shell script would test cosign, not us — and the flags are the whole
// point (the no-transparency-log dialect on sign, which differs per major, and
// `--insecure-ignore-tlog` on verify). Being in-package also lets it swap
// implementations by overriding the `CosignRun`/`CosignVersionOutput` vars,
// rather than manipulating PATH.
//
// NOT RUN BY DEFAULT. Build-tagged `interop`: it needs a live registry, a
// keypair, and cosign binaries on disk. Drive it with hack/cosign-interop.sh,
// which provisions all three.
package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// versionOf returns a CosignVersionOutput bound to a specific binary, so Sign
// picks the offline dialect that binary actually accepts.
func versionOf(bin string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		out, err := exec.CommandContext(ctx, bin, "version").CombinedOutput()
		return string(out), err
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

// assertNoTransparencyLogEntry is the leak guard, and it is the reason this
// harness earns its keep beyond interop.
//
// "Signed offline" is not observable from cosign's exit code: on 3.x, an argv
// missing the offline dialect signs successfully AND publishes the artifact
// digest to the public Rekor. The only way to tell the two apart is to read the
// signature back off the registry and look for transparency-log entries. For a
// private VM snapshot or an air-gapped golden image, that upload is a
// disclosure — so a signature carrying one fails the test.
//
// Only 3.x's Sigstore-bundle form can carry them; 2.x's legacy `.sig` tag has
// nowhere to put one, so its absence there is structural rather than checked.
func assertNoTransparencyLogEntry(t *testing.T, registry, repo, digest, impl string) {
	t.Helper()
	repoPath := strings.TrimPrefix(repo, registry+"/")
	tag := "sha256-" + strings.TrimPrefix(digest, "sha256:")

	get := func(ref, accept string) []byte {
		req, err := http.NewRequest("GET", fmt.Sprintf("https://%s/v2/%s/%s", registry, repoPath, ref), nil)
		if err != nil {
			return nil
		}
		req.Header.Set("Accept", accept)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		b, _ := io.ReadAll(resp.Body)
		return b
	}

	idx := get("manifests/"+tag, "application/vnd.oci.image.index.v1+json")
	if idx == nil {
		t.Logf("  %s: no bundle-form signature at %s (legacy .sig form cannot carry a tlog entry)", impl, tag)
		return
	}
	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(idx, &index); err != nil || len(index.Manifests) == 0 {
		return
	}
	man := get("manifests/"+index.Manifests[0].Digest, "application/vnd.oci.image.manifest.v1+json")
	var manifest struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(man, &manifest); err != nil || len(manifest.Layers) == 0 {
		return
	}
	var bundle struct {
		VerificationMaterial struct {
			TlogEntries []struct {
				LogIndex string `json:"logIndex"`
			} `json:"tlogEntries"`
		} `json:"verificationMaterial"`
	}
	if err := json.Unmarshal(get("blobs/"+manifest.Layers[0].Digest, "*/*"), &bundle); err != nil {
		return
	}
	if n := len(bundle.VerificationMaterial.TlogEntries); n > 0 {
		t.Errorf("LEAK: %s published this artifact to a transparency log — %d entr(y|ies), first logIndex=%s. "+
			"Offline signing must produce none; a public Rekor entry discloses the digest of a private artifact.",
			impl, n, bundle.VerificationMaterial.TlogEntries[0].LogIndex)
		return
	}
	t.Logf("  %s: signed offline (tlogEntries=0)", impl)
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

	origRun, origVer := CosignRun, CosignVersionOutput
	t.Cleanup(func() { CosignRun, CosignVersionOutput = origRun, origVer })

	// Each signer gets its own repository so signatures cannot be confused with
	// one another — cosign stores them at a digest-derived tag, so two signers
	// signing the same digest in one repo would both write the same tag and the
	// second would merely append.
	signed := map[string]string{} // impl name -> repository it signed into
	for _, s := range impls {
		repo := fmt.Sprintf("%s/interop-%s/test", registry, strings.ToLower(s.name))
		t.Run("sign/"+s.name, func(t *testing.T) {
			CosignRun = runWith(s.path)
			CosignVersionOutput = versionOf(s.path)
			if err := Sign(context.Background(), repo, digest, keyPath, false); err != nil {
				t.Errorf("%s could not sign: %v", s.name, err)
				return
			}
			signed[s.name] = repo
			assertNoTransparencyLogEntry(t, registry, repo, digest, s.name)
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

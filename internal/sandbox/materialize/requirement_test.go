package materialize

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubeswift-io/kubeswift/internal/oci"
)

func pemOf(t *testing.T, pub any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func ecKey(t *testing.T, c elliptic.Curve) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pemOf(t, &k.PublicKey)
}

func TestParsePublicKey(t *testing.T) {
	p256 := ecKey(t, elliptic.P256())
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	rsa2048, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, tc := range []struct {
		name, key, alg string
	}{
		{"p256", p256, "ECDSA-P256"},
		{"p384", ecKey(t, elliptic.P384()), "ECDSA-P384"},
		{"ed25519", pemOf(t, edPub), "Ed25519"},
		{"rsa2048", pemOf(t, &rsa2048.PublicKey), "RSA-2048"},
		{"surrounding whitespace", "\n\n  " + p256 + "\n", "ECDSA-P256"},
	} {
		k, err := ParsePublicKey(tc.key)
		if err != nil || k.Algorithm != tc.alg || len(k.ID) != 64 {
			t.Errorf("%s: %+v, %v", tc.name, k, err)
		}
	}
	rsa1024, _ := rsa.GenerateKey(rand.Reader, 1024)
	der, _ := x509.MarshalPKCS8PrivateKey(rsa2048)
	private := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for _, tc := range []struct {
		name, key, want string
	}{
		{"private key", private, "private key"},
		{"two blocks", p256 + p256, "more than one"},
		{"text after", p256 + "trailing", "more than one"},
		{"text before", "kubeswift-artifact-requirement-v1\n" + p256, "not a PEM block"},
		{"rsa 1024", pemOf(t, &rsa1024.PublicKey), "at least 2048"},
		{"p224", ecKey(t, elliptic.P224()), "not accepted"},
		{"certificate", strings.Replace(p256, "PUBLIC KEY", "CERTIFICATE", 2), "want PUBLIC KEY"},
		{"garbage", "hello", "not a PEM block"},
	} {
		if _, err := ParsePublicKey(tc.key); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// The ID is the SubjectPublicKeyInfo's, not the text's.
	a, _ := ParsePublicKey(p256)
	b, _ := ParsePublicKey("  " + p256 + "\n\n")
	if a.ID != b.ID {
		t.Error("whitespace changed the key ID")
	}
}

const (
	dig  = "sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	dig2 = "sha256:" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	repo = "registry.example.com/caches/app"
)

// Every input changes the record's name; claim order does not; field
// boundaries cannot be shifted.
func TestRequirementFingerprint(t *testing.T) {
	claims := []RequiredClaim{{Key: "b", Values: []string{"2", "1"}}, {Key: "a", Values: []string{"x"}}}
	base := RequirementFingerprint("uid", repo, dig, "key", claims)
	same := RequirementFingerprint("uid", repo, dig, "key", []RequiredClaim{{Key: "a", Values: []string{"x"}}, {Key: "b", Values: []string{"1", "2", "1"}}})
	if base != same {
		t.Error("claim and value order changed the fingerprint")
	}
	for name, fp := range map[string]string{
		"uid":           RequirementFingerprint("uid2", repo, dig, "key", claims),
		"repository":    RequirementFingerprint("uid", repo+"2", dig, "key", claims),
		"digest":        RequirementFingerprint("uid", repo, dig2, "key", claims),
		"key":           RequirementFingerprint("uid", repo, dig, "key2", claims),
		"claim value":   RequirementFingerprint("uid", repo, dig, "key", []RequiredClaim{{Key: "a", Values: []string{"y"}}, {Key: "b", Values: []string{"1", "2"}}}),
		"one value":     RequirementFingerprint("uid", repo, dig, "key", []RequiredClaim{{Key: "a", Values: []string{"x"}}, {Key: "b", Values: []string{"1"}}}),
		"no claims":     RequirementFingerprint("uid", repo, dig, "key", nil),
		"shifted field": RequirementFingerprint("ui", "d"+repo, dig, "key", claims),
	} {
		if fp == base {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}
	if RequirementFingerprint("", "", "", "", []RequiredClaim{{Key: "ab", Values: []string{"c"}}}) ==
		RequirementFingerprint("", "", "", "", []RequiredClaim{{Key: "a", Values: []string{"bc"}}}) {
		t.Error("a claim's key and value boundary can be shifted")
	}
}

func payload(digest string, ann map[string]any) oci.SignedPayload {
	var p oci.SignedPayload
	p.Critical.Image.DockerManifestDigest = digest
	p.Optional = ann
	return p
}

func TestCheckRequirement(t *testing.T) {
	keyA, keyB := ecKey(t, elliptic.P256()), ecKey(t, elliptic.P256())
	pa, _ := ParsePublicKey(keyA)
	pb, _ := ParsePublicKey(keyB)
	req := Requirement{Policy: "p", UID: "uid", Keys: []RequirementKey{{Authority: "a", PublicKey: keyA}, {Authority: "b", PublicKey: keyB}},
		Claims: []RequiredClaim{{Key: "example.com/kind", Values: []string{"cache"}}, {Key: "example.com/rt", Values: []string{"1", "2"}}}}
	good := map[string]any{"example.com/kind": "cache", "example.com/rt": "2", "other": "x"}
	// signers maps a key's ID to the payloads cosign verified for it.
	verify := func(signers map[string][]oci.SignedPayload) VerifyPayloadsFunc {
		return func(pemKey []byte) ([]oci.SignedPayload, error) {
			k, err := ParsePublicKey(string(pemKey))
			if err != nil {
				return nil, err
			}
			if p, ok := signers[k.ID]; ok {
				return p, nil
			}
			return nil, errors.New("no matching signatures")
		}
	}
	fpA := RequirementFingerprint("uid", repo, dig, pa.ID, req.Claims)
	fpB := RequirementFingerprint("uid", repo, dig, pb.ID, req.Claims)

	if fp, err := CheckRequirement(req, repo, dig, verify(map[string][]oci.SignedPayload{pa.ID: {payload(dig, good)}})); err != nil || fp != fpA {
		t.Errorf("signed by A: %s, %v", fp, err)
	}
	// Rotation: only the new key signed; its record is the new key's.
	if fp, err := CheckRequirement(req, repo, dig, verify(map[string][]oci.SignedPayload{pb.ID: {payload(dig, good)}})); err != nil || fp != fpB {
		t.Errorf("signed by B: %s, %v", fp, err)
	}
	for _, tc := range []struct {
		name    string
		signers map[string][]oci.SignedPayload
		reason  string
	}{
		{"unsigned", nil, RequirementVerificationFailed},
		{"signed for another digest", map[string][]oci.SignedPayload{pa.ID: {payload(dig2, good)}}, RequirementVerificationFailed},
		{"claim missing", map[string][]oci.SignedPayload{pa.ID: {payload(dig, map[string]any{"example.com/kind": "cache"})}}, RequirementClaimMismatch},
		{"claim value not accepted", map[string][]oci.SignedPayload{pa.ID: {payload(dig, map[string]any{"example.com/kind": "cache", "example.com/rt": "3"})}}, RequirementClaimMismatch},
		{"claim not a string", map[string][]oci.SignedPayload{pa.ID: {payload(dig, map[string]any{"example.com/kind": "cache", "example.com/rt": 2})}}, RequirementClaimMismatch},
		// Each payload carries half the claims: never combined.
		{"claims split across signatures", map[string][]oci.SignedPayload{pa.ID: {
			payload(dig, map[string]any{"example.com/kind": "cache"}),
			payload(dig, map[string]any{"example.com/rt": "1"})}}, RequirementClaimMismatch},
	} {
		_, err := CheckRequirement(req, repo, dig, verify(tc.signers))
		if r, ok := RequirementReason(err); !ok || r != tc.reason {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.reason)
		}
	}
	// A key signing the right claims for the right digest, among others' bad
	// ones, satisfies the requirement.
	mixed := map[string][]oci.SignedPayload{pa.ID: {payload(dig, map[string]any{"example.com/kind": "x"}), payload(dig, good)}}
	if fp, err := CheckRequirement(req, repo, dig, verify(mixed)); err != nil || fp != fpA {
		t.Errorf("mixed payloads: %s, %v", fp, err)
	}
}

// Requirement records live apart from tenant-key records.
func TestRecordRequirement(t *testing.T) {
	dir := t.TempDir()
	fp := RequirementFingerprint("uid", repo, dig, "key", nil)
	if err := RecordRequirement(dir, dig, fp); err != nil {
		t.Fatal(err)
	}
	if err := RecordRequirement(dir, dig, fp); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	p := RequirementMarkerPath(dir, dig, fp)
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ".verified", strings.ReplaceAll(dig, ":", "-"), "r", fp); p != want {
		t.Errorf("path %s, want %s", p, want)
	}
	if VerifiedMarkerPath(dir, dig, fp) == p {
		t.Error("a requirement record and a tenant key record share a path")
	}
	if err := RecordRequirement(dir, "sha256:short", fp); err == nil {
		t.Error("a malformed digest was accepted")
	}
	if err := RecordRequirement(dir, dig, "../x"); err == nil {
		t.Error("a malformed fingerprint was accepted")
	}
}

func TestCanonicalRepository(t *testing.T) {
	for in, want := range map[string]string{
		"busybox":                                      "index.docker.io/library/busybox",
		"Registry.Example.com/caches/app:v1":           "registry.example.com/caches/app",
		"registry.example.com:5000/a/b@" + dig:         "registry.example.com:5000/a/b",
		"ghcr.io/kubeswift-io/kubeswift/kernels/x:1.0": "ghcr.io/kubeswift-io/kubeswift/kernels/x",
	} {
		if got, err := CanonicalRepository(in); err != nil || got != want {
			t.Errorf("%s: %s, %v; want %s", in, got, err, want)
		}
	}
}

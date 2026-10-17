package materialize

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// Platform verification requirements (SwiftArtifactVerificationPolicy).
//
// A Requirement is one governing policy as the controller resolved it for
// one artifact in one namespace: the keys that may sign it (any one) and the
// claims its signature must carry (all, in the same signed payload). The
// controller passes the requirements to the materializer in the immutable
// pod spec; the materializer verifies each and records, per requirement,
// which key satisfied it:
//
//	.verified/<digest>/r/<RequirementFingerprint>
//
// The record's name covers the policy's UID, the canonical repository, the
// digest, the key's identity and the resolved claims, so a record made under
// one policy, key, claim set, repository or namespace never satisfies
// another. Records live in their own r/ directory: a tenant's key Secret,
// whose record is named after the key file's bytes, can never produce one.
// Names are computed here from what was enforced, never taken from input.

// RequirementsEnv carries a materializer run's requirements, as JSON.
const RequirementsEnv = "KUBESWIFT_ARTIFACT_REQUIREMENTS"

const requirementDir = "r"

// requirementDomain separates requirement fingerprints from every other
// hash KubeSwift computes.
const requirementDomain = "kubeswift-artifact-requirement-v1\n"

// Requirement is one governing policy, resolved for one artifact.
type Requirement struct {
	// Policy is the policy's name (for messages only).
	Policy string `json:"policy"`
	// UID is the policy's UID: part of every record's identity.
	UID string `json:"uid"`
	// Generation is the policy generation resolved (for messages only).
	Generation int64 `json:"generation,omitempty"`
	// Keys may sign the artifact; the first whose signature satisfies the
	// claims is recorded.
	Keys []RequirementKey `json:"keys"`
	// Claims must all hold in one payload the key verified.
	Claims []RequiredClaim `json:"claims,omitempty"`
}

// RequirementKey is an authority's public key.
type RequirementKey struct {
	// Authority is the authority's name (for messages only).
	Authority string `json:"authority"`
	// PublicKey is the PEM "PUBLIC KEY" block.
	PublicKey string `json:"publicKey"`
}

// RequiredClaim is a signed annotation with its accepted values (any one).
type RequiredClaim struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// ParsedKey is a public key accepted as a trust root.
type ParsedKey struct {
	// ID is the sha256 of its SubjectPublicKeyInfo (DER), hex.
	ID string
	// Algorithm is ECDSA-P256, ECDSA-P384, Ed25519 or RSA-<bits>.
	Algorithm string
	// PEM is the single PEM block, re-encoded: exactly what cosign reads.
	PEM []byte
}

// ParsePublicKey accepts exactly one PEM "PUBLIC KEY" block, with nothing
// but whitespace around it, holding an ECDSA P-256 or P-384, Ed25519 or RSA
// (at least 2048 bits) key. A private key is refused outright.
func ParsePublicKey(text string) (ParsedKey, error) {
	if strings.Contains(text, "PRIVATE KEY") {
		return ParsedKey{}, errors.New("a private key is never accepted; give the public key")
	}
	trimmed := strings.TrimSpace(text)
	block, rest := pem.Decode([]byte(trimmed))
	if block == nil || !strings.HasPrefix(trimmed, "-----BEGIN ") {
		return ParsedKey{}, errors.New("not a PEM block")
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return ParsedKey{}, errors.New("more than one PEM block, or text after it")
	}
	if block.Type != "PUBLIC KEY" {
		return ParsedKey{}, fmt.Errorf("PEM block is %q, want PUBLIC KEY", block.Type)
	}
	if len(block.Headers) != 0 {
		return ParsedKey{}, errors.New("PEM headers are not accepted")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return ParsedKey{}, fmt.Errorf("parse public key: %w", err)
	}
	var alg string
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256():
			alg = "ECDSA-P256"
		case elliptic.P384():
			alg = "ECDSA-P384"
		default:
			return ParsedKey{}, fmt.Errorf("ECDSA curve %s is not accepted (P-256, P-384)", k.Curve.Params().Name)
		}
	case ed25519.PublicKey:
		alg = "Ed25519"
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return ParsedKey{}, fmt.Errorf("RSA key of %d bits: at least 2048", k.N.BitLen())
		}
		alg = fmt.Sprintf("RSA-%d", k.N.BitLen())
	default:
		return ParsedKey{}, fmt.Errorf("key type %T is not accepted", pub)
	}
	sum := sha256.Sum256(block.Bytes)
	return ParsedKey{ID: hex.EncodeToString(sum[:]), Algorithm: alg,
		PEM: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: block.Bytes})}, nil
}

// CanonicalRepository is the repository a reference names, as policies
// match it: registry host (lowercase) and path, Docker Hub as
// index.docker.io/library/....
func CanonicalRepository(ref string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	return strings.ToLower(r.Context().RegistryStr()) + "/" + r.Context().RepositoryStr(), nil
}

// CanonicalClaims sorts claims by key and each one's values, without
// duplicates: the form fingerprints are computed over.
func CanonicalClaims(claims []RequiredClaim) []RequiredClaim {
	out := make([]RequiredClaim, 0, len(claims))
	for _, c := range claims {
		v := slices.Clone(c.Values)
		slices.Sort(v)
		out = append(out, RequiredClaim{Key: c.Key, Values: slices.Compact(v)})
	}
	slices.SortFunc(out, func(a, b RequiredClaim) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// RequirementFingerprint names the record that digest, read from
// repository, satisfied the policy with UID policyUID through the key with
// ID keyID, under claims. Injective: every field is length-prefixed.
func RequirementFingerprint(policyUID, repository, digest, keyID string, claims []RequiredClaim) string {
	h := sha256.New()
	h.Write([]byte(requirementDomain))
	lp := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	lp(policyUID)
	lp(repository)
	lp(digest)
	lp(keyID)
	canon := CanonicalClaims(claims)
	lp(fmt.Sprint(len(canon)))
	for _, c := range canon {
		lp(c.Key)
		lp(fmt.Sprint(len(c.Values)))
		for _, v := range c.Values {
			lp(v)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AcceptableFingerprints are the records that satisfy req for digest from
// repository: one per key it trusts now.
func (req Requirement) AcceptableFingerprints(repository, digest string) ([]string, error) {
	var out []string
	for _, k := range req.Keys {
		pk, err := ParsePublicKey(k.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("policy %s authority %s: %w", req.Policy, k.Authority, err)
		}
		out = append(out, RequirementFingerprint(req.UID, repository, digest, pk.ID, req.Claims))
	}
	return out, nil
}

// RequirementMarkerPath is where a requirement record lives.
func RequirementMarkerPath(cacheDir, digest, fingerprint string) string {
	return filepath.Join(cacheDir, verifiedDir, strings.ReplaceAll(digest, ":", "-"), requirementDir, fingerprint)
}

// RecordRequirement records that digest satisfied the requirement whose
// fingerprint the caller computed from what it enforced. Idempotent; the
// record appears atomically.
func RecordRequirement(cacheDir, digest, fingerprint string) error {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return fmt.Errorf("record requirement: digest %q is not sha256", digest)
	}
	if len(fingerprint) != 64 {
		return fmt.Errorf("record requirement: fingerprint %q", fingerprint)
	}
	if err := writeMarker(RequirementMarkerPath(cacheDir, digest, fingerprint)); err != nil {
		return fmt.Errorf("record requirement: %w", err)
	}
	return nil
}

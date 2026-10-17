package materialize

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kubeswift-io/kubeswift/internal/oci"
)

// Why a requirement was not met. The materializer prints a failure as
// "<reason>: ..." and the controller reads the reason back from the
// termination message.
const (
	// RequirementVerificationFailed: no signature by any of the policy's
	// keys verified for the digest.
	RequirementVerificationFailed = "VerificationFailed"
	// RequirementClaimMismatch: a policy key signed the digest, but no
	// signed payload carries every required claim.
	RequirementClaimMismatch = "ClaimMismatch"
)

// RequirementError is a requirement not met.
type RequirementError struct {
	Reason string
	Policy string
	Msg    string
}

func (e *RequirementError) Error() string {
	return fmt.Sprintf("%s: policy %s: %s", e.Reason, e.Policy, e.Msg)
}

// RequirementReason returns the reason of a RequirementError in err.
func RequirementReason(err error) (string, bool) {
	var re *RequirementError
	if errors.As(err, &re) {
		return re.Reason, true
	}
	return "", false
}

// VerifyPayloadsFunc returns the payloads cosign verified for digest with
// the key in pem (and only those).
type VerifyPayloadsFunc func(pem []byte) ([]oci.SignedPayload, error)

// CheckRequirement verifies req for digest, read from repository. It tries
// the keys in order and returns the record fingerprint of the first whose
// verified payload for digest carries every claim. Claims are checked
// against one payload at a time, never combined across signatures.
func CheckRequirement(req Requirement, repository, digest string, verify VerifyPayloadsFunc) (string, error) {
	claims := CanonicalClaims(req.Claims)
	var signed bool
	var verifyErrs []string
	var mismatch string
	for _, k := range req.Keys {
		pk, err := ParsePublicKey(k.PublicKey)
		if err != nil {
			return "", &RequirementError{RequirementVerificationFailed, req.Policy, fmt.Sprintf("authority %s: %v", k.Authority, err)}
		}
		payloads, err := verify(pk.PEM)
		if err != nil {
			verifyErrs = append(verifyErrs, fmt.Sprintf("authority %s: %v", k.Authority, err))
			continue
		}
		keySigned := false
		for _, p := range payloads {
			if p.Critical.Image.DockerManifestDigest != digest {
				continue
			}
			keySigned, signed = true, true
			if why := missingClaim(p.Optional, claims); why != "" {
				if mismatch == "" {
					mismatch = fmt.Sprintf("signed by authority %s, but %s", k.Authority, why)
				}
				continue
			}
			return RequirementFingerprint(req.UID, repository, digest, pk.ID, claims), nil
		}
		if !keySigned {
			verifyErrs = append(verifyErrs, fmt.Sprintf("authority %s: no signature for %s", k.Authority, digest))
		}
	}
	if signed {
		return "", &RequirementError{RequirementClaimMismatch, req.Policy, mismatch}
	}
	return "", &RequirementError{RequirementVerificationFailed, req.Policy, strings.Join(verifyErrs, "; ")}
}

// missingClaim says which required claim the annotations do not satisfy,
// or "" when all hold.
func missingClaim(annotations map[string]any, claims []RequiredClaim) string {
	for _, c := range claims {
		v, ok := annotations[c.Key]
		if !ok {
			return fmt.Sprintf("claim %s is missing", c.Key)
		}
		s, ok := v.(string)
		if !ok || !slices.Contains(c.Values, s) {
			return fmt.Sprintf("claim %s is %q, want %s", c.Key, truncateClaim(fmt.Sprint(v)), strings.Join(c.Values, " or "))
		}
	}
	return ""
}

func truncateClaim(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

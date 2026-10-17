package v1alpha1

import (
	"fmt"
	"regexp"
	"sort"
)

// Signed artifact claims (SwiftArtifactVerificationPolicy claims, an
// artifact's claims) are compared as plain strings. Their characters are
// restricted so a claim can never be read as a separator anywhere it travels.
var (
	claimKeyRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	claimValueRE = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]+$`)
)

// Claim limits.
const (
	MaxClaimKeyLength   = 253
	MaxClaimValueLength = 256
	MaxArtifactClaims   = 8
)

// ValidClaimKey reports whether k may be a claim key.
func ValidClaimKey(k string) bool {
	return len(k) <= MaxClaimKeyLength && claimKeyRE.MatchString(k)
}

// ValidClaimValue reports whether v may be a claim value.
func ValidClaimValue(v string) bool {
	return len(v) <= MaxClaimValueLength && claimValueRE.MatchString(v)
}

// ValidateArtifactClaims checks an artifact's claims; at names the field.
func ValidateArtifactClaims(at string, claims map[string]string) error {
	if len(claims) > MaxArtifactClaims {
		return fmt.Errorf("%s has %d entries, at most %d", at, len(claims), MaxArtifactClaims)
	}
	keys := make([]string, 0, len(claims))
	for k := range claims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !ValidClaimKey(k) {
			return fmt.Errorf("%s key %q: letters, digits and . _ / -, at most %d", at, k, MaxClaimKeyLength)
		}
		if !ValidClaimValue(claims[k]) {
			return fmt.Errorf("%s[%s] %q: letters, digits and . _ : / @ + -, 1 to %d", at, k, claims[k], MaxClaimValueLength)
		}
	}
	return nil
}

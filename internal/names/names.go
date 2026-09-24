// Package names derives Kubernetes object names that must fit a length limit.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// MaxJobName is the longest usable Job name: the Job controller copies the
// name into its pods' job-name labels, and a label value is at most 63
// characters, so the apiserver rejects a longer Job name.
const MaxJobName = 63

// hashLen is the length of the "-<8 hex>" disambiguator Bounded appends.
const hashLen = 9

// Bounded returns base+suffix when it fits in max characters. Otherwise base is
// cut short and followed by a hash of the full name, then suffix, so the result
// fits, stays deterministic (the controller re-derives it on every pass), and
// two long names that share a prefix stay distinct. A name that already fits
// is returned unchanged, so objects created under it keep their names.
func Bounded(base, suffix string, max int) string {
	full := base + suffix
	if len(full) <= max {
		return full
	}
	hash := "-" + shortHash(full)
	if len(suffix)+hashLen+1 > max {
		// The suffix alone does not leave room: bound the whole name instead.
		base, suffix = full, ""
	}
	keep := max - len(suffix) - hashLen
	// A name segment must end alphanumeric before the hash's '-'.
	head := strings.TrimRight(base[:keep], "-.")
	if head == "" {
		return strings.TrimPrefix(hash, "-") + suffix
	}
	return head + hash + suffix
}

// MaxLabelValue is the longest label value.
const MaxLabelValue = 63

// LabelValue bounds an object name for use as a label value. Object names can
// run to 253 characters; a label carrying one verbatim made the object that
// bears it fail validation.
func LabelValue(name string) string {
	return Bounded(name, "", MaxLabelValue)
}

// JobName is Bounded to MaxJobName.
func JobName(base, suffix string) string {
	return Bounded(base, suffix, MaxJobName)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

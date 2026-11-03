package clonecommon

import (
	"fmt"
	"strconv"
	"strings"
)

// CloudHypervisorVersion is the Cloud Hypervisor the launcher image ships
// (images/swiftletd/Containerfile CH_VERSION; a test keeps them equal). A
// restore or clone boots a new launcher, so this is the version that restores.
const CloudHypervisorVersion = "v54.0"

// HypervisorVersionRefusal says why a memory snapshot captured by Cloud
// Hypervisor snapVersion cannot be restored by restoreVersion, or "" when it
// can (#871). Cloud Hypervisor restores snapshots from earlier releases (v54
// restores v53 memory snapshots), never from later ones. An unknown version on
// either side (a snapshot from a launcher that did not report it, an
// unparseable string) is not refused: the check has nothing to go on.
func HypervisorVersionRefusal(snapVersion, restoreVersion string) string {
	sMajor, sMinor, sok := majorMinor(snapVersion)
	rMajor, rMinor, rok := majorMinor(restoreVersion)
	if !sok || !rok {
		return ""
	}
	if sMajor > rMajor || (sMajor == rMajor && sMinor > rMinor) {
		return fmt.Sprintf("the snapshot was captured by Cloud Hypervisor %s and this cluster's launcher runs %s; "+
			"Cloud Hypervisor restores snapshots from its own and earlier releases, not later ones",
			snapVersion, restoreVersion)
	}
	return ""
}

// majorMinor parses "v<major>.<minor>" with or without the "v", ignoring a
// patch and anything after the minor's digits (a git-describe build reports
// "v54.0-3-gabc1234").
func majorMinor(s string) (int, int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minorDigits := parts[1]
	if i := strings.IndexFunc(minorDigits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		minorDigits = minorDigits[:i]
	}
	minor, err := strconv.Atoi(minorDigits)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

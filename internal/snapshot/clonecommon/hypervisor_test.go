package clonecommon

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestHypervisorVersionRefusal(t *testing.T) {
	for _, tc := range []struct {
		snap, restore string
		refused       bool
	}{
		{"v54.0", "v54.0", false},
		{"v53.0", "v54.0", false}, // newer restores older (validated v53 -> v54)
		{"v54.0.1", "v54.0", false},
		{"v54.1", "v54.0", true},
		{"v55.0", "v54.0", true},
		{"v55.0-3-gabc1234", "v54.0", true},
		{"54.0", "v53.0", true},
		{"", "v54.0", false}, // not recorded: nothing to check
		{"v54.0", "", false},
		{"garbage", "v54.0", false},
	} {
		msg := HypervisorVersionRefusal(tc.snap, tc.restore)
		if (msg != "") != tc.refused {
			t.Errorf("snap %q restore %q: refusal %q, want refused=%v", tc.snap, tc.restore, msg, tc.refused)
		}
		if tc.refused && (!strings.Contains(msg, tc.snap) || !strings.Contains(msg, tc.restore)) {
			t.Errorf("refusal must name both versions: %q", msg)
		}
	}
}

// CloudHypervisorVersion must be what the launcher image installs, or the
// check compares against a version no launcher runs.
func TestCloudHypervisorVersionMatchesTheLauncherImage(t *testing.T) {
	b, err := os.ReadFile("../../../images/swiftletd/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ARG CH_VERSION=(\S+)$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no ARG CH_VERSION in images/swiftletd/Containerfile")
	}
	if want := "v" + string(m[1]); CloudHypervisorVersion != want {
		t.Errorf("CloudHypervisorVersion = %q, launcher image ships %q", CloudHypervisorVersion, want)
	}
}

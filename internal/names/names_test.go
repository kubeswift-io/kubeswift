package names

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestJobName_ShortNamesAreUnchanged(t *testing.T) {
	if got := JobName("snap1", "-oci-push"); got != "snap1-oci-push" {
		t.Errorf("got %q; a name that fits must not change (existing Jobs keep it)", got)
	}
}

func TestJobName_LongNamesFitAndStayDistinct(t *testing.T) {
	long := strings.Repeat("a", 60)
	a := JobName(long+"-x", "-oci-disk-data")
	b := JobName(long+"-y", "-oci-disk-data")
	for _, n := range []string{a, b} {
		if len(n) > MaxJobName {
			t.Errorf("%q is %d chars, over %d", n, len(n), MaxJobName)
		}
		if errs := validation.IsDNS1123Subdomain(n); len(errs) != 0 {
			t.Errorf("%q is not a valid object name: %v", n, errs)
		}
		if errs := validation.IsValidLabelValue(n); len(errs) != 0 {
			t.Errorf("%q is not a valid label value: %v", n, errs)
		}
		if !strings.HasSuffix(n, "-oci-disk-data") {
			t.Errorf("%q lost its suffix", n)
		}
	}
	if a == b {
		t.Errorf("two long names collided: %q", a)
	}
	if JobName(long+"-x", "-oci-disk-data") != a {
		t.Error("not deterministic")
	}
}

// A cut that lands on a separator must not leave "-." or ".-" before the hash.
func TestJobName_CutNeverEndsOnASeparator(t *testing.T) {
	// The cut keeps 63 - len("-s3-upload") - 9 = 44 characters: "a"*43 + ".".
	base := strings.Repeat("a", 43) + ".-" + strings.Repeat("b", 30)
	n := JobName(base, "-s3-upload")
	if errs := validation.IsDNS1123Subdomain(n); len(errs) != 0 {
		t.Errorf("%q: %v", n, errs)
	}
	if errs := validation.IsValidLabelValue(n); len(errs) != 0 {
		t.Errorf("%q: %v", n, errs)
	}
}

func TestJobName_OversizedSuffixIsBoundedToo(t *testing.T) {
	n := JobName("g", "-datafill-"+strings.Repeat("d", 70))
	if len(n) > MaxJobName || len(validation.IsValidLabelValue(n)) != 0 {
		t.Errorf("%q (%d chars) is not a usable Job name", n, len(n))
	}
}

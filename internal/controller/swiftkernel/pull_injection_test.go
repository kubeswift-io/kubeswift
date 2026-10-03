package swiftkernel

import (
	"strings"
	"testing"
)

// Same defect class as the SwiftImage HTTP import: the OCI reference used to be
// interpolated with %q into an `oras pull` line inside `sh -c`, giving a tenant
// command execution in a root container that mounts /var/lib/kubeswift/kernels
// from the node.
func TestPullScript_DoesNotInterpolateImage(t *testing.T) {
	for _, auth := range []bool{false, true} {
		script := pullScript("/var/lib/kubeswift/kernels/ns-name", auth)
		for _, p := range []string{"$(", "`", "${IFS}"} {
			if strings.Contains(script, p) && !strings.Contains(script, `"$`+pullImageEnv+`"`) {
				t.Fatalf("auth=%v: script may interpolate user input: %s", auth, script)
			}
		}
		if !strings.Contains(script, `oras pull`) || !strings.Contains(script, ` "$`+pullImageEnv+`"`+"\n") {
			t.Fatalf("auth=%v: script does not dereference $%s: %s", auth, pullImageEnv, script)
		}
	}
}

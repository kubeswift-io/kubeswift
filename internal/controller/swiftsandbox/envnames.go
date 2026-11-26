package swiftsandbox

import (
	"fmt"
	"strings"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

const reasonInvalidEnvName = "InvalidEnvName"

// coldEnvViolation is the refusal for a variable a cold boot cannot set
// (sandboxwebhook.GuestEnvName): imageEnv is the resolved environment
// ("KEY=VAL", the image config with spec.env merged over it), and spec.env's
// Secret-backed variables are added by swiftletd. A pooled sandbox that falls
// back to a cold boot is checked too; a warm checkout is not, as the guest
// agent takes any name.
func coldEnvViolation(sb *sandboxv1alpha1.SwiftSandbox, imageEnv []string) (string, bool) {
	inSpec := map[string]bool{}
	for _, e := range sb.Spec.Env {
		inSpec[e.Name] = true
	}
	names := make([]string, 0, len(imageEnv))
	for _, kv := range imageEnv {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	for _, r := range secretEnvRefs(sb) {
		names = append(names, r.Name)
	}
	for _, n := range names {
		if sandboxwebhook.GuestEnvName(n) {
			continue
		}
		from := "spec.env"
		if !inSpec[n] {
			from = "the image config"
		}
		return fmt.Sprintf("variable %q (%s) cannot be set on a cold boot: the guest sets the environment with its shell, "+
			"which takes only letters, digits and _, not starting with a digit", n, from), true
	}
	return "", false
}

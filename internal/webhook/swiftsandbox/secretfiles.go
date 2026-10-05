package swiftsandbox

import (
	"fmt"
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// guestMountedDirs are mounted over the guest root after secret files are
// written, so a file there would be hidden.
var guestMountedDirs = []string{"/proc", "/sys", "/dev"}

// ValidateSecretFiles checks spec.secretFiles. The controller runs it too.
func ValidateSecretFiles(s *sandboxv1alpha1.SwiftSandboxSpec) error {
	if len(s.SecretFiles) > 16 {
		return fmt.Errorf("spec.secretFiles has %d entries, at most 16", len(s.SecretFiles))
	}
	var readOnly []string
	if s.Model != nil {
		readOnly = append(readOnly, s.Model.ModelMountPath())
	}
	seen := map[string]bool{}
	for i, f := range s.SecretFiles {
		at := fmt.Sprintf("spec.secretFiles[%d]", i)
		if errs := validation.IsDNS1123Subdomain(f.SecretName); len(errs) > 0 {
			return fmt.Errorf("%s.secretName %q: %s", at, f.SecretName, errs[0])
		}
		if len(f.Items) == 0 || len(f.Items) > 32 {
			return fmt.Errorf("%s.items: between 1 and 32 entries", at)
		}
		if err := validMode(f.Mode); err != nil {
			return fmt.Errorf("%s.mode: %w", at, err)
		}
		for j, it := range f.Items {
			iat := fmt.Sprintf("%s.items[%d]", at, j)
			if errs := validation.IsConfigMapKey(it.Key); len(errs) > 0 {
				return fmt.Errorf("%s.key %q: %s", iat, it.Key, errs[0])
			}
			if err := validMode(it.Mode); err != nil {
				return fmt.Errorf("%s.mode: %w", iat, err)
			}
			p := it.Path
			if !strings.HasPrefix(p, "/") || p == "/" || path.Clean(p) != p || strings.Contains(p, "/./") {
				return fmt.Errorf("%s.path %q: must be a clean absolute file path", iat, p)
			}
			for _, d := range append(append([]string(nil), guestMountedDirs...), readOnly...) {
				if p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
					return fmt.Errorf("%s.path %q: %s is mounted over the guest root, a file there would not be seen", iat, p, d)
				}
			}
			if seen[p] {
				return fmt.Errorf("%s.path %q is used twice", iat, p)
			}
			seen[p] = true
		}
	}
	return nil
}

func validMode(m *int32) error {
	if m != nil && (*m < 0 || *m > 0o777) {
		return fmt.Errorf("%#o is not a permission mode (0 to 0777)", *m)
	}
	return nil
}

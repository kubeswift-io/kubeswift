package swiftsandbox

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/exportpolicy"
)

// mediaTypeRE is an RFC 6838 type/subtype (what artifactType may hold).
var mediaTypeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,62}/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,62}$`)

// annotationKeyRE is the key grammar for output annotations.
var annotationKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// reservedAnnotationPrefixes are annotation keys KubeSwift or the OCI spec
// define; a workload cannot set them on its output.
var reservedAnnotationPrefixes = []string{"org.opencontainers.image.", "io.kubeswift."}

// ValidateOutputs checks spec.outputs. The controller runs it too. Whether a
// repository is allowed is a policy decision made when the sandbox is
// admitted, not a check here.
func ValidateOutputs(s *sandboxv1alpha1.SwiftSandboxSpec) error {
	if len(s.Outputs) > sandboxv1alpha1.MaxOutputs {
		return fmt.Errorf("spec.outputs has %d entries, at most %d", len(s.Outputs), sandboxv1alpha1.MaxOutputs)
	}
	taken := artifactMountPaths(s)
	if s.Model != nil {
		taken = append(taken, s.Model.ModelMountPath())
	}
	for _, f := range s.SecretFiles {
		for _, it := range f.Items {
			taken = append(taken, it.Path)
		}
	}
	names := map[string]bool{}
	for i, o := range s.Outputs {
		at := fmt.Sprintf("spec.outputs[%d]", i)
		if errs := validation.IsDNS1123Label(o.Name); len(errs) > 0 || len(o.Name) > 40 {
			return fmt.Errorf("%s.name %q: a DNS label of at most 40 characters", at, o.Name)
		}
		if names[o.Name] {
			return fmt.Errorf("%s.name %q is used twice", at, o.Name)
		}
		names[o.Name] = true

		p := o.Path
		if !mountPathRE.MatchString(p) || path.Clean(p) != p || p == "/" || strings.Contains(p, "/./") {
			return fmt.Errorf("%s.path %q: a clean absolute directory other than /, of letters, digits and . _ - /", at, p)
		}
		for _, d := range guestMountedDirs {
			if p == d || strings.HasPrefix(p, d+"/") {
				return fmt.Errorf("%s.path %q: %s is the guest's own", at, p, d)
			}
		}
		for _, t := range taken {
			if nested(p, t) {
				return fmt.Errorf("%s.path %q overlaps %s (an artifact, the model, a secret file or another output)", at, p, t)
			}
		}
		taken = append(taken, p)

		if o.Repository != "" {
			if err := exportpolicy.ValidateRepository(o.Repository); err != nil {
				return fmt.Errorf("%s.repository %q: %v", at, o.Repository, err)
			}
		}
		if q := o.MaxSize; q != nil && (q.Sign() <= 0 || q.Cmp(sandboxv1alpha1.MaxOutputSize) > 0) {
			return fmt.Errorf("%s.maxSize %s: more than 0, at most %s", at, q.String(), sandboxv1alpha1.MaxOutputSize.String())
		}
		if n := o.MaxEntries; n != nil && (*n < 1 || *n > sandboxv1alpha1.MaxOutputEntries) {
			return fmt.Errorf("%s.maxEntries %d: between 1 and %d", at, *n, sandboxv1alpha1.MaxOutputEntries)
		}
		if t := o.ArtifactType; t != "" && !mediaTypeRE.MatchString(t) {
			return fmt.Errorf("%s.artifactType %q: a media type (type/subtype)", at, t)
		}
		if len(o.Annotations) > 8 {
			return fmt.Errorf("%s.annotations has %d entries, at most 8", at, len(o.Annotations))
		}
		for k, v := range o.Annotations {
			if len(k) > 253 || !annotationKeyRE.MatchString(k) {
				return fmt.Errorf("%s.annotations key %q: letters, digits and . _ / -, at most 253", at, k)
			}
			for _, r := range reservedAnnotationPrefixes {
				if strings.HasPrefix(k, r) {
					return fmt.Errorf("%s.annotations key %q: %s* is reserved", at, k, r)
				}
			}
			if len(v) > 256 {
				return fmt.Errorf("%s.annotations[%q]: at most 256 bytes", at, k)
			}
			for _, r := range v {
				if r < 0x20 || r == 0x7f {
					return fmt.Errorf("%s.annotations[%q]: control characters are not allowed", at, k)
				}
			}
		}
	}
	return nil
}

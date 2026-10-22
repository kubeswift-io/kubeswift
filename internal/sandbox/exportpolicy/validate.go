// Package exportpolicy holds the rules of SwiftArtifactExportPolicy: which
// repositories a namespace's sandbox outputs may be pushed to.
package exportpolicy

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/artifactpolicy"
)

// NamespaceSegment is the path segment that stands for the sandbox's
// namespace in a policy's repositories and defaultRepository.
const NamespaceSegment = "{namespace}"

// ValidateRepository accepts a repository exactly as it will be used: an
// explicit registry host (a name with a dot or a port, or localhost) and a
// lowercase path, no tag or digest. Anything a parser would rewrite (a Docker
// Hub short name, upper case, a tag) is refused rather than rewritten, so the
// repository checked is the repository pushed to.
func ValidateRepository(repo string) error {
	host, rest, ok := strings.Cut(repo, "/")
	if !ok || rest == "" {
		return fmt.Errorf("want <registry host>/<path>")
	}
	if !explicitHost(host) {
		return fmt.Errorf("the registry host %q must be explicit (contain a dot or a port, or be localhost)", host)
	}
	r, err := name.NewRepository(repo, name.StrictValidation)
	if err != nil {
		return err
	}
	if canon := r.RegistryStr() + "/" + r.RepositoryStr(); canon != repo {
		return fmt.Errorf("not in canonical form (%s)", canon)
	}
	return nil
}

func explicitHost(h string) bool {
	return h == "localhost" || strings.HasPrefix(h, "localhost:") || strings.ContainsAny(h, ".:")
}

// Validate checks a policy's spec. A policy that fails it still selects its
// namespaces and refuses every output there (fail closed).
func Validate(spec *sandboxv1alpha1.SwiftArtifactExportPolicySpec) error {
	if len(spec.Repositories) == 0 || len(spec.Repositories) > 16 {
		return fmt.Errorf("repositories: between 1 and 16")
	}
	for i, g := range spec.Repositories {
		if err := validateGlob(g); err != nil {
			return fmt.Errorf("repositories[%d] %q: %v", i, g, err)
		}
	}
	if d := spec.DefaultRepository; d != "" {
		if strings.Contains(d, "*") {
			return fmt.Errorf("defaultRepository %q: no globs", d)
		}
		if err := ValidateRepository(Expand(d, "x")); err != nil {
			return fmt.Errorf("defaultRepository %q: %v", d, err)
		}
		if err := segmentRule(d); err != nil {
			return fmt.Errorf("defaultRepository %q: %v", d, err)
		}
	}
	if spec.CredentialsSecretRef.Name == "" {
		return fmt.Errorf("credentialsSecretRef.name is required")
	}
	if q := spec.MaxSize; q != nil && (q.Sign() <= 0 || q.Cmp(sandboxv1alpha1.MaxOutputSize) > 0) {
		return fmt.Errorf("maxSize %s: more than 0, at most %s", q.String(), sandboxv1alpha1.MaxOutputSize.String())
	}
	if n := spec.MaxEntries; n != nil && (*n < 1 || *n > sandboxv1alpha1.MaxOutputEntries) {
		return fmt.Errorf("maxEntries %d: between 1 and %d", *n, sandboxv1alpha1.MaxOutputEntries)
	}
	if t := spec.UploadTimeout; t != nil && (t.Duration <= 0 || t.Duration > sandboxv1alpha1.MaxOutputUploadTimeout) {
		return fmt.Errorf("uploadTimeout %s: more than 0, at most %s", t.Duration, sandboxv1alpha1.MaxOutputUploadTimeout)
	}
	return nil
}

// validateGlob is the verification-policy glob grammar with two additions: a
// segment may be exactly {namespace}, and the first segment (the registry
// host) is literal and explicit.
func validateGlob(g string) error {
	if err := segmentRule(g); err != nil {
		return err
	}
	host, _, ok := strings.Cut(g, "/")
	if !ok {
		return fmt.Errorf("want <registry host>/<path glob>")
	}
	if strings.Contains(host, "*") || !explicitHost(host) {
		return fmt.Errorf("the registry host %q must be literal and explicit", host)
	}
	return artifactpolicy.ValidGlob(Expand(g, "x"))
}

// segmentRule: {namespace} only ever as a whole segment.
func segmentRule(s string) error {
	for _, seg := range strings.Split(s, "/") {
		if strings.Contains(seg, "{") || strings.Contains(seg, "}") {
			if seg != NamespaceSegment {
				return fmt.Errorf("%s must be a whole path segment", NamespaceSegment)
			}
		}
	}
	return nil
}

// Expand replaces every {namespace} segment with the namespace.
func Expand(s, namespace string) string {
	segs := strings.Split(s, "/")
	for i, seg := range segs {
		if seg == NamespaceSegment {
			segs[i] = namespace
		}
	}
	return strings.Join(segs, "/")
}

// Allows reports whether the policy's repositories allow repo for a sandbox
// in namespace.
func Allows(spec *sandboxv1alpha1.SwiftArtifactExportPolicySpec, namespace, repo string) bool {
	for _, g := range spec.Repositories {
		if artifactpolicy.MatchRepository(Expand(g, namespace), repo) {
			return true
		}
	}
	return false
}

// UploadTimeout is the policy's upload timeout, defaulted.
func UploadTimeout(spec *sandboxv1alpha1.SwiftArtifactExportPolicySpec) time.Duration {
	if spec.UploadTimeout != nil {
		return spec.UploadTimeout.Duration
	}
	return sandboxv1alpha1.DefaultOutputUploadTimeout
}

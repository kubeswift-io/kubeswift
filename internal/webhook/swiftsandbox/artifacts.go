package swiftsandbox

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"k8s.io/apimachinery/pkg/util/validation"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// mountPathRE limits a mount path to what survives the kernel cmdline
// (kubeswift.mounts=<tag>:<path>,...) unquoted.
var mountPathRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// ValidateArtifacts checks spec.artifacts. The controller runs it too.
func ValidateArtifacts(s *sandboxv1alpha1.SwiftSandboxSpec) error {
	if len(s.Artifacts) > 8 {
		return fmt.Errorf("spec.artifacts has %d entries, at most 8", len(s.Artifacts))
	}
	var mounts []string
	if s.Model != nil {
		mounts = append(mounts, s.Model.ModelMountPath())
	}
	names := map[string]bool{}
	for i, a := range s.Artifacts {
		at := fmt.Sprintf("spec.artifacts[%d]", i)
		if errs := validation.IsDNS1123Label(a.Name); len(errs) > 0 || len(a.Name) > 40 {
			return fmt.Errorf("%s.name %q: a DNS label of at most 40 characters", at, a.Name)
		}
		if names[a.Name] {
			return fmt.Errorf("%s.name %q is used twice", at, a.Name)
		}
		names[a.Name] = true
		if _, err := name.ParseReference(a.Ref); err != nil {
			return fmt.Errorf("%s.ref %q: %v", at, a.Ref, err)
		}
		if l := a.Layout; l != "" && l != "oci" && l != "unpacked" {
			return fmt.Errorf("%s.layout %q: oci or unpacked", at, l)
		}
		if a.PullSecretRef != nil && a.PullSecretRef.Name == "" {
			return fmt.Errorf("%s.pullSecretRef.name is required when pullSecretRef is set", at)
		}
		if a.VerifyKeySecretRef != nil && a.VerifyKeySecretRef.Name == "" {
			return fmt.Errorf("%s.verifyKeySecretRef.name is required when verifyKeySecretRef is set", at)
		}
		p := a.MountPath
		if !mountPathRE.MatchString(p) || path.Clean(p) != p || strings.Contains(p, "/./") {
			return fmt.Errorf("%s.mountPath %q: a clean absolute path of letters, digits and . _ - /", at, p)
		}
		for _, d := range guestMountedDirs {
			if p == d || strings.HasPrefix(p, d+"/") {
				return fmt.Errorf("%s.mountPath %q: %s is the guest's own", at, p, d)
			}
		}
		for _, m := range mounts {
			if nested(p, m) {
				return fmt.Errorf("%s.mountPath %q overlaps the mount at %s", at, p, m)
			}
		}
		mounts = append(mounts, p)
	}
	return nil
}

// nested reports whether a and b are the same path or one is inside the other.
func nested(a, b string) bool {
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}

// artifactMountPaths are the read-only mount paths of spec.artifacts.
func artifactMountPaths(s *sandboxv1alpha1.SwiftSandboxSpec) []string {
	var out []string
	for _, a := range s.Artifacts {
		out = append(out, a.MountPath)
	}
	return out
}

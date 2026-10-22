package exportpolicy

import (
	"strings"
	"testing"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func policy(repos ...string) *sandboxv1alpha1.SwiftArtifactExportPolicySpec {
	return &sandboxv1alpha1.SwiftArtifactExportPolicySpec{
		Repositories:         repos,
		DefaultRepository:    "registry.example.com/outputs/{namespace}",
		CredentialsSecretRef: sandboxv1alpha1.ExportCredentialsReference{Name: "push"},
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(policy("registry.example.com/outputs/{namespace}/**")); err != nil {
		t.Fatalf("valid policy refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		spec *sandboxv1alpha1.SwiftArtifactExportPolicySpec
		want string
	}{
		{"glob host", policy("*/outputs/**"), "literal"},
		{"short host", policy("outputs/{namespace}/**"), "explicit"},
		{"partial namespace segment", policy("registry.example.com/outputs/{namespace}*"), "whole path segment"},
		{"namespace prefix", policy("registry.example.com/outputs/team-{namespace}/**"), "whole path segment"},
		{"glob default", func() *sandboxv1alpha1.SwiftArtifactExportPolicySpec {
			p := policy("registry.example.com/outputs/**")
			p.DefaultRepository = "registry.example.com/outputs/*"
			return p
		}(), "no globs"},
		{"no credential", func() *sandboxv1alpha1.SwiftArtifactExportPolicySpec {
			p := policy("registry.example.com/outputs/**")
			p.CredentialsSecretRef.Name = ""
			return p
		}(), "credentialsSecretRef"},
		{"no repositories", policy(), "between 1 and 16"},
	} {
		err := Validate(tc.spec)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.want)
		}
	}
}

// {namespace} gives every namespace its own path: one namespace's sandbox
// cannot name another's, nor a namespace that only starts the same way.
func TestAllowsIsolatesNamespaces(t *testing.T) {
	p := policy("registry.example.com/outputs/{namespace}/**")
	if !Allows(p, "team", "registry.example.com/outputs/team/compiled") {
		t.Error("own path refused")
	}
	for _, repo := range []string{
		"registry.example.com/outputs/team-b/compiled",
		"registry.example.com/outputs/compiled",
		"other.example.com/outputs/team/compiled",
	} {
		if Allows(p, "team", repo) {
			t.Errorf("namespace team allowed %s", repo)
		}
	}
}

func TestValidateRepository(t *testing.T) {
	for _, ok := range []string{"registry.example.com/a/b", "localhost:5000/x", "10.0.0.1:5000/x"} {
		if err := ValidateRepository(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"a/b", "registry.example.com", "registry.example.com/a:tag", "registry.example.com/A", "registry.example.com/a@sha256:" + strings.Repeat("a", 64)} {
		if err := ValidateRepository(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

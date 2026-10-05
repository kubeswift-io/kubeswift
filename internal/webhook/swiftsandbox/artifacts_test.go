package swiftsandbox

import (
	"strings"
	"testing"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func arts(a ...sandboxv1alpha1.SandboxArtifact) *sandboxv1alpha1.SwiftSandboxSpec {
	return &sandboxv1alpha1.SwiftSandboxSpec{Image: "x", Artifacts: a}
}

func art(name, mountPath string) sandboxv1alpha1.SandboxArtifact {
	return sandboxv1alpha1.SandboxArtifact{Name: name, Ref: "registry.example.com/team/app@sha256:" + strings.Repeat("a", 64), MountPath: mountPath}
}

func TestValidateArtifacts(t *testing.T) {
	if err := ValidateArtifacts(arts(art("app", "/run/artifacts/app"), art("policy", "/etc/policy"))); err != nil {
		t.Errorf("unexpected error %v", err)
	}
	withModel := arts(art("app", "/model/app"))
	withModel.Model = &sandboxv1alpha1.SandboxModel{ImageRef: "m"}
	badLayout := art("app", "/a")
	badLayout.Layout = "tar"
	badRef := art("app", "/a")
	badRef.Ref = "not a ref!"
	for _, tc := range []struct {
		name, want string
		s          *sandboxv1alpha1.SwiftSandboxSpec
	}{
		{"name twice", "used twice", arts(art("app", "/a"), art("app", "/b"))},
		{"bad name", "DNS label", arts(art("App_1", "/a"))},
		{"bad ref", "ref", arts(badRef)},
		{"bad layout", "oci or unpacked", arts(badLayout)},
		{"relative path", "clean absolute", arts(art("app", "run/x"))},
		{"comma in path", "clean absolute", arts(art("app", "/run/a,b"))},
		{"under /dev", "guest's own", arts(art("app", "/dev/x"))},
		{"nested mounts", "overlaps", arts(art("a", "/run/artifacts"), art("b", "/run/artifacts/b"))},
		{"inside the model mount", "overlaps", withModel},
	} {
		err := ValidateArtifacts(tc.s)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
	// A secret file inside an artifact mount (read-only) is refused.
	s := arts(art("app", "/run/artifacts/app"))
	s.SecretFiles = []sandboxv1alpha1.SandboxSecretFile{{SecretName: "x", Items: []sandboxv1alpha1.SandboxSecretFileItem{{Key: "k", Path: "/run/artifacts/app/token"}}}}
	if err := ValidateSecretFiles(s); err == nil {
		t.Error("a secret file inside a read-only artifact mount must be refused")
	}
}

package swiftsandbox

import (
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func files(f ...sandboxv1alpha1.SandboxSecretFile) *sandboxv1alpha1.SwiftSandboxSpec {
	return &sandboxv1alpha1.SwiftSandboxSpec{Image: "x", SecretFiles: f}
}

func item(key, path string) sandboxv1alpha1.SandboxSecretFileItem {
	return sandboxv1alpha1.SandboxSecretFileItem{Key: key, Path: path}
}

func TestValidateSecretFiles(t *testing.T) {
	ok := files(sandboxv1alpha1.SandboxSecretFile{SecretName: "registry-auth", Mode: ptr.To(int32(0o440)),
		Items: []sandboxv1alpha1.SandboxSecretFileItem{item("config.json", "/run/secrets/registry/config.json"), item("ca.crt", "/etc/ssl/app/ca.crt")}})
	if err := ValidateSecretFiles(ok); err != nil {
		t.Errorf("unexpected error %v", err)
	}
	model := files(sandboxv1alpha1.SandboxSecretFile{SecretName: "s", Items: []sandboxv1alpha1.SandboxSecretFileItem{item("k", "/model/token")}})
	model.Model = &sandboxv1alpha1.SandboxModel{ImageRef: "m"}
	for _, tc := range []struct {
		name, want string
		s          *sandboxv1alpha1.SwiftSandboxSpec
	}{
		{"relative path", "clean absolute", files(sandboxv1alpha1.SandboxSecretFile{SecretName: "s", Items: []sandboxv1alpha1.SandboxSecretFileItem{item("k", "run/x")}})},
		{"dot-dot", "clean absolute", files(sandboxv1alpha1.SandboxSecretFile{SecretName: "s", Items: []sandboxv1alpha1.SandboxSecretFileItem{item("k", "/run/../etc/x")}})},
		{"root", "clean absolute", files(sandboxv1alpha1.SandboxSecretFile{SecretName: "s", Items: []sandboxv1alpha1.SandboxSecretFileItem{item("k", "/")}})},
		{"under /proc", "mounted over", files(sandboxv1alpha1.SandboxSecretFile{SecretName: "s", Items: []sandboxv1alpha1.SandboxSecretFileItem{item("k", "/proc/x")}})},
		{"under the model mount", "mounted over", model},
		{"same path twice", "used twice", files(
			sandboxv1alpha1.SandboxSecretFile{SecretName: "a", Items: []sandboxv1alpha1.SandboxSecretFileItem{item("k", "/run/x")}},
			sandboxv1alpha1.SandboxSecretFile{SecretName: "b", Items: []sandboxv1alpha1.SandboxSecretFileItem{item("k", "/run/x")}})},
		{"no items", "between 1 and 32", files(sandboxv1alpha1.SandboxSecretFile{SecretName: "s"})},
		{"bad key", "key", files(sandboxv1alpha1.SandboxSecretFile{SecretName: "s", Items: []sandboxv1alpha1.SandboxSecretFileItem{item("a/b", "/run/x")}})},
		{"bad mode", "permission mode", files(sandboxv1alpha1.SandboxSecretFile{SecretName: "s", Mode: ptr.To(int32(0o1777)),
			Items: []sandboxv1alpha1.SandboxSecretFileItem{item("k", "/run/x")}})},
		{"bad secret name", "secretName", files(sandboxv1alpha1.SandboxSecretFile{SecretName: "Bad_Name", Items: []sandboxv1alpha1.SandboxSecretFileItem{item("k", "/run/x")}})},
	} {
		err := ValidateSecretFiles(tc.s)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

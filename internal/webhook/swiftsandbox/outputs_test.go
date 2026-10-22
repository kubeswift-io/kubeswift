package swiftsandbox

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func outputSpec(o ...sandboxv1alpha1.SandboxOutput) *sandboxv1alpha1.SwiftSandboxSpec {
	return &sandboxv1alpha1.SwiftSandboxSpec{
		Image:     "alpine:3.20",
		Outputs:   o,
		Artifacts: []sandboxv1alpha1.SandboxArtifact{{Name: "in", Ref: "registry.example.com/in@sha256:" + strings.Repeat("a", 64), MountPath: "/input"}},
		SecretFiles: []sandboxv1alpha1.SandboxSecretFile{{SecretName: "s", Items: []sandboxv1alpha1.SandboxSecretFileItem{
			{Key: "k", Path: "/etc/creds/token"}}}},
	}
}

func TestValidateOutputs(t *testing.T) {
	ok := sandboxv1alpha1.SandboxOutput{Name: "out", Path: "/output/cache"}
	if err := ValidateOutputs(outputSpec(ok)); err != nil {
		t.Fatalf("valid output refused: %v", err)
	}
	full := ok
	full.Repository = "registry.example.com/platform/outputs/team-a/out"
	full.MaxSize = ptr.To(resource.MustParse("512Mi"))
	full.MaxEntries = ptr.To(int64(1000))
	full.ArtifactType = "application/vnd.example.cache.v1"
	full.Annotations = map[string]string{"example.com/input": "sha256:abc"}
	if err := ValidateOutputs(outputSpec(full)); err != nil {
		t.Fatalf("valid output with every field refused: %v", err)
	}

	for _, tc := range []struct {
		name string
		edit func(*sandboxv1alpha1.SandboxOutput)
		want string
	}{
		{"bad name", func(o *sandboxv1alpha1.SandboxOutput) { o.Name = "Out_1" }, "DNS label"},
		{"relative path", func(o *sandboxv1alpha1.SandboxOutput) { o.Path = "output" }, "absolute"},
		{"traversal", func(o *sandboxv1alpha1.SandboxOutput) { o.Path = "/output/../etc" }, "clean"},
		{"root", func(o *sandboxv1alpha1.SandboxOutput) { o.Path = "/" }, "other than /"},
		{"guest dir", func(o *sandboxv1alpha1.SandboxOutput) { o.Path = "/proc/x" }, "guest's own"},
		{"over an artifact", func(o *sandboxv1alpha1.SandboxOutput) { o.Path = "/input/sub" }, "overlaps"},
		{"over a secret file", func(o *sandboxv1alpha1.SandboxOutput) { o.Path = "/etc/creds" }, "overlaps"},
		{"tag in repository", func(o *sandboxv1alpha1.SandboxOutput) { o.Repository = "registry.example.com/x:latest" }, "repository"},
		{"docker hub short name", func(o *sandboxv1alpha1.SandboxOutput) { o.Repository = "library/out" }, "explicit"},
		{"upper case", func(o *sandboxv1alpha1.SandboxOutput) { o.Repository = "registry.example.com/Team/out" }, "repository"},
		{"maxSize too big", func(o *sandboxv1alpha1.SandboxOutput) { o.MaxSize = ptr.To(resource.MustParse("11Gi")) }, "maxSize"},
		{"maxSize zero", func(o *sandboxv1alpha1.SandboxOutput) { o.MaxSize = ptr.To(resource.MustParse("0")) }, "maxSize"},
		{"maxEntries", func(o *sandboxv1alpha1.SandboxOutput) { o.MaxEntries = ptr.To(int64(0)) }, "maxEntries"},
		{"artifactType", func(o *sandboxv1alpha1.SandboxOutput) { o.ArtifactType = "not a type" }, "media type"},
		{"reserved annotation", func(o *sandboxv1alpha1.SandboxOutput) {
			o.Annotations = map[string]string{"org.opencontainers.image.created": "x"}
		}, "reserved"},
		{"kubeswift annotation", func(o *sandboxv1alpha1.SandboxOutput) {
			o.Annotations = map[string]string{"io.kubeswift.namespace": "other"}
		}, "reserved"},
		{"control character", func(o *sandboxv1alpha1.SandboxOutput) {
			o.Annotations = map[string]string{"example.com/k": "a\nb"}
		}, "control"},
	} {
		o := ok
		tc.edit(&o)
		err := ValidateOutputs(outputSpec(o))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.want)
		}
	}

	if err := ValidateOutputs(outputSpec(ok, sandboxv1alpha1.SandboxOutput{Name: "out", Path: "/other"})); err == nil {
		t.Error("duplicate output names accepted")
	}
	if err := ValidateOutputs(outputSpec(ok, sandboxv1alpha1.SandboxOutput{Name: "b", Path: "/output"})); err == nil {
		t.Error("nested outputs accepted")
	}
	five := outputSpec()
	for i := 0; i < 5; i++ {
		five.Outputs = append(five.Outputs, sandboxv1alpha1.SandboxOutput{Name: string(rune('a' + i)), Path: "/o" + string(rune('a'+i))})
	}
	if err := ValidateOutputs(five); err == nil {
		t.Error("five outputs accepted")
	}
}

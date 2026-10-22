package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/oci"
)

func caEnv(c corev1.Container) string {
	for _, e := range c.Env {
		if e.Name == oci.RegistryCAEnv {
			return e.Value
		}
	}
	return ""
}

// The administrator's registry CA reaches every sandbox-materialize
// container (rootfs, model, artifacts, export) and nothing else; without
// one, pods are unchanged (#839).
func TestRegistryCAReachesEveryPull(t *testing.T) {
	sb := plainSandbox("registry.example.com/app@sha256:" + "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9")
	sb.UID = testSBUID
	sb.Spec.Model = &sandboxv1alpha1.SandboxModel{ImageRef: "registry.example.com/model:1"}
	sb.Spec.Artifacts = []sandboxv1alpha1.SandboxArtifact{{Name: "tools", Ref: "registry.example.com/tools:1", MountPath: "/opt/tools"}}
	sb.Spec.Memory = resource.MustParse("512Mi")
	sb.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{{Name: "tools", Digest: "sha256:" + strings.Repeat("ab", 32)}}
	materializers := func() []corev1.Container {
		var out []corev1.Container
		pod := buildPod(sb, "sandbox")
		for _, c := range pod.Spec.InitContainers {
			if c.Image == SandboxMaterializeImage() {
				out = append(out, c)
			}
		}
		inits, _ := artifactInits(sb, false)
		out = append(out, inits...)
		job, err := buildExportJob(sb, &sandboxv1alpha1.SandboxOutputStatus{Name: "cache", Repository: "registry.example.com/out"},
			destination{maxBytes: 1, maxEntries: 1, secret: "push"}, testSystemNS, SandboxMaterializeImage())
		if err != nil {
			t.Fatal(err)
		}
		return append(out, job.Spec.Template.Spec.Containers...)
	}

	for _, c := range materializers() {
		if caEnv(c) != "" {
			t.Errorf("%s carries a CA with none configured", c.Name)
		}
	}
	const ca = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	t.Setenv(SandboxRegistryCAEnv, ca)
	got := materializers()
	if len(got) < 4 {
		names := []string{}
		for _, c := range got {
			names = append(names, c.Name)
		}
		t.Fatalf("expected rootfs, model, artifact and export containers, got %v", names)
	}
	for _, c := range got {
		if caEnv(c) != ca {
			t.Errorf("%s does not carry the registry CA", c.Name)
		}
	}
	for _, c := range buildPod(sb, "sandbox").Spec.Containers {
		if caEnv(c) != "" {
			t.Errorf("launcher container %s carries the registry CA", c.Name)
		}
	}
}

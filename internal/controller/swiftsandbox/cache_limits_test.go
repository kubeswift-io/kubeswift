package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func TestArtifactCacheArgs(t *testing.T) {
	t.Setenv(artifactCacheMaxBytesEnv, "")
	t.Setenv(artifactCacheMaxPercentEnv, "")
	t.Setenv(artifactCacheMinAgeEnv, "")
	if got := strings.Join(artifactCacheArgs(), " "); got != "--use-dir="+artifactUseDir {
		t.Errorf("defaults: %s", got)
	}
	t.Setenv(artifactCacheMaxBytesEnv, "20Gi")
	t.Setenv(artifactCacheMaxPercentEnv, "25")
	t.Setenv(artifactCacheMinAgeEnv, "15m")
	got := strings.Join(artifactCacheArgs(), " ")
	for _, want := range []string{"--cache-max-bytes=21474836480", "--cache-max-percent=25", "--cache-min-age=15m0s"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s missing from %s", want, got)
		}
	}
	// Nonsense is ignored, never a limit of zero bytes.
	t.Setenv(artifactCacheMaxBytesEnv, "lots")
	t.Setenv(artifactCacheMaxPercentEnv, "250")
	if got := strings.Join(artifactCacheArgs(), " "); strings.Contains(got, "max") {
		t.Errorf("invalid limits passed on: %s", got)
	}
}

func hasMountRW(c corev1.Container, name, path string, readOnly bool) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == name && m.MountPath == path && m.ReadOnly == readOnly {
			return true
		}
	}
	return false
}

// Every materializer run records uses (and enforces limits), and every
// launcher that serves cache entries can hold their use locks.
func TestUseLockMounts(t *testing.T) {
	sb := optSandbox(art("a", "r.example/a:1", false))
	sb.Status.Artifacts = []sandboxv1alpha1.SandboxArtifactStatus{{Name: "a", Digest: "sha256:" + strings.Repeat("1", 64), MountPath: "/a"}}
	inits, _ := artifactInits(sb, true)
	if !hasMountRW(inits[0], artifactUseVolume, artifactUseDir, false) || !strings.Contains(strings.Join(inits[0].Args, " "), "--use-dir=") {
		t.Errorf("materializer: %+v %v", inits[0].VolumeMounts, inits[0].Args)
	}
	pod := buildPod(sb, defaultKernelProfile)
	if !hasMountRW(pod.Spec.Containers[0], artifactUseVolume, artifactUseDir, false) {
		t.Error("cold launcher cannot hold use locks")
	}
	if !hasMountRW(pod.Spec.Containers[0], artifactCacheVolume, artifactCacheDir, true) {
		t.Error("the cache itself must stay read-only in the launcher")
	}
	slot := buildPod(&sandboxv1alpha1.SwiftSandbox{}, defaultKernelProfile)
	addWarmStage(slot)
	if !hasMountRW(slot.Spec.Containers[0], artifactUseVolume, artifactUseDir, false) {
		t.Error("warm slot cannot hold use locks")
	}
}

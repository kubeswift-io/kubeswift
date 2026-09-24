package swiftsandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// The node rootfs cache is shared, keyed only by image digest and reused as-is
// on a cache hit. The launcher container runs the untrusted guest, so it must
// mount the cache READ-ONLY: for a virtiofs sandbox virtiofsd shares whatever
// it can reach, and a RW mount would let guest code poison the cache for every
// later sandbox of that image on the node (defeating verify-before-boot). The
// materialize init container populates the cache, so it keeps the mount RW.
func TestBuildPod_RootfsCacheMountIsReadOnlyOnLauncherRWOnInit(t *testing.T) {
	for _, mode := range []sandboxv1alpha1.SandboxRootfsMode{
		sandboxv1alpha1.SandboxRootfsBlock,
		sandboxv1alpha1.SandboxRootfsVirtiofs,
	} {
		sb := &sandboxv1alpha1.SwiftSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default"},
			Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "alpine:3", RootfsMode: mode},
		}
		pod := buildPod(sb, "sandbox")

		launcher := containerNamed(pod.Spec.Containers, launcherName)
		if launcher == nil {
			t.Fatalf("mode=%s: no launcher container %q", mode, launcherName)
		}
		if ro, ok := mountReadOnly(launcher, "rootfs-cache"); !ok {
			t.Fatalf("mode=%s: launcher does not mount rootfs-cache", mode)
		} else if !ro {
			t.Errorf("mode=%s: launcher must mount rootfs-cache READ-ONLY (untrusted guest, shared node cache)", mode)
		}

		init := containerNamed(pod.Spec.InitContainers, materializeInitName)
		if init == nil {
			t.Fatalf("mode=%s: no materialize init container %q", mode, materializeInitName)
		}
		if ro, ok := mountReadOnly(init, "rootfs-cache"); !ok {
			t.Fatalf("mode=%s: materialize init does not mount rootfs-cache", mode)
		} else if ro {
			t.Errorf("mode=%s: materialize init must mount rootfs-cache read-write to populate it", mode)
		}
	}
}

func containerNamed(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

func mountReadOnly(c *corev1.Container, volName string) (ro bool, found bool) {
	for _, m := range c.VolumeMounts {
		if m.Name == volName {
			return m.ReadOnly, true
		}
	}
	return false, false
}

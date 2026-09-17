package swiftguest

import (
	"context"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// The clone Job and clone-grow-init used to run a stock ubuntu and
// `apt-get install qemu-utils gdisk` on every clone, putting a network
// round-trip and an apt mirror in the path of every guest creation. Both now
// run the launcher image, which ships those tools.
func TestCloneJob_RunsLauncherImageAndInstallsNothing(t *testing.T) {
	// Both branches: the Filesystem script (cp + qemu-img resize) and the Block
	// script (qemu-img convert) each had their own apt-get, so covering only one
	// would leave the other free to reintroduce it.
	for _, mode := range []string{"Filesystem", "Block"} {
		t.Run(mode, func(t *testing.T) { assertCloneJobClean(t, mode) })
	}
}

func assertCloneJobClean(t *testing.T, volumeMode string) {
	t.Helper()
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "40Gi")
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: RootDiskCloneName("g1"), Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, schema.GroupVersionKind{
				Group: "swift.kubeswift.io", Version: "v1alpha1", Kind: "SwiftGuest",
			})},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src, clone).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}
	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{PVCName: "src-img"},
		RootDisk:      resolved.RootDisk{Size: resource.MustParse("40Gi")},
		Storage:       resolved.Storage{VolumeMode: volumeMode},
	}
	_, _ = r.EnsureRootDiskClone(context.Background(), guest, rg)

	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Name: CloneJobName("g1"), Namespace: "default"}, &job); err != nil {
		t.Fatalf("clone Job not created: %v", err)
	}
	ctr := job.Spec.Template.Spec.Containers[0]
	if ctr.Image != LauncherImage() {
		t.Errorf("clone Job image = %q, want the launcher image %q", ctr.Image, LauncherImage())
	}
	script := strings.Join(ctr.Command, " ")
	if strings.Contains(script, "apt-get") {
		t.Errorf("clone Job still installs packages at run time:\n%s", script)
	}
	// The tools it relies on must actually be invoked, so the test fails if a
	// future edit drops them along with the install.
	for _, tool := range []string{"qemu-img", "sgdisk"} {
		if !strings.Contains(script, tool) {
			t.Errorf("%s clone script no longer uses %s; the image requirement may have drifted", volumeMode, tool)
		}
	}
}

// clone-grow-init runs inside the launcher pod, so reusing the launcher image
// costs no extra pull at all.
func TestCloneGrowInit_RunsLauncherImageAndInstallsNothing(t *testing.T) {
	for _, mode := range []string{"Filesystem", "Block"} {
		isBlock := mode == "Block"
		rg := &resolved.ResolvedGuest{Storage: resolved.Storage{VolumeMode: mode}}
		ctr := cloneGrowInitContainer(rg, 42*1024*1024*1024)
		if ctr.Image != LauncherImage() {
			t.Errorf("block=%v: image = %q, want the launcher image %q", isBlock, ctr.Image, LauncherImage())
		}
		script := strings.Join(ctr.Command, " ")
		if strings.Contains(script, "apt-get") {
			t.Errorf("block=%v: clone-grow-init still installs packages at run time:\n%s", isBlock, script)
		}
		if !strings.Contains(script, "sgdisk") {
			t.Errorf("block=%v: script no longer uses sgdisk", isBlock)
		}
	}
}

// The launcher image is configurable, and the clone path must follow it rather
// than pinning a second image that upgrades separately.
func TestCloneImages_FollowLauncherImageOverride(t *testing.T) {
	t.Setenv(LauncherImageEnv, "registry.example.com/kubeswift/swiftletd:v9.9.9")
	if got := CloneJobImage(); got != os.Getenv(LauncherImageEnv) {
		t.Errorf("CloneJobImage() = %q, want the overridden launcher image", got)
	}
	if got := CloneGrowInitImage(); got != os.Getenv(LauncherImageEnv) {
		t.Errorf("CloneGrowInitImage() = %q, want the overridden launcher image", got)
	}
}

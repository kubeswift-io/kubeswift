package swiftguest

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func cowParams() RestoreParams {
	return RestoreParams{
		SnapshotPath:      "/var/lib/kubeswift/snapshots/default/snap1",
		NodeName:          "worker-1",
		Mode:              RestoreModeClone,
		MemoryRestoreMode: "copyonwrite",
	}
}

func hasArg(c *corev1.Container, arg string) bool {
	for _, a := range c.Args {
		if a == arg {
			return true
		}
	}
	return false
}

// A copyonwrite clone's launcher mounts the snapshot source read-only (the
// staged memory-ranges links to it), its stager links instead of copying,
// and the pod is marked so a live migration refuses it.
func TestBuildRestorePod_CopyOnWrite(t *testing.T) {
	clone := &RootDiskCloneResult{PVCName: "swiftguest-root-clone1"}
	pod := BuildRestorePod(minimalGuest(), minimalResolved(), "", "intent", clone, cowParams())

	launcher := &pod.Spec.Containers[0]
	src := findMount(launcher, snapshotSourceVolume)
	if src == nil || !src.ReadOnly || src.MountPath != RestoreSourcePath {
		t.Errorf("launcher source mount = %+v, want read-only at %s", src, RestoreSourcePath)
	}
	if stager := findInit(pod, SnapshotStagerInitContainerName); stager == nil || !hasArg(stager, "--memory-from-source=true") {
		t.Errorf("stager does not link the memory file: %+v", stager)
	}
	if pod.Annotations[AnnotationMemoryCopyOnWrite] != "true" {
		t.Errorf("pod annotations = %v, want %s", pod.Annotations, AnnotationMemoryCopyOnWrite)
	}

	// ondemand: none of it.
	p := cowParams()
	p.MemoryRestoreMode = "ondemand"
	pod = BuildRestorePod(minimalGuest(), minimalResolved(), "", "intent", clone, p)
	if findMount(&pod.Spec.Containers[0], snapshotSourceVolume) != nil {
		t.Error("an ondemand clone's launcher mounts the source")
	}
	if stager := findInit(pod, SnapshotStagerInitContainerName); hasArg(stager, "--memory-from-source=true") {
		t.Error("an ondemand clone's stager links the memory file")
	}
	if pod.Annotations[AnnotationMemoryCopyOnWrite] != "" {
		t.Error("an ondemand clone is marked copy-on-write")
	}
	// An in-place restore never maps copy-on-write.
	if (RestoreParams{Mode: RestoreModeInPlace, MemoryRestoreMode: "copyonwrite"}).CopyOnWrite() {
		t.Error("in-place restore treated as copy-on-write")
	}
}

func TestCloneMemoryMode(t *testing.T) {
	if got := cloneMemoryMode(nil); got != "ondemand" {
		t.Errorf("nil = %q", got)
	}
	if got := cloneMemoryMode(&swiftv1alpha1.CloneFromSnapshotSource{}); got != "ondemand" {
		t.Errorf("default = %q", got)
	}
	if got := cloneMemoryMode(&swiftv1alpha1.CloneFromSnapshotSource{MemoryRestoreMode: swiftv1alpha1.CloneMemoryCopyOnWrite}); got != "copyonwrite" {
		t.Errorf("copyonwrite = %q", got)
	}
}

func TestCopyOnWriteRefusals(t *testing.T) {
	snap := func(b snapshotv1alpha1.SnapshotBackendType) *snapshotv1alpha1.SwiftSnapshot {
		return &snapshotv1alpha1.SwiftSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "s"},
			Spec: snapshotv1alpha1.SwiftSnapshotSpec{Backend: snapshotv1alpha1.SwiftSnapshotBackend{Type: b}}}
	}
	if msg := copyOnWriteBackendRefusal(snap(snapshotv1alpha1.SnapshotBackendLocal)); msg != "" {
		t.Errorf("local refused: %s", msg)
	}
	for _, b := range []snapshotv1alpha1.SnapshotBackendType{snapshotv1alpha1.SnapshotBackendS3, snapshotv1alpha1.SnapshotBackendOCI} {
		if msg := copyOnWriteBackendRefusal(snap(b)); !strings.Contains(msg, "needs a local snapshot") {
			t.Errorf("%s: %q", b, msg)
		}
	}

	scheme := rootdiskScheme(t)
	gv := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	scheme.AddKnownTypes(gv, &swiftv1alpha1.SwiftGuestClass{}, &swiftv1alpha1.SwiftGuestClassList{})
	plain := &swiftv1alpha1.SwiftGuestClass{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}
	huge := &swiftv1alpha1.SwiftGuestClass{ObjectMeta: metav1.ObjectMeta{Name: "huge"},
		Spec: swiftv1alpha1.SwiftGuestClassSpec{Hugepages: swiftv1alpha1.Hugepages2Mi}}
	r := &SwiftGuestReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(plain, huge).Build(), Scheme: scheme}
	src := func(class string) *swiftv1alpha1.SwiftGuest {
		return &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{GuestClassRef: corev1.LocalObjectReference{Name: class}}}
	}
	if msg, err := r.copyOnWriteRefusal(context.Background(), src("plain")); err != nil || msg != "" {
		t.Errorf("plain class: %q %v", msg, err)
	}
	if msg, _ := r.copyOnWriteRefusal(context.Background(), src("huge")); !strings.Contains(msg, "hugepages") {
		t.Errorf("hugepage class: %q", msg)
	}
}

package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func scratchSandbox(name, ns string, size string) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name + "-uid")},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image: "alpine:3",
			ScratchDisk: &sandboxv1alpha1.SandboxScratchDisk{
				Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse(size)},
			},
		},
	}
}

func TestReconcileScratchDisk_BlankProvisionsBlockPVCAndGates(t *testing.T) {
	sb := scratchSandbox("sb", "default", "5Gi")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(sb).WithStatusSubresource(sb).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}
	ctx := context.Background()

	// First pass: the PVC is created but not yet Bound → not ready.
	ready, _, err := r.reconcileScratchDisk(ctx, sb)
	if err != nil {
		t.Fatalf("reconcileScratchDisk: %v", err)
	}
	if ready {
		t.Fatal("must not be ready before the scratch PVC binds")
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-scratch"}, &pvc); err != nil {
		t.Fatalf("scratch PVC not created: %v", err)
	}
	if pvc.Spec.VolumeMode == nil || *pvc.Spec.VolumeMode != corev1.PersistentVolumeBlock {
		t.Errorf("scratch PVC volumeMode = %v, want Block", pvc.Spec.VolumeMode)
	}
	if !metav1.IsControlledBy(&pvc, sb) {
		t.Error("scratch PVC must be owned by the sandbox (GC'd with it)")
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "5Gi" {
		t.Errorf("scratch PVC size = %s, want 5Gi", got.String())
	}

	// Simulate the binder, re-reconcile → ready + status stamped.
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, &pvc); err != nil {
		t.Fatal(err)
	}
	ready, _, err = r.reconcileScratchDisk(ctx, sb)
	if err != nil {
		t.Fatalf("reconcileScratchDisk (bound): %v", err)
	}
	if !ready {
		t.Fatal("must be ready once the scratch PVC is Bound")
	}
	if sb.Status.ScratchDisk == nil || !sb.Status.ScratchDisk.Bound ||
		sb.Status.ScratchDisk.DevicePath != "/dev/kubeswift-data-scratch" ||
		sb.Status.ScratchDisk.PVCName != "sb-scratch" {
		t.Errorf("status.scratchDisk = %+v", sb.Status.ScratchDisk)
	}
}

func TestReconcileScratchDisk_PVCRefReadyWhenBound(t *testing.T) {
	block := corev1.PersistentVolumeBlock
	existing := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "cache-pvc", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &block},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default", UID: "sb-uid"},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image:       "alpine:3",
			ScratchDisk: &sandboxv1alpha1.SandboxScratchDisk{PVCRef: &corev1.LocalObjectReference{Name: "cache-pvc"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(sb, existing).WithStatusSubresource(sb).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}

	ready, _, err := r.reconcileScratchDisk(context.Background(), sb)
	if err != nil || !ready {
		t.Fatalf("pvcRef bound should be ready: ready=%v err=%v", ready, err)
	}
	// pvcRef is NOT sandbox-owned — no new PVC created, no ownership claimed.
	if sb.Status.ScratchDisk.PVCName != "cache-pvc" {
		t.Errorf("status PVCName = %q, want cache-pvc", sb.Status.ScratchDisk.PVCName)
	}
	var owned corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb-scratch"}, &owned); err == nil {
		t.Error("pvcRef must NOT create a sandbox-owned PVC")
	}
}

func TestBuildPodAndIntent_ScratchDisk(t *testing.T) {
	sb := scratchSandbox("sb", "default", "5Gi")

	ri := buildIntent(sb, "sandbox", "/cache/x.ext4", execSpec{Argv: []string{"/bin/sh"}}, false)
	if len(ri.DataDisks) != 1 || ri.DataDisks[0].Path != "/dev/kubeswift-data-scratch" || ri.DataDisks[0].Format != "raw" {
		t.Fatalf("intent dataDisks = %+v, want one raw scratch disk", ri.DataDisks)
	}

	pod := buildPod(sb, "sandbox")
	if !hasVolume(pod, "scratch-disk") {
		t.Error("pod must carry the scratch-disk PVC volume")
	}
	var launcher *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == launcherName {
			launcher = &pod.Spec.Containers[i]
		}
	}
	found := false
	for _, vd := range launcher.VolumeDevices {
		if vd.Name == "scratch-disk" && vd.DevicePath == "/dev/kubeswift-data-scratch" {
			found = true
		}
	}
	if !found {
		t.Errorf("launcher must attach scratch-disk as a block device at /dev/kubeswift-data-scratch, got %+v", launcher.VolumeDevices)
	}
}

// A blank scratch disk is guest-owned and GC'd via ownerRef — assert the
// controller sets that owner reference so nothing leaks.
func TestEnsureBlankScratchPVC_OwnerRef(t *testing.T) {
	sb := scratchSandbox("sb", "default", "1Gi")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sb).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}
	if err := r.ensureBlankScratchPVC(context.Background(), sb, "sb-scratch"); err != nil {
		t.Fatal(err)
	}
	var pvc corev1.PersistentVolumeClaim
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb-scratch"}, &pvc)
	if len(pvc.OwnerReferences) == 0 || !*pvc.OwnerReferences[0].Controller {
		t.Error("scratch PVC must have a controller ownerRef to the sandbox")
	}
}

package swiftguest

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

func blankGuest(disks ...swiftv1alpha1.DataDiskRef) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "bg", Namespace: "default", UID: "uid-bg"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "gc"},
			DataDiskRefs:  disks,
		},
	}
}

// TestEnsureBlankDataDisks_CreatesBlockPVCThenGates: first call creates the
// guest-owned Block PVC and returns an error (requeue); once the PVC is Bound a
// later call returns nil. No fill Job for a Block disk.
func TestEnsureBlankDataDisks_CreatesBlockPVCThenGates(t *testing.T) {
	scheme := rootdiskScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}
	guest := blankGuest(swiftv1alpha1.DataDiskRef{Name: "db", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("20Gi")}})
	rg := &resolved.ResolvedGuest{}

	// First call: PVC absent -> created, error returned (requeue).
	if err := r.EnsureBlankDataDisks(context.Background(), guest, rg); err == nil {
		t.Fatal("expected requeue error while blank PVC is unbound")
	}

	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Name: "bg-data-db", Namespace: "default"}, &pvc); err != nil {
		t.Fatalf("blank PVC not created: %v", err)
	}
	if pvc.Spec.VolumeMode == nil || *pvc.Spec.VolumeMode != corev1.PersistentVolumeBlock {
		t.Errorf("blank PVC volumeMode = %v, want Block", pvc.Spec.VolumeMode)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Errorf("blank PVC size = %v, want 20Gi", got)
	}
	if !metav1.IsControlledBy(&pvc, guest) {
		t.Error("blank PVC should be owned by the guest")
	}

	// Mark Bound; now EnsureBlankDataDisks should pass.
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(context.Background(), &pvc); err != nil {
		t.Fatalf("update PVC status: %v", err)
	}
	if err := r.EnsureBlankDataDisks(context.Background(), guest, rg); err != nil {
		t.Fatalf("expected nil once blank PVC Bound, got %v", err)
	}
}

// TestEnsureBlankDataDisks_FilesystemRunsFillJob: a Filesystem blank disk binds,
// then a fill Job is created and gated on.
func TestEnsureBlankDataDisks_FilesystemRunsFillJob(t *testing.T) {
	scheme := rootdiskScheme(t)
	fsMode := corev1.PersistentVolumeFilesystem
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "bg-data-fs", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &fsMode},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	guest := blankGuest(swiftv1alpha1.DataDiskRef{Name: "fs", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("5Gi"), VolumeMode: corev1.PersistentVolumeFilesystem}})
	// Owner-ref so the controlled-by check passes.
	pvc.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	// Bound PVC but no fill Job yet -> Job created, error (requeue).
	if err := r.EnsureBlankDataDisks(context.Background(), guest, &resolved.ResolvedGuest{}); err == nil {
		t.Fatal("expected requeue error while fill Job runs")
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Name: blankFillJobName("bg", "fs"), Namespace: "default"}, &job); err != nil {
		t.Fatalf("fill Job not created: %v", err)
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	if !strings.Contains(script, "truncate -s") {
		t.Errorf("fill Job script should truncate image.raw, got: %s", script)
	}

	// Mark the Job complete; EnsureBlankDataDisks should now pass.
	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), &job); err != nil {
		t.Fatalf("update Job status: %v", err)
	}
	if err := r.EnsureBlankDataDisks(context.Background(), guest, &resolved.ResolvedGuest{}); err != nil {
		t.Fatalf("expected nil once fill Job complete, got %v", err)
	}
}

func TestBlankDataDiskPVCName(t *testing.T) {
	if got := resolved.BlankDataDiskPVCName("guest1", "db"); got != "guest1-data-db" {
		t.Errorf("BlankDataDiskPVCName = %q, want guest1-data-db", got)
	}
}

// TestBuildPod_BlankBlockDataDisk_VolumeDevices: a resolved Block data disk is
// attached via volumeDevices (not a filesystem mount), at its host device path.
func TestBuildPod_BlankBlockDataDisk_VolumeDevices(t *testing.T) {
	guest := blankGuest(swiftv1alpha1.DataDiskRef{Name: "db", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("20Gi")}})
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc-root"},
		DataDisks: []resolved.ResolvedDataDisk{
			{Name: "db", PVCName: "bg-data-db", Block: true, HostPath: "/dev/kubeswift-data-db", Format: "raw", Ready: true},
		},
	}
	pod := BuildPod(guest, rg, "", "intent", nil)
	launcher := pod.Spec.Containers[0]
	found := false
	for _, d := range launcher.VolumeDevices {
		if d.Name == "data-disk-db" && d.DevicePath == "/dev/kubeswift-data-db" {
			found = true
		}
	}
	if !found {
		t.Errorf("blank Block data disk should attach as a VolumeDevice; got %v", launcher.VolumeDevices)
	}
	// And NOT as a filesystem mount.
	for _, m := range launcher.VolumeMounts {
		if m.Name == "data-disk-db" {
			t.Error("Block data disk must not be a VolumeMount")
		}
	}
}

// TestApplyDataDiskRefs_SkipsAttachAsDisk: a plain pvcRef is filesystem-mounted;
// a pvcRef WITH attachAsDisk is a VM disk and must NOT be fs-mounted here.
func TestApplyDataDiskRefs_SkipsAttachAsDisk(t *testing.T) {
	guest := blankGuest(
		swiftv1alpha1.DataDiskRef{Name: "pool", PVCRef: &corev1.LocalObjectReference{Name: "pool-pvc"}},
		swiftv1alpha1.DataDiskRef{Name: "vmdisk", PVCRef: &corev1.LocalObjectReference{Name: "vm-pvc"}, AttachAsDisk: true},
	)
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "launcher"}}}}
	applyDataDiskRefs(pod, guest)

	foundPool, foundVM := false, false
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		switch v.PersistentVolumeClaim.ClaimName {
		case "pool-pvc":
			foundPool = true
		case "vm-pvc":
			foundVM = true
		}
	}
	if !foundPool {
		t.Error("plain pvcRef should be filesystem-mounted by applyDataDiskRefs")
	}
	if foundVM {
		t.Error("attachAsDisk pvcRef must NOT be filesystem-mounted by applyDataDiskRefs")
	}
}

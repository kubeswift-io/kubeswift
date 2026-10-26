package swiftguest

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// An image-backed data disk used to attach the SwiftImage's own PVC to the
// guest, writable: the guest changed the image for every later guest, and a
// second guest using the image could not start while one ran (Cloud
// Hypervisor's disk lock). Each guest now gets its own copy.

func imageDataDiskRG(guestName string) *resolved.ResolvedGuest {
	return &resolved.ResolvedGuest{DataDisks: []resolved.ResolvedDataDisk{{
		Name: "seeded", PVCName: resolved.BlankDataDiskPVCName(guestName, "seeded"), Block: true,
		Ready: true, SourcePVC: "swiftimage-import-dimg", SourceSize: 117440512 + 1,
	}}}
}

func TestEnsureImageDataDisks_CopiesIntoGuestPVC(t *testing.T) {
	scheme := rootdiskScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}
	guest := blankGuest(swiftv1alpha1.DataDiskRef{Name: "seeded", ImageRef: &corev1.LocalObjectReference{Name: "dimg"}})
	rg := imageDataDiskRG(guest.Name)
	ctx := context.Background()

	if err := r.EnsureImageDataDisks(ctx, guest, rg); err == nil || !strings.Contains(err.Error(), "copy Job") {
		t.Fatalf("first pass: want a wait on the copy Job, got %v", err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "bg-data-seeded"}, &pvc); err != nil {
		t.Fatalf("guest-owned PVC not created: %v", err)
	}
	if !metav1.IsControlledBy(&pvc, guest) || *pvc.Spec.VolumeMode != corev1.PersistentVolumeBlock {
		t.Errorf("PVC must be guest-owned Block: %+v", pvc.Spec)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Value() != 113<<20 {
		t.Errorf("PVC size %s, want the image size rounded up to a MiB (113Mi)", got.String())
	}

	var job batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: imageDataDiskJobName("bg", "seeded")}, &job); err != nil {
		t.Fatalf("copy Job not created: %v", err)
	}
	spec := job.Spec.Template.Spec
	var src, dst *corev1.Volume
	for i := range spec.Volumes {
		switch spec.Volumes[i].Name {
		case "src":
			src = &spec.Volumes[i]
		case "dst":
			dst = &spec.Volumes[i]
		}
	}
	if src == nil || src.PersistentVolumeClaim.ClaimName != "swiftimage-import-dimg" || !src.PersistentVolumeClaim.ReadOnly || !spec.Containers[0].VolumeMounts[0].ReadOnly {
		t.Errorf("the image PVC must be mounted read-only by the Job: %+v", src)
	}
	if dst == nil || dst.PersistentVolumeClaim.ClaimName != "bg-data-seeded" {
		t.Errorf("the Job must write the guest's PVC: %+v", dst)
	}
	if !strings.Contains(strings.Join(spec.Containers[0].Command, " "), "qemu-img convert -f raw -O raw -n /src/image.raw /dev/dst") {
		t.Errorf("copy command: %v", spec.Containers[0].Command)
	}

	job.Status.Succeeded = 1
	if err := c.Status().Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureImageDataDisks(ctx, guest, rg); err != nil {
		t.Fatalf("after the copy completed: %v", err)
	}
}

// A running guest whose launcher pod still attaches the image's PVC (created
// before the fix) is left alone: no copy of an image it is writing.
func TestEnsureImageDataDisks_LegacyLauncherLeftAlone(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := blankGuest(swiftv1alpha1.DataDiskRef{Name: "seeded", ImageRef: &corev1.LocalObjectReference{Name: "dimg"}})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: guest.Name, Namespace: "default", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)}},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data-disk-seeded", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "swiftimage-import-dimg"}}}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	if err := r.EnsureImageDataDisks(context.Background(), guest, imageDataDiskRG(guest.Name)); err != nil {
		t.Fatalf("legacy launcher: %v", err)
	}
	var pvcs corev1.PersistentVolumeClaimList
	_ = c.List(context.Background(), &pvcs)
	if len(pvcs.Items) != 0 {
		t.Errorf("a copy was made while the legacy launcher writes the image: %d PVCs", len(pvcs.Items))
	}
}

// A PVC at the copy's name that the guest does not own is never adopted.
func TestEnsureImageDataDisks_ForeignPVCRefused(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := blankGuest(swiftv1alpha1.DataDiskRef{Name: "seeded", ImageRef: &corev1.LocalObjectReference{Name: "dimg"}})
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "bg-data-seeded", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}
	if err := r.EnsureImageDataDisks(context.Background(), guest, imageDataDiskRG(guest.Name)); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("want a refusal of the foreign PVC, got %v", err)
	}
}

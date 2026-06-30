package swiftimage

import (
	"context"
	"strings"
	"testing"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	gvImage := schema.GroupVersion{Group: "image.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvImage, &imagev1alpha1.SwiftImage{}, &imagev1alpha1.SwiftImageList{})
	return s
}

func TestStartImport_UploadReturnsPendingWithError(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme}
	img := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format: imagev1alpha1.DiskFormatRaw,
			Source: imagev1alpha1.ImageSource{Upload: &imagev1alpha1.UploadSource{}},
		},
	}
	result, err := r.StartImport(context.Background(), img)
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if result.Phase != imagev1alpha1.SwiftImagePhasePending {
		t.Errorf("phase = %s, want Pending", result.Phase)
	}
	if result.Error != ReasonUploadNotImpl {
		t.Errorf("error = %q, want %q", result.Error, ReasonUploadNotImpl)
	}
}

func TestStartImport_NoSourceReturnsFailed(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme}
	img := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format: imagev1alpha1.DiskFormatRaw,
			Source: imagev1alpha1.ImageSource{},
		},
	}
	result, err := r.StartImport(context.Background(), img)
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if result.Phase != imagev1alpha1.SwiftImagePhaseFailed {
		t.Errorf("phase = %s, want Failed", result.Phase)
	}
}

func TestStartImport_HTTPSourceCreatesJobAndPVC(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme}
	img := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format: imagev1alpha1.DiskFormatRaw,
			Source: imagev1alpha1.ImageSource{
				HTTP: &imagev1alpha1.HTTPSource{URL: "https://example.com/image.raw"},
			},
		},
	}
	result, err := r.StartImport(context.Background(), img)
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if result.Phase != imagev1alpha1.SwiftImagePhaseImporting {
		t.Errorf("phase = %s, want Importing", result.Phase)
	}
	if result.PVCRef == nil || result.PVCRef.Name != importPVCNamePrefix+"test" {
		t.Errorf("pvcRef = %+v, want name %s", result.PVCRef, importPVCNamePrefix+"test")
	}
	// Verify PVC and Job were created (default 10Gi when no rootDisk.size)
	var pvc corev1.PersistentVolumeClaim
	if err := client.Get(context.Background(), types.NamespacedName{Name: importPVCNamePrefix + "test", Namespace: "default"}, &pvc); err != nil {
		t.Errorf("PVC not created: %v", err)
	}
	if req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; req.String() != "10Gi" {
		t.Errorf("PVC storage default = %s, want 10Gi", req.String())
	}
	var job batchv1.Job
	if err := client.Get(context.Background(), types.NamespacedName{Name: importJobNamePrefix + "test", Namespace: "default"}, &job); err != nil {
		t.Errorf("Job not created: %v", err)
	}
}

func TestStartImport_HTTPSourceUsesRootDiskSize(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme}
	img := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format: imagev1alpha1.DiskFormatRaw,
			Source: imagev1alpha1.ImageSource{
				HTTP: &imagev1alpha1.HTTPSource{URL: "https://example.com/image.raw"},
			},
			RootDisk: &imagev1alpha1.SwiftImageRootDiskSpec{
				Size: ptr.To(resource.MustParse("40Gi")),
			},
		},
	}
	result, err := r.StartImport(context.Background(), img)
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if result.Phase != imagev1alpha1.SwiftImagePhaseImporting {
		t.Errorf("phase = %s, want Importing", result.Phase)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := client.Get(context.Background(), types.NamespacedName{Name: importPVCNamePrefix + "test", Namespace: "default"}, &pvc); err != nil {
		t.Fatalf("PVC not created: %v", err)
	}
	if req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; req.String() != "40Gi" {
		t.Errorf("PVC storage = %s, want 40Gi", req.String())
	}
}

func TestStartImport_HTTPSourceScriptPatchesGRUBForUEFI(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme}
	img := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format: imagev1alpha1.DiskFormatQcow2,
			Source: imagev1alpha1.ImageSource{
				HTTP: &imagev1alpha1.HTTPSource{URL: "https://example.com/ubuntu.img"},
			},
		},
	}
	result, err := r.StartImport(context.Background(), img)
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if result.Phase != imagev1alpha1.SwiftImagePhaseImporting {
		t.Fatalf("phase = %s, want Importing", result.Phase)
	}
	var job batchv1.Job
	if err := client.Get(context.Background(), types.NamespacedName{Name: importJobNamePrefix + "test", Namespace: "default"}, &job); err != nil {
		t.Fatalf("Job not created: %v", err)
	}
	cmd := job.Spec.Template.Spec.Containers[0].Command
	if len(cmd) < 3 {
		t.Fatalf("job command expected sh -c script, got %v", cmd)
	}
	script := cmd[2] // sh -c "<script>"
	// Verify GRUB patch covers UEFI (ESP + root), Rocky/RHEL (grub.conf), serial console
	for _, want := range []string{"patch_grub", "find", "grub.cfg", "grub.conf", "console=ttyS0,115200n8", "104857600", "1048576", "Linux LVM"} {
		if !strings.Contains(script, want) {
			t.Errorf("import script missing %q", want)
		}
	}
	// Linux import is privileged (the GRUB patch needs a loop-mount).
	if sc := job.Spec.Template.Spec.Containers[0].SecurityContext; sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Errorf("linux import Job should be privileged, got %+v", sc)
	}
}

func TestStartImport_WindowsSkipsGRUBPatchAndPrivileged(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme}
	img := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "win", Namespace: "default"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format: imagev1alpha1.DiskFormatQcow2,
			OSType: imagev1alpha1.OSTypeWindows,
			Source: imagev1alpha1.ImageSource{
				HTTP: &imagev1alpha1.HTTPSource{URL: "https://example.com/windows.qcow2"},
			},
		},
	}
	if _, err := r.StartImport(context.Background(), img); err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	var job batchv1.Job
	if err := client.Get(context.Background(), types.NamespacedName{Name: importJobNamePrefix + "win", Namespace: "default"}, &job); err != nil {
		t.Fatalf("Job not created: %v", err)
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	// The Linux-only GRUB/serial patch and its loop-mount must be absent.
	for _, unwanted := range []string{"patch_grub", "console=ttyS0", "mount -o loop"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("windows import script should not contain %q", unwanted)
		}
	}
	// OS-agnostic steps stay: qcow2->raw convert + size measurement.
	for _, want := range []string{"qemu-img convert", ".size"} {
		if !strings.Contains(script, want) {
			t.Errorf("windows import script should still contain %q", want)
		}
	}
	// Privileged is dropped for windows (no loop-mount needed).
	if sc := job.Spec.Template.Spec.Containers[0].SecurityContext; sc == nil || sc.Privileged == nil || *sc.Privileged {
		t.Errorf("windows import Job should be non-privileged, got %+v", sc)
	}
}

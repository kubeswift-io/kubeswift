package swiftimage

import (
	"context"
	"testing"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
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

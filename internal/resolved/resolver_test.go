package resolved

import (
	"context"
	"errors"
	"strings"
	"testing"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	kernelv1alpha1 "github.com/projectbeskar/kubeswift/api/kernel/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{}, &swiftv1alpha1.SwiftGuestClass{}, &swiftv1alpha1.SwiftGuestClassList{})
	gvImage := schema.GroupVersion{Group: "image.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvImage, &imagev1alpha1.SwiftImage{}, &imagev1alpha1.SwiftImageList{})
	gvSeed := schema.GroupVersion{Group: "seed.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSeed, &seedv1alpha1.SwiftSeedProfile{}, &seedv1alpha1.SwiftSeedProfileList{})
	gvKernel := schema.GroupVersion{Group: "kernel.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvKernel, &kernelv1alpha1.SwiftKernel{}, &kernelv1alpha1.SwiftKernelList{})
	return s
}

func TestResolve_FailsWhenSwiftImageDoesNotExist(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	// No SwiftImage in client
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "missing"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if rg != nil {
		t.Fatal("expected nil ResolvedGuest")
	}
	var re *ResolutionError
	if !errors.As(err, &re) {
		t.Fatalf("expected ResolutionError, got %T: %v", err, err)
	}
	if re.Reason == "" {
		t.Error("ResolutionError.Reason must be non-empty")
	}
	if re.AffectedResource != "missing" {
		t.Errorf("AffectedResource = %q, want missing", re.AffectedResource)
	}
}

func TestResolve_OSTypeMismatchRejected(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	// Linux image, but the guest declares windows -> mismatch (the cross-check
	// fires before the heavier existence/compat validation).
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Spec:       imagev1alpha1.SwiftImageSpec{OSType: imagev1alpha1.OSTypeLinux},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}, OSType: swiftv1alpha1.OSTypeWindows},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if rg != nil {
		t.Fatal("expected nil ResolvedGuest on osType mismatch")
	}
	var re *ResolutionError
	if !errors.As(err, &re) {
		t.Fatalf("expected ResolutionError, got %T: %v", err, err)
	}
	if !strings.Contains(re.Reason, "osType mismatch") {
		t.Errorf("Reason = %q, want osType mismatch", re.Reason)
	}
}

func TestResolve_FailsWhenSwiftImageNotReady(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhasePending},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if rg != nil {
		t.Fatal("expected nil ResolvedGuest")
	}
	var re *ResolutionError
	if !errors.As(err, &re) {
		t.Fatalf("expected ResolutionError, got %T: %v", err, err)
	}
	if re.Reason == "" {
		t.Error("ResolutionError.Reason must be non-empty")
	}
}

func TestResolve_FailsWhenSwiftGuestClassDoesNotExist(t *testing.T) {
	scheme := testScheme()
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}},
	}
	// No SwiftGuestClass in client
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(image).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "missing"}},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if rg != nil {
		t.Fatal("expected nil ResolvedGuest")
	}
	var re *ResolutionError
	if !errors.As(err, &re) {
		t.Fatalf("expected ResolutionError, got %T: %v", err, err)
	}
	if re.AffectedResource != "missing" {
		t.Errorf("AffectedResource = %q, want missing", re.AffectedResource)
	}
}

func TestResolve_FailsWhenSwiftSeedProfileDoesNotExistWhenReferenced(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}},
	}
	// No SwiftSeedProfile in client
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "gc"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "missing-sp"},
		},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if rg != nil {
		t.Fatal("expected nil ResolvedGuest")
	}
	var re *ResolutionError
	if !errors.As(err, &re) {
		t.Fatalf("expected ResolutionError, got %T: %v", err, err)
	}
	if re.AffectedResource != "missing-sp" {
		t.Errorf("AffectedResource = %q, want missing-sp", re.AffectedResource)
	}
}

func TestResolve_ResolutionErrorIncludesReasonString(t *testing.T) {
	re := &ResolutionError{Reason: "SwiftImage not Ready", AffectedResource: "img"}
	if re.Error() != "SwiftImage not Ready" {
		t.Errorf("Error() = %q, want %q", re.Error(), "SwiftImage not Ready")
	}
}

func TestResolve_SucceedsWhenAllChecksPass(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", UID: "uid-123"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rg == nil {
		t.Fatal("expected non-nil ResolvedGuest")
	}
	if rg.Meta.Name != "g" || rg.Meta.Namespace != "ns" {
		t.Errorf("Meta = %+v, want name=g namespace=ns", rg.Meta)
	}
	if rg.Resources.CPU != 2 {
		t.Errorf("Resources.CPU = %d, want 2", rg.Resources.CPU)
	}
	if rg.PreparedImage.Ready != true {
		t.Error("PreparedImage.Ready should be true")
	}
}

// --- dataDiskRef tests ---

func TestResolve_DataDiskRef_Success(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw, PVCRef: &imagev1alpha1.PVCObjectReference{Name: "pvc-root"}}},
	}
	dataDisk := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "data-img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw, PVCRef: &imagev1alpha1.PVCObjectReference{Name: "pvc-data"}}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image, dataDisk).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", UID: "uid-dd"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "gc"},
			DataDiskRef:   &corev1.LocalObjectReference{Name: "data-img"},
		},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(rg.DataDisks) != 1 {
		t.Fatalf("DataDisks should have 1 entry, got %d", len(rg.DataDisks))
	}
	if !rg.DataDisks[0].Ready {
		t.Error("DataDisks[0].Ready should be true")
	}
	if rg.DataDisks[0].PVCName != "pvc-data" {
		t.Errorf("DataDisks[0].PVCName = %q, want pvc-data", rg.DataDisks[0].PVCName)
	}
}

func TestResolve_DataDiskRef_Missing(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "gc"},
			DataDiskRef:   &corev1.LocalObjectReference{Name: "missing-data"},
		},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if rg != nil {
		t.Fatal("expected nil ResolvedGuest")
	}
	var re *ResolutionError
	if !errors.As(err, &re) {
		t.Fatalf("expected ResolutionError, got %T: %v", err, err)
	}
	if re.AffectedResource != "missing-data" {
		t.Errorf("AffectedResource = %q, want missing-data", re.AffectedResource)
	}
}

func TestResolve_DataDiskRef_NotReady(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}},
	}
	dataDisk := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "data-img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseImporting},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image, dataDisk).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "gc"},
			DataDiskRef:   &corev1.LocalObjectReference{Name: "data-img"},
		},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if rg != nil {
		t.Fatal("expected nil ResolvedGuest when data disk not Ready")
	}
	var re *ResolutionError
	if !errors.As(err, &re) {
		t.Fatalf("expected ResolutionError, got %T: %v", err, err)
	}
}

func TestResolve_NoDataDiskRef_BackwardCompat(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", UID: "uid-bc"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "gc"},
		},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(rg.DataDisks) != 0 {
		t.Error("DataDisks should be empty when dataDiskRef is not set")
	}
}

// --- plural dataDiskRefs tests (blank / imageRef / attachAsDisk) ---

func ddGuest(refs ...swiftv1alpha1.DataDiskRef) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "gc"},
			DataDiskRefs:  refs,
		},
	}
}

func ddBaseObjects() (*swiftv1alpha1.SwiftGuestClass, *imagev1alpha1.SwiftImage) {
	gc := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}},
	}
	img := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw, PVCRef: &imagev1alpha1.PVCObjectReference{Name: "pvc-root"}}},
	}
	return gc, img
}

func TestResolve_BlankDataDisk_Block(t *testing.T) {
	gc, img := ddBaseObjects()
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gc, img).Build()
	guest := ddGuest(swiftv1alpha1.DataDiskRef{Name: "db", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("100Gi")}})

	rg, err := NewResolver(client).Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(rg.DataDisks) != 1 {
		t.Fatalf("want 1 data disk, got %d", len(rg.DataDisks))
	}
	d := rg.DataDisks[0]
	if !d.Block {
		t.Error("blank default should be Block")
	}
	if d.PVCName != "g-data-db" {
		t.Errorf("PVCName = %q, want g-data-db", d.PVCName)
	}
	if d.HostPath != "/dev/kubeswift-data-db" {
		t.Errorf("HostPath = %q, want /dev/kubeswift-data-db", d.HostPath)
	}
	if d.MountPath != "" {
		t.Errorf("Block disk MountPath should be empty, got %q", d.MountPath)
	}
}

func TestResolve_BlankDataDisk_Filesystem(t *testing.T) {
	gc, img := ddBaseObjects()
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gc, img).Build()
	guest := ddGuest(swiftv1alpha1.DataDiskRef{Name: "fs", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("10Gi"), VolumeMode: corev1.PersistentVolumeFilesystem}})

	rg, err := NewResolver(client).Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	d := rg.DataDisks[0]
	if d.Block {
		t.Error("Filesystem blank should not be Block")
	}
	if d.MountPath != "/var/lib/kubeswift/disks/data-fs" {
		t.Errorf("MountPath = %q, want /var/lib/kubeswift/disks/data-fs", d.MountPath)
	}
	if d.HostPath != "/var/lib/kubeswift/disks/data-fs/image.raw" {
		t.Errorf("HostPath = %q, want .../data-fs/image.raw", d.HostPath)
	}
}

func TestResolve_PluralImageDataDisk(t *testing.T) {
	gc, img := ddBaseObjects()
	dataImg := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "data-img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw, PVCRef: &imagev1alpha1.PVCObjectReference{Name: "pvc-dimg"}}},
	}
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gc, img, dataImg).Build()
	guest := ddGuest(swiftv1alpha1.DataDiskRef{Name: "extra", ImageRef: &corev1.LocalObjectReference{Name: "data-img"}})

	rg, err := NewResolver(client).Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	d := rg.DataDisks[0]
	if d.Block {
		t.Error("image-backed data disk should be Filesystem")
	}
	if d.PVCName != "pvc-dimg" {
		t.Errorf("PVCName = %q, want pvc-dimg", d.PVCName)
	}
	if d.MountPath != "/var/lib/kubeswift/disks/data-extra" {
		t.Errorf("MountPath = %q, want .../data-extra", d.MountPath)
	}
}

func TestResolve_AttachPVCAsDisk_Block(t *testing.T) {
	gc, img := ddBaseObjects()
	blockMode := corev1.PersistentVolumeBlock
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "myblk", Namespace: "ns"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &blockMode},
	}
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gc, img, pvc).Build()
	guest := ddGuest(swiftv1alpha1.DataDiskRef{Name: "att", PVCRef: &corev1.LocalObjectReference{Name: "myblk"}, AttachAsDisk: true})

	rg, err := NewResolver(client).Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	d := rg.DataDisks[0]
	if !d.Block {
		t.Error("attachAsDisk Block PVC should be Block")
	}
	if d.PVCName != "myblk" {
		t.Errorf("PVCName = %q, want myblk", d.PVCName)
	}
	if d.HostPath != "/dev/kubeswift-data-att" {
		t.Errorf("HostPath = %q, want /dev/kubeswift-data-att", d.HostPath)
	}
}

func TestResolve_AttachPVCAsDisk_FilesystemRejected(t *testing.T) {
	gc, img := ddBaseObjects()
	fsMode := corev1.PersistentVolumeFilesystem
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "myfs", Namespace: "ns"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &fsMode},
	}
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gc, img, pvc).Build()
	guest := ddGuest(swiftv1alpha1.DataDiskRef{Name: "att", PVCRef: &corev1.LocalObjectReference{Name: "myfs"}, AttachAsDisk: true})

	_, err := NewResolver(client).Resolve(context.Background(), guest)
	var re *ResolutionError
	if !errors.As(err, &re) {
		t.Fatalf("expected ResolutionError for Filesystem attachAsDisk, got %T: %v", err, err)
	}
}

func TestResolve_PlainPVCRef_NotAVMDisk(t *testing.T) {
	gc, img := ddBaseObjects()
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gc, img).Build()
	// pvcRef WITHOUT attachAsDisk: a SwiftGuestPool filesystem mount, not a VM
	// disk — it must NOT appear in rg.DataDisks (pod.go::applyDataDiskRefs owns it).
	guest := ddGuest(swiftv1alpha1.DataDiskRef{Name: "pool", PVCRef: &corev1.LocalObjectReference{Name: "pool-pvc"}})

	rg, err := NewResolver(client).Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(rg.DataDisks) != 0 {
		t.Errorf("plain pvcRef should not be a VM disk; got %d data disks", len(rg.DataDisks))
	}
}

// TestResolve_FailsWhenArchitectureMismatch: skipped - architecture validation not in MVP (API has no architecture field yet).
func TestResolve_FailsWhenArchitectureMismatch(t *testing.T) {
	t.Skip("architecture validation not implemented in MVP")
}

// TestResolve_FailsWhenMemoryHotplugMaxGuestLessThanGuestMemory: skipped - memory hotplug validation not in MVP (API has no hotplug field yet).
func TestResolve_FailsWhenMemoryHotplugMaxGuestLessThanGuestMemory(t *testing.T) {
	t.Skip("memory hotplug validation not implemented in MVP")
}

func TestResolve_FailsWhenRootDiskFormatIncompatibleWithImageFormat(t *testing.T) {
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatQcow2}},
	}
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image).Build()
	resolver := NewResolver(client)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}},
	}

	rg, err := resolver.Resolve(context.Background(), guest)
	if rg != nil {
		t.Fatal("expected nil ResolvedGuest")
	}
	var re *ResolutionError
	if !errors.As(err, &re) {
		t.Fatalf("expected ResolutionError, got %T: %v", err, err)
	}
	if re.Reason == "" {
		t.Error("ResolutionError.Reason must be non-empty")
	}
}

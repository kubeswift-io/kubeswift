package swiftimage

import (
	"context"
	"testing"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func finalizerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	if err := volumesnapshotv1.AddToScheme(s); err != nil {
		t.Fatalf("volumesnapshotv1: %v", err)
	}
	gvImg := schema.GroupVersion{Group: "image.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvImg, &imagev1alpha1.SwiftImage{}, &imagev1alpha1.SwiftImageList{})
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(s, gvImg)
	metav1.AddToGroupVersion(s, gvSwift)
	return s
}

func makeImage(name, ns string, strategy imagev1alpha1.CloneStrategy) *imagev1alpha1.SwiftImage {
	return &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format:        imagev1alpha1.DiskFormatRaw,
			CloneStrategy: strategy,
		},
	}
}

func TestEnsureCloneSeedFinalizers_CopyStrategy_NoOp(t *testing.T) {
	scheme := finalizerScheme(t)
	img := makeImage("img1", "default", imagev1alpha1.CloneStrategyCopy)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img).Build()
	r := &SwiftImageReconciler{Client: c, Scheme: scheme}

	if err := r.EnsureCloneSeedFinalizers(context.Background(), img); err != nil {
		t.Fatalf("err: %v", err)
	}
	if controllerutil.ContainsFinalizer(img, CloneSeedFinalizer) {
		t.Errorf("copy strategy must not add the clone-seed finalizer")
	}
}

func TestEnsureCloneSeedFinalizers_SnapshotStrategy_AddsFinalizerToImageOnly(t *testing.T) {
	scheme := finalizerScheme(t)
	img := makeImage("img1", "default", imagev1alpha1.CloneStrategySnapshot)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img).Build()
	r := &SwiftImageReconciler{Client: c, Scheme: scheme}

	if err := r.EnsureCloneSeedFinalizers(context.Background(), img); err != nil {
		t.Fatalf("err: %v", err)
	}
	var got imagev1alpha1.SwiftImage
	if err := c.Get(context.Background(), client.ObjectKey{Name: "img1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, CloneSeedFinalizer) {
		t.Errorf("snapshot strategy must add the clone-seed finalizer to SwiftImage")
	}
}

func TestEnsureCloneSeedFinalizers_AddsFinalizerToSnapshotWhenPresent(t *testing.T) {
	scheme := finalizerScheme(t)
	img := makeImage("img1", "default", imagev1alpha1.CloneStrategySnapshot)
	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: CloneSeedSnapshotName("img1"), Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img, snap).Build()
	r := &SwiftImageReconciler{Client: c, Scheme: scheme}

	if err := r.EnsureCloneSeedFinalizers(context.Background(), img); err != nil {
		t.Fatalf("err: %v", err)
	}
	var got volumesnapshotv1.VolumeSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: snap.Name, Namespace: snap.Namespace}, &got); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, CloneSeedFinalizer) {
		t.Errorf("clone-seed snapshot must carry CloneSeedFinalizer")
	}
}

func TestHandleCloneSeedDeletion_BlockedByDependentGuest(t *testing.T) {
	scheme := finalizerScheme(t)
	img := makeImage("img1", "default", imagev1alpha1.CloneStrategySnapshot)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef: &corev1.LocalObjectReference{Name: "img1"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img, guest).Build()
	r := &SwiftImageReconciler{Client: c, Scheme: scheme}

	canRemove, blocking, err := r.HandleCloneSeedDeletion(context.Background(), img)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if canRemove {
		t.Errorf("canRemove must be false while dependent guests exist")
	}
	if len(blocking) != 1 || blocking[0] != "g1" {
		t.Errorf("blocking = %v, want [g1]", blocking)
	}
}

func TestHandleCloneSeedDeletion_NoDependents_RemovesSnapshotFinalizer(t *testing.T) {
	scheme := finalizerScheme(t)
	img := makeImage("img1", "default", imagev1alpha1.CloneStrategySnapshot)
	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:       CloneSeedSnapshotName("img1"),
			Namespace:  "default",
			Finalizers: []string{CloneSeedFinalizer},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img, snap).Build()
	r := &SwiftImageReconciler{Client: c, Scheme: scheme}

	canRemove, blocking, err := r.HandleCloneSeedDeletion(context.Background(), img)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !canRemove {
		t.Errorf("canRemove must be true with no dependent guests")
	}
	if len(blocking) != 0 {
		t.Errorf("blocking = %v, want empty", blocking)
	}
	var got volumesnapshotv1.VolumeSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: snap.Name, Namespace: snap.Namespace}, &got); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if controllerutil.ContainsFinalizer(&got, CloneSeedFinalizer) {
		t.Errorf("clone-seed snapshot finalizer must be removed once safe")
	}
}

func TestHandleCloneSeedDeletion_GuestInOtherNamespace_NotBlocking(t *testing.T) {
	// Same-namespace constraint: guests in OTHER namespaces must not block
	// deletion of a SwiftImage. ListGuestsReferencingImage scopes to the
	// SwiftImage's namespace by design (Phase 0 §6a).
	scheme := finalizerScheme(t)
	img := makeImage("img1", "default", imagev1alpha1.CloneStrategySnapshot)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "other"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef: &corev1.LocalObjectReference{Name: "img1"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img, guest).Build()
	r := &SwiftImageReconciler{Client: c, Scheme: scheme}

	canRemove, blocking, err := r.HandleCloneSeedDeletion(context.Background(), img)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !canRemove {
		t.Errorf("canRemove must be true — guest is in a different namespace")
	}
	if len(blocking) != 0 {
		t.Errorf("blocking = %v, want empty (cross-namespace guest must not block)", blocking)
	}
}

package swiftmigration

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func TestResolveAutoMode_NoVFIO_AllowIPChange_ResolvesToLive(t *testing.T) {
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{GuestClassRef: corev1.LocalObjectReference{Name: "class-default"}},
	}
	mig := newMigration("m", "default")
	mig.Spec.AllowIPChange = true

	// newGuestClass carries live-capable (RWX+Block) storage.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newGuestClass("class-default", 2, 2048)).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	if res := r.resolveAutoMode(context.Background(), mig, &mig.Status); res != nil {
		t.Fatalf("expected nil result; got %+v", res)
	}
	if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeLive {
		t.Errorf("status.Mode: want live, got %q", mig.Status.Mode)
	}
}

// C4: a disk-boot guest on storage that cannot be live-migrated (the default
// RWO/Filesystem) must resolve auto to OFFLINE. It used to resolve live, so a
// drain migration's destination pod hit Multi-Attach and failed DstNeverReady,
// blocking the drain instead of taking the documented offline path.
func TestResolveAutoMode_NonLiveCapableStorage_ResolvesToOffline(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *swiftv1alpha1.StorageSpec
	}{
		{"default RWO/Filesystem", nil},
		{"RWX Filesystem", &swiftv1alpha1.StorageSpec{AccessMode: corev1.ReadWriteMany, VolumeMode: corev1.PersistentVolumeFilesystem}},
		{"RWO Block", &swiftv1alpha1.StorageSpec{AccessMode: corev1.ReadWriteOnce, VolumeMode: corev1.PersistentVolumeBlock}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := testScheme(t)
			guest := &swiftv1alpha1.SwiftGuest{
				ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
				Spec: swiftv1alpha1.SwiftGuestSpec{
					ImageRef:      &corev1.LocalObjectReference{Name: "img"},
					GuestClassRef: corev1.LocalObjectReference{Name: "c"},
				},
			}
			class := newGuestClass("c", 2, 2048)
			class.Spec.Storage = tc.st
			mig := newMigration("m", "default")
			mig.Spec.AllowIPChange = true

			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
			r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}
			if res := r.resolveAutoMode(context.Background(), mig, &mig.Status); res != nil {
				t.Fatalf("expected nil result; got %+v", res)
			}
			if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeOffline {
				t.Errorf("status.Mode: want offline for non-live-capable storage, got %q", mig.Status.Mode)
			}
		})
	}
}

// A kernel-boot guest has no root-disk PVC, so it stays live-eligible under
// auto regardless of the class's storage defaults.
func TestResolveAutoMode_KernelBoot_ResolvesToLive(t *testing.T) {
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{KernelRef: &corev1.LocalObjectReference{Name: "k"}},
	}
	mig := newMigration("m", "default")
	mig.Spec.AllowIPChange = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}
	if res := r.resolveAutoMode(context.Background(), mig, &mig.Status); res != nil {
		t.Fatalf("expected nil result; got %+v", res)
	}
	if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeLive {
		t.Errorf("status.Mode: want live for kernel-boot, got %q", mig.Status.Mode)
	}
}

func TestResolveAutoMode_VFIO_ResolvesToOffline(t *testing.T) {
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			GPUProfileRef: &corev1.LocalObjectReference{Name: "gpu-profile"},
		},
	}
	mig := newMigration("m", "default")
	mig.Spec.AllowIPChange = true

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	if res := r.resolveAutoMode(context.Background(), mig, &mig.Status); res != nil {
		t.Fatalf("expected nil result; got %+v", res)
	}
	if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeOffline {
		t.Errorf("VFIO present must resolve auto→offline; got %q", mig.Status.Mode)
	}
}

// A guest that would otherwise resolve to live (no VFIO, allowIPChange) but rides its
// namespace's primary OVN-K UDN (Model A) MUST resolve to offline — live migration of a
// primary-UDN guest is unsupported in v1 (no swiftletd<->swiftletd channel).
func TestResolveAutoMode_ModelA_ResolvesToOffline(t *testing.T) {
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "model-a"},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "model-a",
		Labels: map[string]string{"k8s.ovn.org/primary-user-defined-network": ""},
	}}
	mig := newMigration("m", "model-a")
	mig.Spec.AllowIPChange = true

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, ns).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	if res := r.resolveAutoMode(context.Background(), mig, &mig.Status); res != nil {
		t.Fatalf("expected nil result; got %+v", res)
	}
	if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeOffline {
		t.Errorf("Model A must resolve auto→offline; got %q", mig.Status.Mode)
	}
}

func TestResolveAutoMode_DefaultNetworking_NoAllowIPChange_ResolvesToOffline(t *testing.T) {
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		// No Interfaces set → default node-local networking.
	}
	mig := newMigration("m", "default")
	mig.Spec.AllowIPChange = false

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	if res := r.resolveAutoMode(context.Background(), mig, &mig.Status); res != nil {
		t.Fatalf("expected nil result; got %+v", res)
	}
	if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeOffline {
		t.Errorf("default-networking + !allowIPChange must resolve to offline; got %q", mig.Status.Mode)
	}
}

func TestResolveAutoMode_GuestNotFound_ReturnsFailure(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigration("m", "default")
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	res := r.resolveAutoMode(context.Background(), mig, &mig.Status)
	if res == nil || res.FailureMsg == "" {
		t.Fatalf("expected phaseFailure; got %+v", res)
	}
}

func TestHasVFIODevices_GPUProfile_True(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			GPUProfileRef: &corev1.LocalObjectReference{Name: "g"},
		},
	}
	if !hasVFIODevices(guest) {
		t.Errorf("gpuProfileRef set must yield true")
	}
}

func TestHasVFIODevices_SRIOVInterface_True(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "data", Type: swiftv1alpha1.InterfaceTypeSRIOV},
			},
		},
	}
	if !hasVFIODevices(guest) {
		t.Errorf("SR-IOV interface must yield true")
	}
}

func TestHasVFIODevices_None_False(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{}
	if hasVFIODevices(guest) {
		t.Errorf("no VFIO devices must yield false")
	}
}

func TestResolveAutoMode_NodeLocalVirtioBackends_ResolvesToOffline(t *testing.T) {
	// virtiofs / vhost-user backends are node-local; auto must resolve to
	// offline (mirrors the VFIO rule). AllowIPChange=true so the networking
	// branch cannot be what forces offline — the backend rule must.
	scheme := testScheme(t)
	hp := "/srv/share"
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Filesystems: []swiftv1alpha1.Filesystem{
				{Name: "data", Source: swiftv1alpha1.FilesystemSource{HostPath: &hp}},
			},
		},
	}
	mig := newMigration("m", "default")
	mig.Spec.AllowIPChange = true

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	if res := r.resolveAutoMode(context.Background(), mig, &mig.Status); res != nil {
		t.Fatalf("expected nil result; got %+v", res)
	}
	if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeOffline {
		t.Errorf("virtiofs guest must resolve auto→offline; got %q", mig.Status.Mode)
	}
}

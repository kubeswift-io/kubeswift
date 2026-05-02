package swiftmigration

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func TestResolveAutoMode_NoVFIO_AllowIPChange_ResolvesToLive(t *testing.T) {
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
	}
	mig := newMigration("m", "default")
	mig.Spec.AllowIPChange = true

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	if res := r.resolveAutoMode(context.Background(), mig, &mig.Status); res != nil {
		t.Fatalf("expected nil result; got %+v", res)
	}
	if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeLive {
		t.Errorf("status.Mode: want live, got %q", mig.Status.Mode)
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

package swiftguest

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func guest(mut func(*swiftv1alpha1.SwiftGuest)) *swiftv1alpha1.SwiftGuest {
	g := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "cls"},
		},
	}
	if mut != nil {
		mut(g)
	}
	return g
}

func errContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want containing %q", err, want)
	}
}

func TestValidate_BootSourceExclusivity(t *testing.T) {
	// imageRef alone: OK.
	if err := validateSwiftGuest(guest(nil)); err != nil {
		t.Errorf("imageRef-only should be valid: %v", err)
	}
	// kernelRef alone: OK.
	if err := validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.ImageRef = nil
		g.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	})); err != nil {
		t.Errorf("kernelRef-only should be valid: %v", err)
	}
	// cloneFromSnapshot (with guestClassRef, which the CRD requires): OK.
	if err := validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.ImageRef = nil
		g.Spec.CloneFromSnapshot = &swiftv1alpha1.CloneFromSnapshotSource{
			SnapshotRef: corev1.LocalObjectReference{Name: "snap"},
		}
	})); err != nil {
		t.Errorf("cloneFromSnapshot should be valid: %v", err)
	}
	// cloneFromSnapshot without guestClassRef: rejected (CRD requires it; webhook aligned).
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.ImageRef = nil
		g.Spec.GuestClassRef = corev1.LocalObjectReference{}
		g.Spec.CloneFromSnapshot = &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "snap"}}
	})), "spec.guestClassRef.name is required")
	// none set.
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) { g.Spec.ImageRef = nil })),
		"exactly one of spec.imageRef")
	// two set (imageRef + cloneFromSnapshot).
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.CloneFromSnapshot = &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "snap"}}
	})), "exactly one of spec.imageRef")
}

func TestValidate_OSType(t *testing.T) {
	// default (unset) osType + imageRef: OK (no behaviour change for existing guests).
	if err := validateSwiftGuest(guest(nil)); err != nil {
		t.Errorf("unset osType should be valid: %v", err)
	}
	// explicit linux + imageRef: OK.
	if err := validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.OSType = swiftv1alpha1.OSTypeLinux
	})); err != nil {
		t.Errorf("linux + imageRef should be valid: %v", err)
	}
	// windows + imageRef (disk boot): OK.
	if err := validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.OSType = swiftv1alpha1.OSTypeWindows
	})); err != nil {
		t.Errorf("windows + imageRef should be valid: %v", err)
	}
	// windows + kernelRef: rejected (Windows is disk-boot only).
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.ImageRef = nil
		g.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
		g.Spec.OSType = swiftv1alpha1.OSTypeWindows
	})), "windows requires disk boot")
	// windows + gpuProfileRef: rejected (GPU-to-Windows out of scope v1).
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.OSType = swiftv1alpha1.OSTypeWindows
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
	})), "gpuProfileRef is not supported")
	// invalid enum value: rejected (defense-in-depth beyond the CRD schema).
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.OSType = swiftv1alpha1.OSType("bsd")
	})), "spec.osType must be linux or windows")
}

func TestValidate_CloneFromSnapshotRules(t *testing.T) {
	cloneGuest := func(mut func(*swiftv1alpha1.SwiftGuest)) *swiftv1alpha1.SwiftGuest {
		return guest(func(g *swiftv1alpha1.SwiftGuest) {
			g.Spec.ImageRef = nil
			g.Spec.GuestClassRef = corev1.LocalObjectReference{}
			g.Spec.CloneFromSnapshot = &swiftv1alpha1.CloneFromSnapshotSource{
				SnapshotRef: corev1.LocalObjectReference{Name: "snap"},
			}
			if mut != nil {
				mut(g)
			}
		})
	}
	// missing snapshotRef.name.
	errContains(t, validateSwiftGuest(cloneGuest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.CloneFromSnapshot.SnapshotRef.Name = ""
	})), "snapshotRef.name is required")
	// gpuProfileRef + cloneFromSnapshot: rejected.
	errContains(t, validateSwiftGuest(cloneGuest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
	})), "mutually exclusive with spec.gpuProfileRef")
}

func TestValidate_GuestClassRequiredForImageBoot(t *testing.T) {
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GuestClassRef = corev1.LocalObjectReference{}
	})), "spec.guestClassRef.name is required")
}

func hostPathFS(name string, mut func(*swiftv1alpha1.Filesystem)) swiftv1alpha1.Filesystem {
	hp := "/srv/" + name
	fs := swiftv1alpha1.Filesystem{
		Name:   name,
		Source: swiftv1alpha1.FilesystemSource{HostPath: &hp},
	}
	if mut != nil {
		mut(&fs)
	}
	return fs
}

func TestValidate_Filesystems(t *testing.T) {
	// Valid: two distinct shares, one hostPath one PVC.
	g := guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Filesystems = []swiftv1alpha1.Filesystem{
			hostPathFS("data", nil),
			{Name: "pvc", Source: swiftv1alpha1.FilesystemSource{
				PVCRef: &corev1.LocalObjectReference{Name: "claim"}}},
		}
	})
	if err := validateSwiftGuest(g); err != nil {
		t.Fatalf("valid filesystems rejected: %v", err)
	}

	// Duplicate name.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Filesystems = []swiftv1alpha1.Filesystem{hostPathFS("dup", nil), hostPathFS("dup", nil)}
	})
	errContains(t, validateSwiftGuest(g), "is duplicated")

	// Duplicate effective tag (one explicit, one defaulted from name).
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Filesystems = []swiftv1alpha1.Filesystem{
			hostPathFS("a", func(f *swiftv1alpha1.Filesystem) { f.Tag = "shared" }),
			hostPathFS("shared", nil),
		}
	})
	errContains(t, validateSwiftGuest(g), "tag")

	// Both sources set.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Filesystems = []swiftv1alpha1.Filesystem{
			hostPathFS("both", func(f *swiftv1alpha1.Filesystem) {
				f.Source.PVCRef = &corev1.LocalObjectReference{Name: "claim"}
			}),
		}
	})
	errContains(t, validateSwiftGuest(g), "not both")

	// No source set.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Filesystems = []swiftv1alpha1.Filesystem{{Name: "empty"}}
	})
	errContains(t, validateSwiftGuest(g), "exactly one of hostPath or pvcRef")

	// Rejected with gpuProfileRef (CH-only in v1).
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
		g.Spec.Filesystems = []swiftv1alpha1.Filesystem{hostPathFS("data", nil)}
	})
	errContains(t, validateSwiftGuest(g), "gpuProfileRef")

	// Rejected with Windows (no virtio-fs guest driver in v1).
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.OSType = swiftv1alpha1.OSTypeWindows
		g.Spec.Filesystems = []swiftv1alpha1.Filesystem{hostPathFS("data", nil)}
	})
	errContains(t, validateSwiftGuest(g), "windows")
}

func TestValidate_VhostUserInterfaces(t *testing.T) {
	// Valid: a bridge primary + a vhost-user NIC with a socket.
	g := guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
			{Name: "mgmt"},
			{Name: "fast0", Type: swiftv1alpha1.InterfaceTypeVhostUser, Socket: "/var/run/vhost/fast0.sock"},
		}
	})
	if err := validateSwiftGuest(g); err != nil {
		t.Fatalf("valid vhost-user rejected: %v", err)
	}

	// Missing socket.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
			{Name: "fast0", Type: swiftv1alpha1.InterfaceTypeVhostUser},
		}
	})
	errContains(t, validateSwiftGuest(g), "requires a socket")

	// networkRef not allowed.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
			{Name: "fast0", Type: swiftv1alpha1.InterfaceTypeVhostUser,
				Socket: "/s.sock", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "nad"}},
		}
	})
	errContains(t, validateSwiftGuest(g), "networkRef")

	// resourceName not allowed.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
			{Name: "fast0", Type: swiftv1alpha1.InterfaceTypeVhostUser,
				Socket: "/s.sock", ResourceName: "intel.com/sriov"},
		}
	})
	errContains(t, validateSwiftGuest(g), "resourceName")

	// Rejected with gpuProfileRef (CH-only in v1).
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
		g.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
			{Name: "fast0", Type: swiftv1alpha1.InterfaceTypeVhostUser, Socket: "/s.sock"},
		}
	})
	errContains(t, validateSwiftGuest(g), "gpuProfileRef")
}

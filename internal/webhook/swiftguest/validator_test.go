package swiftguest

import (
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
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
	})), "GPU passthrough")
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
	})), "mutually exclusive with GPU passthrough")
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

func TestValidate_VhostUserDevices(t *testing.T) {
	// Valid: a blk + a generic with virtioId.
	g := guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.VhostUserDevices = []swiftv1alpha1.VhostUserDevice{
			{Name: "disk0", Type: swiftv1alpha1.VhostUserDeviceTypeBlk, Socket: "/run/spdk/0"},
			{Name: "gen0", Type: swiftv1alpha1.VhostUserDeviceTypeGeneric, Socket: "/run/x/g", VirtioID: "block"},
		}
	})
	if err := validateSwiftGuest(g); err != nil {
		t.Fatalf("valid vhost-user devices rejected: %v", err)
	}

	// Missing socket.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.VhostUserDevices = []swiftv1alpha1.VhostUserDevice{
			{Name: "d", Type: swiftv1alpha1.VhostUserDeviceTypeBlk},
		}
	})
	errContains(t, validateSwiftGuest(g), "socket is required")

	// Generic without virtioId.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.VhostUserDevices = []swiftv1alpha1.VhostUserDevice{
			{Name: "d", Type: swiftv1alpha1.VhostUserDeviceTypeGeneric, Socket: "/s"},
		}
	})
	errContains(t, validateSwiftGuest(g), "virtioId")

	// Bad type.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.VhostUserDevices = []swiftv1alpha1.VhostUserDevice{
			{Name: "d", Type: "weird", Socket: "/s"},
		}
	})
	errContains(t, validateSwiftGuest(g), "must be blk or generic")

	// Duplicate name.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.VhostUserDevices = []swiftv1alpha1.VhostUserDevice{
			{Name: "d", Type: swiftv1alpha1.VhostUserDeviceTypeBlk, Socket: "/a"},
			{Name: "d", Type: swiftv1alpha1.VhostUserDeviceTypeBlk, Socket: "/b"},
		}
	})
	errContains(t, validateSwiftGuest(g), "duplicated")

	// Rejected with gpuProfileRef.
	g = guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
		g.Spec.VhostUserDevices = []swiftv1alpha1.VhostUserDevice{
			{Name: "d", Type: swiftv1alpha1.VhostUserDeviceTypeBlk, Socket: "/s"},
		}
	})
	errContains(t, validateSwiftGuest(g), "gpuProfileRef")
}

func TestValidate_DataDisks(t *testing.T) {
	gi := func(refs ...swiftv1alpha1.DataDiskRef) *swiftv1alpha1.SwiftGuest {
		return guest(func(g *swiftv1alpha1.SwiftGuest) { g.Spec.DataDiskRefs = refs })
	}

	// Valid: a blank Block disk (default mode).
	if err := validateSwiftGuest(gi(swiftv1alpha1.DataDiskRef{
		Name:  "db",
		Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("100Gi")},
	})); err != nil {
		t.Errorf("blank Block disk should be valid: %v", err)
	}

	// Valid: blank with explicit Filesystem mode.
	if err := validateSwiftGuest(gi(swiftv1alpha1.DataDiskRef{
		Name:  "fs",
		Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("10Gi"), VolumeMode: corev1.PersistentVolumeFilesystem},
	})); err != nil {
		t.Errorf("blank Filesystem disk should be valid: %v", err)
	}

	// Valid: image-backed and an attached-as-disk PVC and a plain fs PVC.
	if err := validateSwiftGuest(gi(
		swiftv1alpha1.DataDiskRef{Name: "img", ImageRef: &corev1.LocalObjectReference{Name: "i"}},
		swiftv1alpha1.DataDiskRef{Name: "vmpvc", PVCRef: &corev1.LocalObjectReference{Name: "p"}, AttachAsDisk: true},
		swiftv1alpha1.DataDiskRef{Name: "fspvc", PVCRef: &corev1.LocalObjectReference{Name: "q"}},
	)); err != nil {
		t.Errorf("mixed image/pvc disks should be valid: %v", err)
	}

	// Invalid: no source kind.
	errContains(t, validateSwiftGuest(gi(swiftv1alpha1.DataDiskRef{Name: "empty"})),
		"exactly one of imageRef, pvcRef, or blank")

	// Invalid: two source kinds.
	errContains(t, validateSwiftGuest(gi(swiftv1alpha1.DataDiskRef{
		Name:     "two",
		ImageRef: &corev1.LocalObjectReference{Name: "i"},
		Blank:    &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("1Gi")},
	})), "exactly one of imageRef, pvcRef, or blank")

	// Invalid: blank size 0.
	errContains(t, validateSwiftGuest(gi(swiftv1alpha1.DataDiskRef{
		Name:  "zero",
		Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("0")},
	})), "blank.size must be greater than 0")

	// Invalid: attachAsDisk without pvcRef.
	errContains(t, validateSwiftGuest(gi(swiftv1alpha1.DataDiskRef{
		Name:         "bad",
		Blank:        &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("1Gi")},
		AttachAsDisk: true,
	})), "attachAsDisk is only valid with pvcRef")

	// Invalid: duplicate names.
	errContains(t, validateSwiftGuest(gi(
		swiftv1alpha1.DataDiskRef{Name: "dup", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("1Gi")}},
		swiftv1alpha1.DataDiskRef{Name: "dup", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("2Gi")}},
	)), "duplicated")

	// Invalid: a plural entry named "data" collides with the singular shorthand.
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.DataDiskRef = &corev1.LocalObjectReference{Name: "data-img"}
		g.Spec.DataDiskRefs = []swiftv1alpha1.DataDiskRef{
			{Name: "data", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("1Gi")}},
		}
	})), "collides with the implicit name of spec.dataDiskRef")

	// Invalid: more than 8 data disks.
	many := make([]swiftv1alpha1.DataDiskRef, 9)
	for i := range many {
		many[i] = swiftv1alpha1.DataDiskRef{Name: "d" + strconv.Itoa(i), Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("1Gi")}}
	}
	errContains(t, validateSwiftGuest(gi(many...)), "at most 8 data disks")

	// Valid: data disks compose with GPU (no usesGPU rejection).
	if err := validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
		g.Spec.DataDiskRefs = []swiftv1alpha1.DataDiskRef{
			{Name: "db", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("50Gi")}},
		}
	})); err != nil {
		t.Errorf("data disk + GPU should be valid: %v", err)
	}
}

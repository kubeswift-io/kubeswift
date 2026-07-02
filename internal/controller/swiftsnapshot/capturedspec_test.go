package swiftsnapshot

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// A nil rg (CSI/local capture, or a resolve failure) leaves the source-independent
// surface empty — such a clone still needs the live source spec.
func TestCapturedGuestSpec_NilRG_Minimal(t *testing.T) {
	g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{
		ImageRef: &corev1.LocalObjectReference{Name: "ubuntu-noble"},
	}}
	out := capturedGuestSpec(g, nil)
	if out.ImageName != "ubuntu-noble" {
		t.Errorf("imageName = %q, want ubuntu-noble", out.ImageName)
	}
	if out.Storage != nil || out.RootDiskSize != "" || out.CPU != "" || len(out.InterfaceNames) != 0 {
		t.Errorf("nil rg must leave the source-independent surface empty; got %+v", out)
	}
}

// A full-state capture (rg present) freezes the launcher-sufficient surface.
func TestCapturedGuestSpec_FullState_Surface(t *testing.T) {
	g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{
		ImageRef:       &corev1.LocalObjectReference{Name: "ubuntu-noble"},
		SeedProfileRef: &corev1.LocalObjectReference{Name: "ft-seed"},
		DataDiskRefs:   []swiftv1alpha1.DataDiskRef{{Name: "scratch"}},
	}}
	rg := &resolved.ResolvedGuest{
		Resources: resolved.Resources{CPU: 2, Memory: 2048},
		RootDisk:  resolved.RootDisk{Size: resource.MustParse("40Gi")},
		Storage: resolved.Storage{
			AccessMode: "ReadWriteMany", VolumeMode: "Block", StorageClassName: "longhorn-migratable",
		},
		Network:           true,
		Interfaces:        []swiftv1alpha1.GuestInterface{{Name: "eth0"}, {Name: "data"}},
		GuestAgentEnabled: true,
		OSType:            "linux",
	}
	out := capturedGuestSpec(g, rg)
	if out.CPU != "2" || out.MemoryMi != 2048 {
		t.Errorf("resources = cpu %q mem %d, want 2 / 2048", out.CPU, out.MemoryMi)
	}
	if out.RootDiskSize != "40Gi" {
		t.Errorf("rootDiskSize = %q, want 40Gi", out.RootDiskSize)
	}
	if out.Storage == nil || out.Storage.AccessMode != "ReadWriteMany" ||
		out.Storage.VolumeMode != "Block" || out.Storage.StorageClassName != "longhorn-migratable" {
		t.Errorf("storage not frozen: %+v", out.Storage)
	}
	if !out.Network {
		t.Errorf("network must be captured true")
	}
	if len(out.InterfaceNames) != 2 || out.InterfaceNames[0] != "eth0" || out.InterfaceNames[1] != "data" {
		t.Errorf("interfaceNames = %v, want [eth0 data]", out.InterfaceNames)
	}
	if !out.GuestAgent {
		t.Errorf("guestAgent must be captured true")
	}
	if out.OSType != "linux" {
		t.Errorf("osType = %q, want linux", out.OSType)
	}
	if !out.HasSeed {
		t.Errorf("hasSeed must be true (source had a seedProfileRef)")
	}
	if !out.HasDataDisks {
		t.Errorf("hasDataDisks must be true (source had dataDiskRefs)")
	}
}

package resolved

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func TestFromCapturedSpec(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-a", Namespace: "team-a"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			GuestClassRef: corev1.LocalObjectReference{Name: "ft-small"},
		},
	}
	// A minimal class shell: its values are overridden by the captured surface,
	// but Merge parses them, so keep them valid.
	class := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ft-small"},
		Spec: swiftv1alpha1.SwiftGuestClassSpec{
			CPU:      resource.MustParse("1"),
			Memory:   resource.MustParse("1Gi"),
			RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi")},
		},
	}
	c := CapturedInput{
		CPU: 2, MemoryMi: 2048, RootDiskSize: "40Gi",
		AccessMode: "ReadWriteMany", VolumeMode: "Block", StorageClassName: "longhorn-migratable",
		Network: true, OSType: "linux", InterfaceNames: []string{"eth0", "data"},
	}

	rg := FromCapturedSpec(guest, class, c)

	// Resume-specific fields come from the captured surface, not the class.
	if rg.Resources.CPU != 2 || rg.Resources.Memory != 2048 {
		t.Errorf("resources = %+v, want cpu 2 / mem 2048 (from captured, not class)", rg.Resources)
	}
	if rg.RootDisk.Size.String() != "40Gi" || rg.RootDisk.Format != "raw" || !rg.RootDisk.FromOCI {
		t.Errorf("rootDisk = %+v, want 40Gi/raw/FromOCI", rg.RootDisk)
	}
	if rg.Storage.AccessMode != "ReadWriteMany" || rg.Storage.VolumeMode != "Block" ||
		rg.Storage.StorageClassName != "longhorn-migratable" {
		t.Errorf("storage = %+v", rg.Storage)
	}
	if !rg.Network || rg.OSType != "linux" {
		t.Errorf("network=%v osType=%q, want true/linux", rg.Network, rg.OSType)
	}
	if len(rg.Interfaces) != 2 || rg.Interfaces[0].Name != "eth0" || rg.Interfaces[1].Name != "data" {
		t.Errorf("interfaces = %+v, want [eth0 data]", rg.Interfaces)
	}
	// No image was resolved — the disk comes from oci.
	if rg.PreparedImage.Ready || rg.PreparedImage.PVCName != "" {
		t.Errorf("PreparedImage must be empty for a source-independent clone; got %+v", rg.PreparedImage)
	}
	// The system-default shell still comes through Merge.
	if rg.GuestSettings.Firmware != DefaultFirmware || rg.GuestSettings.Bus != DefaultBus {
		t.Errorf("GuestSettings shell not filled from Merge: %+v", rg.GuestSettings)
	}
	if rg.Meta.Name != "clone-a" || rg.Meta.Namespace != "team-a" {
		t.Errorf("Meta = %+v, want clone-a/team-a", rg.Meta)
	}
}

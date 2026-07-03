package resolved

import (
	"k8s.io/apimachinery/pkg/api/resource"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// CapturedInput is the launcher-sufficient surface a source-independent
// full-state clone needs, mapped from a SwiftSnapshot's status.capturedGuestSpec
// by the caller (so this package stays free of the snapshot API). It is the
// resume-specific configuration that overrides the guestClass shell in
// FromCapturedSpec.
type CapturedInput struct {
	CPU              int // cores (from the captured/resumed VM, not the clone's class)
	MemoryMi         int // MiB
	RootDiskSize     string
	AccessMode       string
	VolumeMode       string
	StorageClassName string
	Network          bool
	OSType           string
	InterfaceNames   []string
}

// FromCapturedSpec builds a ResolvedGuest for a source-independent (fully
// cross-cluster) full-state clone — one whose source SwiftGuest / SwiftImage /
// SwiftSeedProfile are all gone, so the effective-spec resolve path
// (resolveDiskBoot) can't run.
//
// It reuses Merge (with a nil image + nil seedProfile) for the system-default
// shell — GuestSettings, Lifecycle, Networks, Meta, CoreScheduling — from the
// clone's OWN guestClass, then overrides the resume-specific fields from the
// captured surface: the disk is materialized from the OCI artifact (RootDisk
// .FromOCI, raw), and the resumed VM's actual resources/storage/os/interfaces
// come from the snapshot, not the clone's class (CH --restore uses the captured
// config.json; the class exists only to satisfy the guestClassRef requirement).
//
// PreparedImage is left empty (no image) — the controller runs
// EnsureRootDiskClone on RootDisk.FromOCI (→ maybeRootDiskFromOCI) to supply the
// disk. Pure: no client, no I/O — the caller fetches the guestClass.
//
// Same-cluster clones are unaffected: they keep resolving via the effective-spec
// path (source guest present). This constructor fires only when the source is
// gone AND the snapshot carries a populated captured surface.
func FromCapturedSpec(guest *swiftv1alpha1.SwiftGuest, guestClass *swiftv1alpha1.SwiftGuestClass, c CapturedInput) *ResolvedGuest {
	rg := Merge(guest, guestClass, nil, nil)

	// Resume-specific overrides (the captured/resumed VM's real config).
	rg.Resources = Resources{CPU: c.CPU, Memory: c.MemoryMi}
	if c.RootDiskSize != "" {
		if q, err := resource.ParseQuantity(c.RootDiskSize); err == nil {
			rg.RootDisk.Size = q
		}
	}
	rg.RootDisk.Format = "raw"
	rg.RootDisk.FromOCI = true
	rg.Storage = Storage{
		AccessMode:       c.AccessMode,
		VolumeMode:       c.VolumeMode,
		StorageClassName: c.StorageClassName,
	}
	rg.Network = c.Network
	if c.OSType != "" {
		rg.OSType = c.OSType
	}
	// Interface names only — enough for the launcher's default per-NIC setup and
	// for deterministic per-clone MAC rewrites (the source MACs are irrelevant;
	// the clone re-derives them). Multi-NIC with secondary NADs is a documented
	// v1 limitation (the NADs must also exist in the target cluster).
	if len(c.InterfaceNames) > 0 {
		rg.Interfaces = make([]swiftv1alpha1.GuestInterface, 0, len(c.InterfaceNames))
		for _, n := range c.InterfaceNames {
			rg.Interfaces = append(rg.Interfaces, swiftv1alpha1.GuestInterface{Name: n})
		}
	}
	return rg
}

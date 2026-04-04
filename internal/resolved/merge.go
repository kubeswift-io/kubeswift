package resolved

import (
	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Merge produces ResolvedGuest from the fetched CRDs.
// Precedence: guest explicit > class > system defaults.
func Merge(
	guest *swiftv1alpha1.SwiftGuest,
	guestClass *swiftv1alpha1.SwiftGuestClass,
	image *imagev1alpha1.SwiftImage,
	seedProfile *seedv1alpha1.SwiftSeedProfile,
) *ResolvedGuest {
	rg := &ResolvedGuest{}

	// Meta from guest
	rg.Meta = Meta{
		Name:      guest.Name,
		Namespace: guest.Namespace,
		UID:       guest.UID,
	}

	// Lifecycle: guest runPolicy > system default
	rg.Lifecycle = mergeLifecycle(guest)

	// Resources: from GuestClass (guest has no override in MVP)
	rg.Resources = mergeResources(guestClass)

	// RootDisk: from GuestClass
	rg.RootDisk = mergeRootDisk(guestClass)

	// GuestSettings: system defaults (class has no override in MVP)
	rg.GuestSettings = GuestSettings{
		Architecture:   DefaultArchitecture,
		Firmware:       DefaultFirmware,
		Bus:            DefaultBus,
		InterfaceModel: DefaultInterfaceModel,
		ShutdownMethod: DefaultShutdownMethod,
	}

	// Networks: one network, system default
	rg.Networks = Networks{InterfaceModel: DefaultInterfaceModel}

	// Seed: from SwiftSeedProfile when referenced
	rg.Seed = mergeSeed(seedProfile)

	// PreparedImage: from SwiftImage when Ready
	rg.PreparedImage = mergePreparedImage(image)

	// All guests get tap+bridge networking; the field exists for future per-guest opt-out.
	rg.Network = true

	// Copy interfaces from SwiftGuest spec for multi-NIC support.
	rg.Interfaces = guest.Spec.Interfaces

	return rg
}

func mergeLifecycle(guest *swiftv1alpha1.SwiftGuest) Lifecycle {
	runPolicy := string(guest.Spec.RunPolicy)
	if runPolicy == "" {
		runPolicy = DefaultRunPolicy
	}
	return Lifecycle{RunPolicy: runPolicy}
}

func mergeResources(guestClass *swiftv1alpha1.SwiftGuestClass) Resources {
	cpu := 0
	if !guestClass.Spec.CPU.IsZero() {
		cpu = int(guestClass.Spec.CPU.Value())
	}
	mem := 0
	if !guestClass.Spec.Memory.IsZero() {
		mem = int(guestClass.Spec.Memory.Value() / (1024 * 1024)) // to MiB
	}
	return Resources{CPU: cpu, Memory: mem}
}

func mergeRootDisk(guestClass *swiftv1alpha1.SwiftGuestClass) RootDisk {
	size := resource.Quantity{}
	format := DefaultDiskFormat
	if guestClass != nil && guestClass.Spec.RootDisk.Size != (resource.Quantity{}) {
		size = guestClass.Spec.RootDisk.Size
	}
	if guestClass != nil && guestClass.Spec.RootDisk.Format != "" {
		format = string(guestClass.Spec.RootDisk.Format)
	}
	return RootDisk{Size: size, Format: format}
}

func mergeSeed(seedProfile *seedv1alpha1.SwiftSeedProfile) *Seed {
	if seedProfile == nil {
		return nil
	}
	s := &Seed{
		Datasource:  string(seedProfile.Spec.Datasource),
		UserData:    seedProfile.Spec.UserData,
		MetaData:    seedProfile.Spec.MetaData,
		NetworkData: seedProfile.Spec.NetworkData,
	}
	if seedProfile.Spec.UserDataFrom != nil {
		s.UserDataFrom = seedProfile.Spec.UserDataFrom
	}
	if seedProfile.Spec.MetaDataFrom != nil {
		s.MetaDataFrom = seedProfile.Spec.MetaDataFrom
	}
	if seedProfile.Spec.NetworkDataFrom != nil {
		s.NetworkDataFrom = seedProfile.Spec.NetworkDataFrom
	}
	return s
}

func mergePreparedImage(image *imagev1alpha1.SwiftImage) PreparedImage {
	if image == nil || image.Status.Phase != imagev1alpha1.SwiftImagePhaseReady || image.Status.PreparedArtifact == nil {
		return PreparedImage{Ready: false}
	}
	pa := image.Status.PreparedArtifact
	size := int64(0)
	if pa.Size != nil {
		size = pa.Size.Value()
	}
	pvcName := ""
	if pa.PVCRef != nil {
		pvcName = pa.PVCRef.Name
	}
	return PreparedImage{
		Path:    "", // Controller sets mount path
		Format:  string(pa.Format),
		Size:    size,
		Ready:   true,
		PVCName: pvcName,
	}
}

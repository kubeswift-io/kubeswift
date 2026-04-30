package resolved

import (
	"fmt"

	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

// KernelBoot holds resolved kernel boot information.
type KernelBoot struct {
	LocalPath     string // e.g. /var/lib/kubeswift/kernels/default-faas-minimal
	KernelCmdline string // effective cmdline: guest override > kernel default
}

// ResolvedGuest is the normalized internal model produced by the resolver.
// The controller uses only this type for runtime decisions after resolution succeeds.
type ResolvedGuest struct {
	GuestSettings GuestSettings  `json:"guestSettings"`
	Resources     Resources      `json:"resources"`
	RootDisk      RootDisk       `json:"rootDisk"`
	Networks      Networks       `json:"networks"`
	Seed          *Seed          `json:"seed,omitempty"`
	Lifecycle     Lifecycle      `json:"lifecycle"`
	PreparedImage PreparedImage  `json:"preparedImage"`
	Meta          Meta           `json:"meta"`
	KernelBoot    *KernelBoot    `json:"kernelBoot,omitempty"`
	DataDisk      *PreparedImage `json:"dataDisk,omitempty"`
	Network       bool           `json:"network"`
	// Storage is the post-merge effective storage spec for controller-
	// created PVCs (today: the root-disk clone). Always non-nil after a
	// successful resolution: defaults are filled in (RWO + Filesystem +
	// empty StorageClassName). The controller writes AccessMode +
	// VolumeMode + StorageClassName onto SwiftGuest.status.storage as an
	// informational echo. liveMigrationCapable is recomputed from this
	// spec at the SwiftMigration validation webhook (not stored in
	// status — see api/swift/v1alpha1.ResolvedStorageStatus's doc comment).
	Storage Storage `json:"storage"`
	// Hypervisor overrides the default hypervisor selection.
	// "qemu" forces the QEMU path; empty or "cloud-hypervisor" uses Cloud Hypervisor.
	// Set by the controller from the kubeswift.io/hypervisor-override annotation.
	Hypervisor string `json:"hypervisor,omitempty"`
	// Interfaces from SwiftGuest spec, used for multi-NIC support.
	// Nil or empty means single default NIC (backward compatible).
	Interfaces []swiftv1alpha1.GuestInterface `json:"interfaces,omitempty"`
}

// GuestSettings holds architecture, firmware, bus, interface model, shutdown method.
// MVP: minimal fields; system defaults apply when not specified.
type GuestSettings struct {
	Architecture   string `json:"architecture"`
	Firmware       string `json:"firmware"`
	Bus            string `json:"bus"`
	InterfaceModel string `json:"interfaceModel"`
	ShutdownMethod string `json:"shutdownMethod"`
}

// Resources holds cpu and memory from the merged spec.
type Resources struct {
	CPU    int `json:"cpu"`    // cores
	Memory int `json:"memory"` // MiB
}

// RootDisk holds size, format, and prepared info for the root disk.
type RootDisk struct {
	Size   resource.Quantity `json:"size"`
	Format string            `json:"format"` // raw or qcow2
}

// Storage is the post-merge effective storage spec for controller-created
// PVCs. Defaults are pre-filled by Merge: AccessMode=ReadWriteOnce,
// VolumeMode=Filesystem, StorageClassName="" (legacy fall-through to the
// source SwiftImage's PVC storage class).
//
// IsLiveMigrationCapable returns true iff AccessMode=ReadWriteMany AND
// VolumeMode=Block — the canonical KubeVirt-style rule. The SwiftMigration
// webhook's ValidateCreate calls it directly to gate live mode; the
// controller writes the AccessMode/VolumeMode/StorageClassName onto
// SwiftGuest.status.storage as an informational echo only.
type Storage struct {
	AccessMode       string `json:"accessMode"`       // ReadWriteOnce or ReadWriteMany
	VolumeMode       string `json:"volumeMode"`       // Filesystem or Block
	StorageClassName string `json:"storageClassName"` // empty = inherit from source SwiftImage's PVC
}

// IsLiveMigrationCapable mirrors swiftv1alpha1.IsLiveMigrationCapable
// against the resolved storage shape; we keep the rule in two places
// because the API package and the resolved package can't share string
// constants without a circular import (the API package can't depend on
// resolved). The two implementations are textually identical and a unit
// test in this package locks them in step.
func (s Storage) IsLiveMigrationCapable() bool {
	return s.AccessMode == "ReadWriteMany" && s.VolumeMode == "Block"
}

// Networks holds network config. MVP: one network.
type Networks struct {
	InterfaceModel string `json:"interfaceModel"`
}

// Seed holds materialization inputs for cloud-init.
// UserData, MetaData, NetworkData are inline strings. When *From is set, the renderer fetches from Secret/ConfigMap.
type Seed struct {
	Datasource      string                          `json:"datasource"`
	UserData        string                          `json:"userData"`
	UserDataFrom    *seedv1alpha1.SeedDataValueFrom `json:"userDataFrom,omitempty"`
	MetaData        string                          `json:"metaData"`
	MetaDataFrom    *seedv1alpha1.SeedDataValueFrom `json:"metaDataFrom,omitempty"`
	NetworkData     string                          `json:"networkData"`
	NetworkDataFrom *seedv1alpha1.SeedDataValueFrom `json:"networkDataFrom,omitempty"`
}

// Lifecycle holds run policy and start/stop intent.
type Lifecycle struct {
	RunPolicy string `json:"runPolicy"` // Running or Stopped
}

// PreparedImage holds the resolved image info from SwiftImage when Ready.
type PreparedImage struct {
	Path    string `json:"path"` // PVC mount path (set by controller)
	Format  string `json:"format"`
	Size    int64  `json:"size"`
	Ready   bool   `json:"ready"`
	PVCName string `json:"pvcName"` // PVC name for pod volume creation (from preparedArtifact.pvcRef)
	// CloneSeed is non-nil when SwiftImage.spec.cloneStrategy is "snapshot"
	// AND the clone-seed VolumeSnapshot is readyToUse=true. When non-nil,
	// per-guest cloning uses CSI dataSource instead of the legacy Copy Job
	// path. Empty/nil means the legacy Copy Job path is used.
	CloneSeed *PreparedCloneSeed `json:"cloneSeed,omitempty"`
}

// PreparedCloneSeed denormalises SwiftImage.status.cloneSeed for use by
// the SwiftGuest controller's clone path.
type PreparedCloneSeed struct {
	Kind            string `json:"kind"`            // "VolumeSnapshot"
	Name            string `json:"name"`            // VolumeSnapshot name
	Namespace       string `json:"namespace"`       // same as SwiftImage; same-namespace constraint
	SourceSizeBytes int64  `json:"sourceSizeBytes"` // Longhorn refuses different-size dataSource clones
}

// Meta holds guest identity for pod naming and logging.
type Meta struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	UID       types.UID `json:"uid"`
}

// HasSeed returns true if seed materialization inputs are present.
func (r *ResolvedGuest) HasSeed() bool {
	return r.Seed != nil && r.Seed.Datasource != ""
}

// HasKernel returns true when the guest boots via kernel+initramfs instead of disk.
func (r *ResolvedGuest) HasKernel() bool {
	return r.KernelBoot != nil
}

// HasNetwork returns true when the guest should have tap+bridge networking.
func (r *ResolvedGuest) HasNetwork() bool {
	return r.Network
}

// HasDataDisk returns true when a secondary data disk is attached.
func (r *ResolvedGuest) HasDataDisk() bool {
	return r.DataDisk != nil && r.DataDisk.Ready
}

// GetDataDiskPVCName returns the PVC name for the data disk volume.
func (r *ResolvedGuest) GetDataDiskPVCName() string {
	if r.DataDisk == nil {
		return ""
	}
	return r.DataDisk.PVCName
}

// GetKernelPath returns the full path to the kernel (bzImage) inside the artifact dir.
func (r *ResolvedGuest) GetKernelPath() string {
	return r.KernelBoot.LocalPath + "/bzImage"
}

// GetInitramfsPath returns the full path to the initramfs inside the artifact dir.
func (r *ResolvedGuest) GetInitramfsPath() string {
	return r.KernelBoot.LocalPath + "/rootfs.cpio.gz"
}

// GetKernelCmdline returns the effective kernel command line.
func (r *ResolvedGuest) GetKernelCmdline() string {
	return r.KernelBoot.KernelCmdline
}

// GetRootDiskFormat returns the root disk format for runtime intent.
func (r *ResolvedGuest) GetRootDiskFormat() string {
	if r.RootDisk.Format != "" {
		return r.RootDisk.Format
	}
	return "raw"
}

// GetCPU returns CPU cores for runtime intent.
func (r *ResolvedGuest) GetCPU() int {
	return r.Resources.CPU
}

// GetMemoryMiB returns memory in MiB for runtime intent.
func (r *ResolvedGuest) GetMemoryMiB() int {
	return r.Resources.Memory
}

// GetLifecycle returns "start" or "stop" for runtime intent.
func (r *ResolvedGuest) GetLifecycle() string {
	if r.Lifecycle.RunPolicy == "Stopped" {
		return "stop"
	}
	return "start"
}

// GetHypervisor returns the hypervisor override, or empty string for default (Cloud Hypervisor).
func (r *ResolvedGuest) GetHypervisor() string {
	return r.Hypervisor
}

// GetGuestID returns a unique ID for the guest (namespace/name).
func (r *ResolvedGuest) GetGuestID() string {
	if r.Meta.Namespace != "" && r.Meta.Name != "" {
		return r.Meta.Namespace + "/" + r.Meta.Name
	}
	return string(r.Meta.UID)
}

// GetNICs builds the NICIntent list from spec.interfaces.
// Returns nil when interfaces is nil/empty (backward compat — single default NIC).
func (r *ResolvedGuest) GetNICs() []runtimeintent.NICIntent {
	if len(r.Interfaces) == 0 {
		return nil
	}
	nics := make([]runtimeintent.NICIntent, 0, len(r.Interfaces))
	tapIdx := 0
	bridgeIdx := 0
	multusIdx := 1 // Multus interfaces start at net1
	for _, iface := range r.Interfaces {
		ifaceType := iface.Type
		if ifaceType == "" {
			ifaceType = swiftv1alpha1.InterfaceTypeBridge
		}

		if ifaceType == swiftv1alpha1.InterfaceTypeSRIOV {
			// SR-IOV: VFIO passthrough — no tap, no bridge, no MAC from controller.
			nic := runtimeintent.NICIntent{
				Name: iface.Name,
				Type: swiftv1alpha1.InterfaceTypeSRIOV,
				SRIOVDevice: &runtimeintent.SRIOVDeviceIntent{
					ResourceName: iface.ResourceName,
				},
			}
			if iface.NetworkRef != nil {
				nic.MultusInterface = fmt.Sprintf("net%d", multusIdx)
				multusIdx++
			}
			nics = append(nics, nic)
			continue
		}

		// Bridge type: tap+bridge+virtio-net.
		nic := runtimeintent.NICIntent{
			Name:      iface.Name,
			Type:      swiftv1alpha1.InterfaceTypeBridge,
			TapDevice: fmt.Sprintf("tap%d", tapIdx),
			MAC:       runtimeintent.GenerateMAC(runtimeintent.InterfaceMACSeed(r.Meta.Namespace, r.Meta.Name, iface.Name)),
			Bridge:    fmt.Sprintf("br%d", bridgeIdx),
		}
		if iface.NetworkRef == nil {
			nic.Primary = true
		} else {
			nic.MultusInterface = fmt.Sprintf("net%d", multusIdx)
			multusIdx++
		}
		nics = append(nics, nic)
		tapIdx++
		bridgeIdx++
	}
	return nics
}

// ResolutionError is returned when resolution fails.
type ResolutionError struct {
	Reason           string `json:"reason"`
	AffectedResource string `json:"affectedResource,omitempty"`
}

func (e *ResolutionError) Error() string {
	return e.Reason
}

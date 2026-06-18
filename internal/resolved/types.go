package resolved

import (
	"fmt"
	"strings"

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
	GuestSettings GuestSettings `json:"guestSettings"`
	Resources     Resources     `json:"resources"`
	RootDisk      RootDisk      `json:"rootDisk"`
	Networks      Networks      `json:"networks"`
	Seed          *Seed         `json:"seed,omitempty"`
	Lifecycle     Lifecycle     `json:"lifecycle"`
	PreparedImage PreparedImage `json:"preparedImage"`
	Meta          Meta          `json:"meta"`
	KernelBoot    *KernelBoot   `json:"kernelBoot,omitempty"`
	// DataDisks are the resolved secondary VM disks, in deterministic
	// declaration order: the legacy singular spec.dataDiskRef first (if set),
	// then spec.dataDiskRefs[] in array order. Each becomes a CH --disk and
	// enumerates in the guest as /dev/vdc, /dev/vdd, ... in this order.
	DataDisks []ResolvedDataDisk `json:"dataDisks,omitempty"`
	Network   bool               `json:"network"`
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
	// PrimaryUDNInterface names the pod's OVN-Kubernetes primary UDN interface
	// (ovn-udn1) when the guest's namespace is a primary-UDN tenant namespace
	// (Model A). Empty otherwise. GetNICs applies it to the primary node-local
	// NIC (a primary interface without a networkRef). Set by the resolver from
	// the namespace label.
	PrimaryUDNInterface string `json:"primaryUDNInterface,omitempty"`
	// NetworkSpec is the SwiftGuest spec.network block (binding + declarative
	// service ports). Nil preserves today's behavior (nat, no exposure).
	// See docs/design/service-exposure.md.
	NetworkSpec *swiftv1alpha1.GuestNetworkSpec `json:"networkSpec,omitempty"`
	// Filesystems from SwiftGuest spec — virtiofs (vhost-user-fs) shares.
	// Nil or empty means no virtiofs mounts. CH path only.
	Filesystems []swiftv1alpha1.Filesystem `json:"filesystems,omitempty"`
	// VhostUserDevices from SwiftGuest spec — operator-backed vhost-user-blk
	// disks and generic vhost-user devices. CH path only.
	VhostUserDevices []swiftv1alpha1.VhostUserDevice `json:"vhostUserDevices,omitempty"`
	// OSType is the resolved guest OS family ("linux" or "windows"). For a
	// disk boot it is taken from the SwiftImage (the image defines the OS);
	// kernel boot is always "linux". Empty resolves to "linux" via GetOSType.
	// Consumed by the runtime layer (PR 4: windows adds kvm_hyperv on the CH
	// disk-boot path) and provisioning (PR 5: cloudbase-init vs cloud-init).
	OSType string `json:"osType,omitempty"`
	// CoreScheduling is the vCPU core-scheduling policy from the SwiftGuestClass
	// ("off"/"vm"/"vcpu"). Empty/"off" omits the CH --cpus core_scheduling param.
	CoreScheduling string `json:"coreScheduling,omitempty"`
	// GuestAgentEnabled is true for a SOURCE guest that opted into the in-guest
	// identity agent (spec.guestAgentEnabled). It gates the CH --vsock device.
	// The resolver sets it false for a clone (cloneFromSnapshot): a clone
	// reopens the captured vsock device from config.json, so it must not also
	// add --vsock. See docs/design/clone-identity-vsock-agent.md.
	GuestAgentEnabled bool `json:"guestAgentEnabled,omitempty"`
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

// Seed holds materialization inputs for the guest's first-boot provisioner.
// The seed pipeline is OS-AGNOSTIC by design: it produces a NoCloud seed.iso
// (volume label "cidata") with flat user-data/meta-data/network-config files,
// read by cloud-init on Linux AND by cloudbase-init on Windows (osType: windows)
// — the same mechanism, no Windows-specific branch. Do not inject OS-specific
// content here; keep it operator-provided passthrough so both consumers work.
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

// ResolvedDataDisk is one resolved secondary VM disk. It abstracts the three
// kinds (image-backed, blank, attached-PVC) into what the pod builder and
// swiftletd need: a PVC to attach and where the disk lives.
type ResolvedDataDisk struct {
	// Name is the disk's stable identifier (the legacy singular dataDiskRef
	// resolves as "data"). Drives the pod volume/device name.
	Name string `json:"name"`
	// PVCName backs this disk: the SwiftImage's prepared PVC (image-backed),
	// a guest-owned PVC the controller creates (blank), or the operator's
	// PVCRef (attached).
	PVCName string `json:"pvcName"`
	// Block is true for a raw block disk (volumeDevices + CH --disk path=/dev/..);
	// false for a filesystem-backed disk (volumeMount + an image.raw file).
	Block bool `json:"block"`
	// HostPath is the value passed to CH --disk path=: the in-pod block device
	// path (Block) or the image.raw file path (Filesystem). Opaque to swiftletd.
	HostPath string `json:"hostPath"`
	// MountPath is the in-pod filesystem mount dir for a Filesystem disk (the
	// directory that contains image.raw); empty for Block disks.
	MountPath string `json:"mountPath,omitempty"`
	// Format is the CH disk format ("raw").
	Format string `json:"format"`
	// Ready is true once the disk is usable: image-backed = SwiftImage Ready;
	// blank/attached = always true (PVC binding is gated by the controller).
	Ready bool `json:"ready"`
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

// HasDataDisks returns true when at least one secondary data disk is attached.
func (r *ResolvedGuest) HasDataDisks() bool {
	return len(r.DataDisks) > 0
}

// GetDataDisks maps the resolved data disks to the runtimeintent type for
// RuntimeIntent (the build.go interface). The pod builder uses the r.DataDisks
// field directly (it needs PVCName/Block/MountPath, not just the CH args).
func (r *ResolvedGuest) GetDataDisks() []runtimeintent.DataDiskSpec {
	if len(r.DataDisks) == 0 {
		return nil
	}
	out := make([]runtimeintent.DataDiskSpec, 0, len(r.DataDisks))
	for _, d := range r.DataDisks {
		out = append(out, runtimeintent.DataDiskSpec{
			Name:   d.Name,
			Path:   d.HostPath,
			Format: d.Format,
		})
	}
	return out
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

// GetRootDiskVolumeMode returns the resolved storage volumeMode
// ("Filesystem" or "Block") for the root disk. Empty string is
// treated as "Filesystem" by callers (the pre-W9 default). Used by
// runtimeintent.Build to decide whether RootDisk.Path resolves to a
// filesystem path or a Block device path.
func (r *ResolvedGuest) GetRootDiskVolumeMode() string {
	return r.Storage.VolumeMode
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

// GetOSType returns the resolved guest OS family ("linux" or "windows"),
// defaulting to "linux" when unset (legacy guests / kernel boot).
func (r *ResolvedGuest) GetOSType() string {
	if r.OSType == "" {
		return "linux"
	}
	return r.OSType
}

// GetCoreScheduling returns the vCPU core-scheduling policy ("vm"/"vcpu"), or
// "" when off/unset (no CH core_scheduling param).
func (r *ResolvedGuest) GetCoreScheduling() string {
	if r.CoreScheduling == "" || r.CoreScheduling == string(swiftv1alpha1.CoreSchedulingOff) {
		return ""
	}
	return r.CoreScheduling
}

// IsWindows reports whether the resolved guest is a Windows guest.
func (r *ResolvedGuest) IsWindows() bool {
	return r.GetOSType() == string(swiftv1alpha1.OSTypeWindows)
}

// GetGuestID returns a unique ID for the guest (namespace/name).
func (r *ResolvedGuest) GetGuestID() string {
	if r.Meta.Namespace != "" && r.Meta.Name != "" {
		return r.Meta.Namespace + "/" + r.Meta.Name
	}
	return string(r.Meta.UID)
}

// GetVsockCID returns the deterministic vsock CID for a SOURCE guest that opted
// into the in-guest identity agent, or 0 when the agent is not enabled. The
// resolver already clears GuestAgentEnabled for a clone, so a clone returns 0
// (it reopens the captured vsock device from config.json instead).
func (r *ResolvedGuest) GetVsockCID() uint32 {
	if !r.GuestAgentEnabled || r.Meta.Namespace == "" || r.Meta.Name == "" {
		return 0
	}
	return runtimeintent.DeriveVsockCID(r.Meta.Namespace, r.Meta.Name)
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
	// An operator may mark one interface primary=true. When unset, the legacy
	// rule applies (every node-local bridge interface is primary) — preserved
	// exactly for backward compatibility.
	explicitPrimary := ""
	for i := range r.Interfaces {
		if r.Interfaces[i].Primary {
			explicitPrimary = r.Interfaces[i].Name
			break
		}
	}
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

		if ifaceType == swiftv1alpha1.InterfaceTypeVhostUser {
			// vhost-user-net: operator-provided backend reached via a node
			// socket. No tap, no bridge, no Multus — just a virtio-net device
			// whose datapath is the backend. MAC is pinned if set, else
			// generated. Never the primary DHCP NIC (the backend owns L2).
			mac := iface.MAC
			if mac == "" {
				mac = runtimeintent.GenerateMAC(runtimeintent.InterfaceMACSeed(r.Meta.Namespace, r.Meta.Name, iface.Name))
			}
			nics = append(nics, runtimeintent.NICIntent{
				Name:            iface.Name,
				Type:            swiftv1alpha1.InterfaceTypeVhostUser,
				MAC:             mac,
				VhostUserSocket: iface.Socket,
			})
			continue
		}

		// Bridge type: tap+bridge+virtio-net.
		mac := iface.MAC
		if mac == "" {
			mac = runtimeintent.GenerateMAC(runtimeintent.InterfaceMACSeed(r.Meta.Namespace, r.Meta.Name, iface.Name))
		}
		nic := runtimeintent.NICIntent{
			Name:      iface.Name,
			Type:      swiftv1alpha1.InterfaceTypeBridge,
			TapDevice: fmt.Sprintf("tap%d", tapIdx),
			MAC:       mac,
			Bridge:    fmt.Sprintf("br%d", bridgeIdx),
		}
		// MultusInterface is set whenever the interface rides a NAD —
		// including when it is ALSO the primary (primary-on-NAD). swiftletd
		// branches on MultusInterface != "" to choose the node-local bridge
		// path vs the Multus-attach path.
		if iface.NetworkRef != nil {
			nic.MultusInterface = fmt.Sprintf("net%d", multusIdx)
			multusIdx++
		}
		if explicitPrimary != "" {
			// Operator picked the primary explicitly; it may ride a NAD.
			nic.Primary = iface.Name == explicitPrimary
		} else {
			// Legacy rule (unchanged): node-local bridges are primary.
			nic.Primary = iface.NetworkRef == nil
		}
		if nic.Primary && iface.NetworkRef == nil && r.PrimaryUDNInterface != "" {
			// Model A: the primary rides the namespace primary OVN-K UDN
			// (ovn-udn1), not the node-local bridge. swiftletd's
			// setup_primary_udn_nic bridges it to br0/tap0; eth0 stays on the
			// cluster default. Skipped for a primary that rides a NAD (net1).
			nic.PrimaryUDNInterface = r.PrimaryUDNInterface
		}
		nics = append(nics, nic)
		tapIdx++
		bridgeIdx++
	}
	return nics
}

// GetExposedPorts builds the PortIntent list from spec.network.ports for a
// nat-bound guest (the in-pod DNAT targets). Returns nil for a bridge-bound
// guest (its ports reach the NAD IP, not via in-pod DNAT) and when no ports
// are declared. See docs/design/service-exposure.md.
func (r *ResolvedGuest) GetExposedPorts() []runtimeintent.PortIntent {
	if r.NetworkSpec == nil || len(r.NetworkSpec.Ports) == 0 {
		return nil
	}
	if r.NetworkSpec.Binding == "bridge" {
		return nil
	}
	out := make([]runtimeintent.PortIntent, 0, len(r.NetworkSpec.Ports))
	for _, p := range r.NetworkSpec.Ports {
		target := p.TargetPort
		if target == 0 {
			target = p.Port
		}
		proto := "tcp"
		if p.Protocol != "" {
			proto = strings.ToLower(string(p.Protocol))
		}
		out = append(out, runtimeintent.PortIntent{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: target,
			Protocol:   proto,
		})
	}
	return out
}

// GetFilesystems builds the FilesystemIntent list from spec.filesystems.
// SourcePath is the canonical in-pod share directory the pod builder mounts
// the hostPath/PVC source into; swiftletd derives the virtiofsd socket from
// the runtime dir. Tag defaults to Name when unset.
func (r *ResolvedGuest) GetFilesystems() []runtimeintent.FilesystemIntent {
	if len(r.Filesystems) == 0 {
		return nil
	}
	out := make([]runtimeintent.FilesystemIntent, 0, len(r.Filesystems))
	for _, fs := range r.Filesystems {
		tag := fs.Tag
		if tag == "" {
			tag = fs.Name
		}
		out = append(out, runtimeintent.FilesystemIntent{
			Name:       fs.Name,
			Tag:        tag,
			SourcePath: runtimeintent.VirtiofsBasePath + "/" + fs.Name,
			ReadOnly:   fs.ReadOnly,
		})
	}
	return out
}

// GetVhostUserDevices builds the VhostUserDeviceIntent list from
// spec.vhostUserDevices. Socket is passed through as-is (the operator's
// node path, mounted into the launcher at the same path by the pod builder).
func (r *ResolvedGuest) GetVhostUserDevices() []runtimeintent.VhostUserDeviceIntent {
	if len(r.VhostUserDevices) == 0 {
		return nil
	}
	out := make([]runtimeintent.VhostUserDeviceIntent, 0, len(r.VhostUserDevices))
	for _, d := range r.VhostUserDevices {
		out = append(out, runtimeintent.VhostUserDeviceIntent{
			Name:       d.Name,
			Type:       d.Type,
			Socket:     d.Socket,
			VirtioID:   d.VirtioID,
			QueueSizes: d.QueueSizes,
		})
	}
	return out
}

// ResolutionError is returned when resolution fails.
type ResolutionError struct {
	Reason           string `json:"reason"`
	AffectedResource string `json:"affectedResource,omitempty"`
}

func (e *ResolutionError) Error() string {
	return e.Reason
}

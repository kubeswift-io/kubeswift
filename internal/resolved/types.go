package resolved

import (
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
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

// GetGuestID returns a unique ID for the guest (namespace/name).
func (r *ResolvedGuest) GetGuestID() string {
	if r.Meta.Namespace != "" && r.Meta.Name != "" {
		return r.Meta.Namespace + "/" + r.Meta.Name
	}
	return string(r.Meta.UID)
}

// ResolutionError is returned when resolution fails.
type ResolutionError struct {
	Reason           string `json:"reason"`
	AffectedResource string `json:"affectedResource,omitempty"`
}

func (e *ResolutionError) Error() string {
	return e.Reason
}

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunPolicy defines the desired run state of a guest.
//
// RunPolicy governs what the controller does when the launcher POD reaches a
// terminal state (Succeeded/Failed) — i.e. when Cloud Hypervisor itself exits.
// A guest *reboot* is NOT such an event: on Cloud Hypervisor v52 a guest reboot
// RESETS THE VM IN PLACE (the CH process and the launcher pod survive, the
// guest restarts), so reboots never trigger RunPolicy. CH exits only on guest
// shutdown/poweroff or a crash; those are what Stopped/RestartOnFailure/Always
// act on. (v51 exited on reboot too, churning the pod — v52 reset-in-place is
// the cleaner behavior; KubeSwift passes no --no-reboot, keeping the default.)
//
//	Running          run; do not auto-restart if CH exits (guest stops).
//	Stopped          keep the guest stopped (no pod).
//	RestartOnFailure recreate the pod if CH exits abnormally (pod Failed).
//	Always           recreate the pod whenever CH exits (Failed or Succeeded).
//
// +kubebuilder:validation:Enum=Running;Stopped;RestartOnFailure;Always
type RunPolicy string

const (
	RunPolicyRunning          RunPolicy = "Running"
	RunPolicyStopped          RunPolicy = "Stopped"
	RunPolicyRestartOnFailure RunPolicy = "RestartOnFailure"
	RunPolicyAlways           RunPolicy = "Always"
)

// OSType is the guest operating-system family. It gates the Linux-only
// provisioning datasource and, for windows, the Cloud Hypervisor runtime
// settings (kvm_hyperv on the disk-boot path). Default "linux" — existing
// guests are unaffected. See docs/design/windows-guest-support.md.
// +kubebuilder:validation:Enum=linux;windows
type OSType string

const (
	OSTypeLinux   OSType = "linux"
	OSTypeWindows OSType = "windows"
)

// ConditionGPUAllocated is set on SwiftGuest when the SwiftGPU controller has
// allocated GPU devices and the guest is ready to be scheduled.
const ConditionGPUAllocated = "GPUAllocated"

// ConditionGPUClaimPending is set on a DRA-backed GPU guest (spec.gpuResourceClaim)
// after the launcher pod has been created with a ResourceClaim but the scheduler
// + DRA driver have not yet allocated a device. Unlike the native backend (which
// decides allocation in the controller before the pod exists), the DRA backend
// defers allocation to pod-schedule time; the controller flips GPUAllocated=True
// only once the claim's allocation result is read back. (DRA Phase 1.)
const ConditionGPUClaimPending = "GPUClaimPending"

// ConditionPortsProgrammed reports whether swiftletd installed the in-pod DNAT
// rules for spec.network.ports (service exposure — docs/design/service-exposure.md).
const ConditionPortsProgrammed = "PortsProgrammed"

// ConditionServiceReady reports whether the per-guest Service and its endpoint
// exist and reference a Ready endpoint.
const ConditionServiceReady = "ServiceReady"

// ConditionEgressReady reports whether the guest's pod netns can reach the
// cluster DNS ClusterIP (the egress observability probe — §4). On kube-proxy
// clusters this reflects the VM's egress; on eBPF kube-proxy-free clusters it is
// the pod-netns signal (the VM's forwarded traffic can bypass the eth0 hook).
const ConditionEgressReady = "EgressReady"

// SwiftGuestPhase is the phase of a SwiftGuest.
// +kubebuilder:validation:Enum=Pending;Scheduling;Running;Stopped;Failed
type SwiftGuestPhase string

const (
	SwiftGuestPhasePending    SwiftGuestPhase = "Pending"
	SwiftGuestPhaseScheduling SwiftGuestPhase = "Scheduling"
	SwiftGuestPhaseRunning    SwiftGuestPhase = "Running"
	SwiftGuestPhaseStopped    SwiftGuestPhase = "Stopped"
	SwiftGuestPhaseFailed     SwiftGuestPhase = "Failed"
)

// SwiftGuestSpec defines the desired state of SwiftGuest.
type SwiftGuestSpec struct {
	// ImageRef references the SwiftImage to boot from (disk boot).
	// Mutually exclusive with kernelRef.
	// +optional
	ImageRef *corev1.LocalObjectReference `json:"imageRef,omitempty"`
	// KernelRef references the SwiftKernel to boot from (kernel boot).
	// Mutually exclusive with imageRef and gpuProfileRef.
	// +optional
	KernelRef     *corev1.LocalObjectReference `json:"kernelRef,omitempty"`
	KernelCmdline string                       `json:"kernelCmdline,omitempty"`
	GuestClassRef corev1.LocalObjectReference  `json:"guestClassRef"`
	// SeedProfileRef references a SwiftSeedProfile for cloud-init (disk boot only).
	// +optional
	SeedProfileRef *corev1.LocalObjectReference `json:"seedProfileRef,omitempty"`
	RunPolicy      RunPolicy                    `json:"runPolicy,omitempty"`
	// GPUProfileRef references a SwiftGPUProfile for GPU passthrough via the
	// NATIVE allocation backend: the SwiftGPU controller picks node+devices
	// (findAndAllocate) before the pod is created. Mutually exclusive with
	// kernelRef (GPU boot requires disk boot with UEFI) and with
	// gpuResourceClaim (pick exactly one GPU allocation backend).
	// +optional
	GPUProfileRef *corev1.LocalObjectReference `json:"gpuProfileRef,omitempty"`
	// GPUResourceClaim opts this guest into the DRA (Dynamic Resource Allocation)
	// GPU backend instead of the native SwiftGPU model: the launcher pod carries
	// a ResourceClaim and the scheduler + a DRA driver (e.g. the NVIDIA DRA
	// driver in VFIO/IOMMUFD mode) allocate the device at pod-schedule time; the
	// controller reads the allocated device back and VFIO-passes it into the VM.
	// Mutually exclusive with gpuProfileRef, kernelRef, cloneFromSnapshot, and
	// osType: windows (same constraints as gpuProfileRef — GPU is disk-boot
	// passthrough). (DRA Phase 1: whole-GPU passthrough; MIG/fractional and
	// multi-node NVLink/IMEX are later phases.)
	// +optional
	GPUResourceClaim *GPUResourceClaimSpec `json:"gpuResourceClaim,omitempty"`
	// CloneFromSnapshot boots this guest as a clone of a SwiftSnapshot (Tier B
	// local or Tier C s3) instead of imageRef/kernelRef — the guest resumes the
	// captured state byte-for-byte (CH --restore) with per-clone identity
	// regeneration. Mutually exclusive with imageRef, kernelRef, and
	// gpuProfileRef (VFIO state cannot be CH-restored). The resumed VM's CPU/
	// memory come from the snapshot, so guestClassRef is not used for resources
	// in this mode — but it is still required by the CRD schema (set it to any
	// class). (Snapshot Phase 4.)
	// +optional
	CloneFromSnapshot *CloneFromSnapshotSource `json:"cloneFromSnapshot,omitempty"`
	// DataDiskRef references a SwiftImage to attach as a secondary data disk.
	// The referenced image must be in Ready state. The disk appears as /dev/vdb
	// inside the guest. Works with all boot paths (disk, kernel, GPU).
	// +optional
	DataDiskRef *corev1.LocalObjectReference `json:"dataDiskRef,omitempty"`
	// Interfaces defines the network interfaces for this guest.
	// If nil or empty, a single default interface is created (backward compatible).
	// The first interface without NetworkRef is the primary interface (DHCP, management).
	// Interfaces with NetworkRef are secondary interfaces backed by Multus/NADs.
	// +optional
	Interfaces []GuestInterface `json:"interfaces,omitempty"`
	// Network configures pod-network binding and declarative service ports
	// (service exposure — see docs/design/service-exposure.md). nil preserves
	// today's behavior (nat binding, no Service). The binding/ports here apply
	// to the guest's PRIMARY interface.
	// +optional
	Network *GuestNetworkSpec `json:"network,omitempty"`
	// TopologySpreadConstraints applied to the launcher pod.
	// Typically set by SwiftGuestPool controller for fleet spread.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	// DataDiskRefs is a list of additional data disks.
	// Each entry references either a SwiftImage or a PVC directly.
	// +optional
	DataDiskRefs []DataDiskRef `json:"dataDiskRefs,omitempty"`
	// Filesystems is a list of virtiofs (vhost-user-fs) shares mounted into the
	// guest. Each entry shares a host directory or PVC into the guest, which
	// mounts it with `mount -t virtiofs <tag> <mountpoint>`. Backed by a
	// virtiofsd process the launcher runs alongside Cloud Hypervisor (CH path
	// only in v1; the QEMU/GPU path rejects this). Works with disk-boot,
	// kernel-boot, and clones. See docs/design/vhost-user-devices.md.
	// +optional
	Filesystems []Filesystem `json:"filesystems,omitempty"`
	// VhostUserDevices is a list of operator-backed vhost-user devices attached
	// to the guest: a vhost-user-blk disk (type: blk) or a generic vhost-user
	// device (type: generic). KubeSwift does not run the backend — it mounts the
	// backend's node socket into the launcher and points Cloud Hypervisor at it
	// (the same operator-provides-the-datapath model as SR-IOV and
	// vhost-user-net). Cloud Hypervisor path only in v1; the QEMU/GPU path
	// rejects this. See docs/design/vhost-user-devices.md.
	// +optional
	VhostUserDevices []VhostUserDevice `json:"vhostUserDevices,omitempty"`
	// NodeName pins the launcher pod to a specific Kubernetes node by
	// setting pod.spec.nodeName directly (bypassing the scheduler).
	// Set by the SwiftMigration controller during the StopAndCopy phase
	// to recreate the launcher pod on the migration target node.
	// Operators may also set it manually for static placement.
	//
	// When set, the pod builder writes pod.Spec.NodeName = NodeName.
	// Direct binding gives fast kubelet rejection on bad fits (~5s
	// OutOfcpu) vs. the indefinite Pending state from a nodeSelector
	// path; the SwiftMigration controller relies on this for clean
	// failure detection.
	//
	// Precedence with gpuProfileRef: when both are set, the validation
	// webhook enforces NodeName == status.GPU.NodeName. The pod builder
	// refuses to build with a Resolved=False condition if they disagree.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// Migration is the per-guest migration policy. If nil, migration is
	// permitted with default settings (preferredMode: auto). Set
	// migration.enabled=false to pin a guest in place — the SwiftMigration
	// validation webhook rejects migrations of pinned guests.
	// +optional
	Migration *MigrationSpec `json:"migration,omitempty"`
	// OSType is the guest OS family: "linux" (default) or "windows". For a
	// disk boot the referenced SwiftImage's osType is authoritative and this
	// field, when set, must agree with it (the resolver cross-checks). windows
	// requires disk boot (imageRef) — kernel boot is Linux-only — and is not
	// supported with gpuProfileRef in v1. Default linux; existing guests are
	// unaffected. (Windows guest support — see docs/design/windows-guest-support.md.)
	// +kubebuilder:default=linux
	// +optional
	OSType OSType `json:"osType,omitempty"`
	// Storage overrides the SwiftGuestClass storage defaults for PVCs
	// the controller creates for this guest (today: the root-disk clone).
	// Per-field merge: each non-empty field overrides the same field on
	// SwiftGuestClass.spec.storage; empty fields fall through to the
	// class default, then to system defaults (ReadWriteOnce + Filesystem
	// + class-of-source-image storage class).
	//
	// Set this when a single guest needs different storage characteristics
	// than its class — for example, opting one guest into RWX+Block for
	// live-migration capability while the rest of the class stays on RWO.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`
}

// MigrationSpec is the per-SwiftGuest migration policy. Defaults are
// enabled=true and preferredMode=auto. To pin a guest in place, set
// enabled=false; the SwiftMigration validation webhook then rejects any
// SwiftMigration referencing this guest.
type MigrationSpec struct {
	// Enabled controls whether SwiftMigrations targeting this guest are
	// allowed. Default true. Set false to pin the guest in place.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// PreferredMode is the migration mode the SwiftMigration controller
	// should pick when spec.mode on the SwiftMigration is "auto".
	// Phase 1 always resolves to offline regardless of this field; the
	// field is here for forward compatibility with Phase 3 (live mode).
	// +kubebuilder:validation:Enum=auto;live;offline
	// +kubebuilder:default=auto
	// +optional
	PreferredMode string `json:"preferredMode,omitempty"`
	// DrainPolicy controls how kubectl drain / the eviction API evacuates
	// this guest off a node (Phase 4 drain integration):
	//   Migrate (default): mode=auto — live-migrate where possible,
	//     offline-migrate (bounded downtime) for VFIO/GPU guests. Drain
	//     always succeeds for an eligible guest.
	//   LiveMigrate: live only; if the guest cannot live-migrate
	//     (VFIO/GPU), deny the drain rather than incur downtime.
	//   Block: always deny the drain; the operator handles the guest
	//     manually.
	// Orthogonal to Enabled (Enabled=false disables migration entirely and
	// also denies drain). Has no effect until the Phase 4 eviction webhook
	// + drain controller ship.
	// +kubebuilder:validation:Enum=Migrate;LiveMigrate;Block
	// +kubebuilder:default=Migrate
	// +optional
	DrainPolicy string `json:"drainPolicy,omitempty"`
}

// DrainPolicy values for MigrationSpec.DrainPolicy (Phase 4 drain
// integration). Kept in sync with the +kubebuilder:validation:Enum marker
// above.
const (
	// DrainPolicyMigrate (default) evacuates the guest with mode=auto: live
	// where possible, offline (bounded downtime) for VFIO/GPU. Drain always
	// succeeds for an eligible guest.
	DrainPolicyMigrate = "Migrate"
	// DrainPolicyLiveMigrate evacuates live only; a guest that cannot
	// live-migrate (VFIO/GPU) blocks the drain rather than incur downtime.
	DrainPolicyLiveMigrate = "LiveMigrate"
	// DrainPolicyBlock always blocks the drain; the operator handles the
	// guest manually.
	DrainPolicyBlock = "Block"
)

// AnnotationDrainRequested, when present on a SwiftGuest, carries the name
// of the node being drained. The Phase 4 eviction webhook stamps it when it
// intercepts an eviction of the guest's launcher pod; the Phase 4 drain
// controller consumes it to create a SwiftMigration off that node and
// clears it once the guest has moved. Inert until the drain controller
// ships.
const AnnotationDrainRequested = "kubeswift.io/drain-requested"

// HasVFIODevices reports whether the guest references VFIO passthrough
// devices (GPU via gpuProfileRef, or any SR-IOV interface). VFIO devices
// cannot live-migrate cross-node — the destination Cloud Hypervisor has no
// equivalent host device to receive the guest's device state — so a guest
// with VFIO devices can only be moved by an offline (release-and-reallocate)
// migration.
//
// This is the canonical predicate. The SwiftMigration controller
// (auto_mode.go) and validation webhook (validator.go) carry the same check
// inline (a package-cycle workaround that predates this method).
func (g *SwiftGuest) HasVFIODevices() bool {
	if g.Spec.GPUProfileRef != nil || g.Spec.GPUResourceClaim != nil {
		return true
	}
	return g.HasSRIOVInterface()
}

// GPUBackend returns which GPU allocation backend this guest selects:
// "native" (spec.gpuProfileRef), "dra" (spec.gpuResourceClaim), or "" (no GPU).
// The webhook enforces that at most one of the two is set, so the order here is
// not load-bearing.
func (g *SwiftGuest) GPUBackend() string {
	switch {
	case g.Spec.GPUProfileRef != nil:
		return GPUBackendNative
	case g.Spec.GPUResourceClaim != nil:
		return GPUBackendDRA
	default:
		return ""
	}
}

// GPU allocation backend identifiers (see GPUBackend).
const (
	GPUBackendNative = "native"
	GPUBackendDRA    = "dra"
)

// HasNodeLocalVirtioBackends reports whether the guest uses virtio devices
// whose backend is a node-local process or socket that cannot follow a live
// migration: virtiofs shares (spec.filesystems — the virtiofsd backend and its
// hostPath/PVC source mount live in the source launcher pod) and vhost-user
// devices (spec.vhostUserDevices and vhost-user NICs — the operator's
// DPDK/SPDK backend socket is node-local). Live migration would resume the
// guest on a destination with no equivalent backend, breaking the device
// mid-flight — so such guests are OFFLINE-only, like VFIO: the offline path
// recreates the launcher pod on the target, where the pod builder re-mounts
// the sources and swiftletd respawns/reconnects the backends (for hostPath
// sources and operator sockets, provisioning equivalent content on the target
// is the operator's documented responsibility).
// See docs/design/vhost-user-devices.md §7.
func (g *SwiftGuest) HasNodeLocalVirtioBackends() bool {
	if len(g.Spec.Filesystems) > 0 || len(g.Spec.VhostUserDevices) > 0 {
		return true
	}
	for i := range g.Spec.Interfaces {
		if g.Spec.Interfaces[i].Type == InterfaceTypeVhostUser {
			return true
		}
	}
	return false
}

// HasSRIOVInterface reports whether the guest has any SR-IOV (VFIO NIC)
// interface. SR-IOV NIC passthrough cannot be migrated off a node by the GPU
// release-and-reallocate path (that handles GPUs only; NIC reattach on the
// target is out of scope) — so an sriov guest is not auto-evacuated on drain,
// regardless of GPU presence.
func (g *SwiftGuest) HasSRIOVInterface() bool {
	for _, iface := range g.Spec.Interfaces {
		if iface.Type == InterfaceTypeSRIOV {
			return true
		}
	}
	return false
}

// OfflineGPUMigratable reports whether the guest can be evacuated off a node by
// the offline GPU release-and-reallocate path: it has a GPU profile and no
// SR-IOV NIC (which the path cannot reattach on the target).
func (g *SwiftGuest) OfflineGPUMigratable() bool {
	return g.Spec.GPUProfileRef != nil && !g.HasSRIOVInterface()
}

// UsesCloneFromSnapshot reports whether this guest boots as a clone of a
// SwiftSnapshot (Snapshot Phase 4) rather than from imageRef/kernelRef.
func (g *SwiftGuest) UsesCloneFromSnapshot() bool {
	return g.Spec.CloneFromSnapshot != nil && g.Spec.CloneFromSnapshot.SnapshotRef.Name != ""
}

// PrimaryInterface returns the guest's primary network interface, or nil when
// the guest has no interfaces (the default single node-local NIC) or none
// qualifies. The primary is the interface with Primary=true; if none is marked,
// it falls back to the first node-local (no networkRef) bridge interface — the
// legacy rule, matching the resolver's GetNICs primary selection.
func (g *SwiftGuest) PrimaryInterface() *GuestInterface {
	isBridge := func(iface *GuestInterface) bool {
		return iface.Type == "" || iface.Type == InterfaceTypeBridge
	}
	// 1. Explicit primary wins.
	for i := range g.Spec.Interfaces {
		if g.Spec.Interfaces[i].Primary {
			return &g.Spec.Interfaces[i]
		}
	}
	// 2. Legacy rule: the first node-local (no networkRef) bridge interface is
	//    the primary (its IP comes from node-local dnsmasq).
	for i := range g.Spec.Interfaces {
		if isBridge(&g.Spec.Interfaces[i]) && g.Spec.Interfaces[i].NetworkRef == nil {
			return &g.Spec.Interfaces[i]
		}
	}
	// 3. No node-local bridge: the guest's only/main IP is on a NAD. The first
	//    bridge interface (which therefore rides a networkRef) is the de-facto
	//    primary. This keeps a single-NAD-interface guest correctly classified
	//    as IP-preserving.
	for i := range g.Spec.Interfaces {
		if isBridge(&g.Spec.Interfaces[i]) {
			return &g.Spec.Interfaces[i]
		}
	}
	return nil
}

// PrimaryIPPreservedCrossNode reports whether the guest's primary IP rides a
// multi-node Multus NAD (the primary interface has a networkRef) and therefore
// survives a move to another node. Guests on the default node-local bridge
// return false. The SwiftMigration webhook + controller use this to decide
// whether cross-node migration changes the guest IP (and thus requires
// spec.allowIPChange). SR-IOV guests are handled by the VFIO refusal before
// this is consulted.
//
// This replaces the earlier "any interface with networkRef is multi-node"
// heuristic (which let a node-local-primary + secondary-NAD guest skip
// allowIPChange even though its primary IP changed). See
// docs/design/network-architecture-requirements.md §7.
func (g *SwiftGuest) PrimaryIPPreservedCrossNode() bool {
	p := g.PrimaryInterface()
	return p != nil && p.NetworkRef != nil
}

// CloneIdentityItem names a guest-identity attribute to regenerate on a
// cloneFromSnapshot clone. Mirrors snapshot.IdentityRegenerationItem but is
// defined locally to keep the swift and snapshot api groups decoupled.
// +kubebuilder:validation:Enum=hostname;machineId;sshHostKeys;macAddresses
type CloneIdentityItem string

const (
	CloneRegenHostname     CloneIdentityItem = "hostname"
	CloneRegenMachineID    CloneIdentityItem = "machineId"
	CloneRegenSSHHostKeys  CloneIdentityItem = "sshHostKeys"
	CloneRegenMACAddresses CloneIdentityItem = "macAddresses"
)

// CloneFromSnapshotSource selects a SwiftSnapshot to clone a SwiftGuest from.
type CloneFromSnapshotSource struct {
	// SnapshotRef names a SwiftSnapshot in the same namespace. It must be Ready.
	SnapshotRef corev1.LocalObjectReference `json:"snapshotRef"`
	// TargetNode pins where this clone runs. Required for an s3 (Tier C) snapshot
	// whose capture node is not where the clone should run (same role as
	// SwiftRestore.spec.targetNode); ignored for a Tier B (local) snapshot (the
	// clone is pinned to the capture node). In a SwiftGuestPool, the pool fills
	// this per replica from the replica's scheduled node.
	// +optional
	TargetNode string `json:"targetNode,omitempty"`
	// Regenerate lists identity attributes reset on the clone. macAddresses is
	// ALWAYS forced on (two clones sharing a host-side MAC L2-collide); this list
	// controls hostname/machineId/sshHostKeys (which fire on the clone's first
	// reboot via the seed bootcmd). Empty defaults to all four.
	// +optional
	Regenerate []CloneIdentityItem `json:"regenerate,omitempty"`
}

// DataDiskRef references either a SwiftImage or a PVC for a data disk.
type DataDiskRef struct {
	// Name identifies this data disk (used for volume naming).
	Name string `json:"name"`
	// ImageRef references a SwiftImage for this data disk.
	// Mutually exclusive with PVCRef.
	// +optional
	ImageRef *corev1.LocalObjectReference `json:"imageRef,omitempty"`
	// PVCRef references a PersistentVolumeClaim directly.
	// Used by SwiftGuestPool for per-replica persistent storage.
	// Mutually exclusive with ImageRef.
	// +optional
	PVCRef *corev1.LocalObjectReference `json:"pvcRef,omitempty"`
}

// Filesystem is a virtiofs (vhost-user-fs) share mounted into the guest.
type Filesystem struct {
	// Name uniquely identifies this filesystem within the guest. Drives the
	// virtiofsd socket name and the in-pod source mount path. DNS-label-ish:
	// lowercase alphanumeric + '-', <= 36 chars.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Tag is the virtiofs mount tag the guest uses:
	// `mount -t virtiofs <tag> <mountpoint>`. Unique per guest; defaults to
	// Name when empty. The virtiofs protocol caps the tag at 36 bytes.
	// +kubebuilder:validation:MaxLength=36
	// +optional
	Tag string `json:"tag,omitempty"`
	// Source is the backing directory shared into the guest. Exactly one of
	// hostPath or pvcRef.
	Source FilesystemSource `json:"source"`
	// ReadOnly shares the source read-only (virtiofsd --readonly); the guest
	// cannot mutate the backing content. Default false.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// FilesystemSource is the backing store for a Filesystem (exactly one set).
type FilesystemSource struct {
	// HostPath shares a node-local directory (created if absent,
	// DirectoryOrCreate). Node-pinned content; not portable across nodes.
	// +optional
	HostPath *string `json:"hostPath,omitempty"`
	// PVCRef shares a PersistentVolumeClaim. Use an RWX claim to share the same
	// content across guests/nodes; an RWO claim pins the guest to the claim.
	// +optional
	PVCRef *corev1.LocalObjectReference `json:"pvcRef,omitempty"`
}

// InterfaceType constants for GuestInterface.Type.
const (
	InterfaceTypeBridge    = "bridge"
	InterfaceTypeSRIOV     = "sriov"
	InterfaceTypeVhostUser = "vhost-user"
)

// VhostUserDeviceType constants for VhostUserDevice.Type.
const (
	// VhostUserDeviceTypeBlk is a vhost-user-blk disk (CH --disk vhost_user=on).
	VhostUserDeviceTypeBlk = "blk"
	// VhostUserDeviceTypeGeneric is a generic vhost-user device
	// (CH --generic-vhost-user virtio_id=...).
	VhostUserDeviceTypeGeneric = "generic"
)

// VhostUserDevice is an operator-backed vhost-user device. The operator runs
// the backend (e.g. SPDK for blk) exposing a vhost-user listener socket on the
// node; KubeSwift mounts the socket's directory into the launcher and points
// Cloud Hypervisor at it. KubeSwift does not run the backend.
type VhostUserDevice struct {
	// Name uniquely identifies this device within the guest.
	// Lowercase alphanumeric + '-', <= 36 chars.
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Type selects the device kind:
	//   blk:     a vhost-user block device (appears as a virtio-blk disk in the
	//            guest) — Cloud Hypervisor `--disk vhost_user=on,socket=`.
	//   generic: any vhost-user device by virtio id — Cloud Hypervisor
	//            `--generic-vhost-user virtio_id=,socket=,queue_sizes=`.
	// +kubebuilder:validation:Enum=blk;generic
	Type string `json:"type"`
	// Socket is the node-local path of the operator's vhost-user backend
	// listener (e.g. /var/run/spdk/vhost.0). Required. Its parent directory is
	// mounted into the launcher pod so Cloud Hypervisor can connect.
	Socket string `json:"socket"`
	// VirtioID is the virtio device-type id for a generic device: a number or a
	// symbolic name (e.g. "block", "fs", "net"). Required when type is generic;
	// ignored for blk.
	// +optional
	VirtioID string `json:"virtioId,omitempty"`
	// QueueSizes optionally sets per-queue sizes for a generic device. Ignored
	// for blk. When empty, Cloud Hypervisor's defaults apply.
	// +optional
	QueueSizes []int32 `json:"queueSizes,omitempty"`
}

// GuestInterface defines a single network interface for a SwiftGuest.
type GuestInterface struct {
	// Name is a unique identifier for this interface.
	// Used in status reporting and logging.
	Name string `json:"name"`
	// Type specifies the interface type.
	//   bridge:     (default) tap+bridge, virtio-net in guest. Used for overlay and standard networks.
	//   sriov:      SR-IOV VF passthrough via VFIO. Guest sees hardware NIC. Requires SR-IOV NAD.
	//   vhost-user: virtio-net whose datapath is an operator-provided vhost-user
	//               backend (DPDK/OVS-DPDK) reached via Socket. Cloud Hypervisor
	//               only (v1); KubeSwift does not run the backend.
	// +kubebuilder:validation:Enum=bridge;sriov;vhost-user
	// +kubebuilder:default=bridge
	// +optional
	Type string `json:"type,omitempty"`
	// NetworkRef references a NetworkAttachmentDefinition for this interface.
	// If nil, this is a node-local tap+bridge interface.
	// If set, Multus attaches the pod to the referenced NAD.
	// +optional
	NetworkRef *NetworkReference `json:"networkRef,omitempty"`
	// Primary marks this interface as the guest's primary NIC (its
	// status.network.primaryIP and the DHCP/management interface). At most one
	// interface may set primary=true; if none does, the first interface without
	// a networkRef is the primary (backward compatible).
	//
	// When primary=true AND networkRef is set, the primary NIC rides a
	// multi-node Multus NAD instead of the node-local bridge: KubeSwift skips
	// tap0+br0 NAT + node-local dnsmasq for it, the guest's IP comes from the
	// NAD's IPAM, and the IP is discovered by snooping the bridge neighbor
	// table (there is no node-local DHCP lease). This is what makes the guest's
	// primary IP portable across nodes (e.g. for IP-preserving live migration —
	// see docs/design/network-architecture-requirements.md). The operator
	// putting the primary on a NAD is also the attestation that the NAD is a
	// genuine multi-node L2 (the SwiftMigration webhook treats such a guest as
	// IP-preserving).
	// +optional
	Primary bool `json:"primary,omitempty"`
	// ResourceName is the SR-IOV device plugin resource name (e.g., "intel.com/sriov_netdevice").
	// Required when type is "sriov". The device plugin allocates a VF and the controller
	// adds this resource to the pod's resource limits.
	// +optional
	ResourceName string `json:"resourceName,omitempty"`
	// Socket is the node-local path of the operator-provided vhost-user backend
	// listener (e.g. /var/run/vhost/fast0.sock). Required when type is
	// "vhost-user"; its parent directory is mounted into the launcher pod so
	// Cloud Hypervisor can connect. Not used for bridge/sriov.
	// +optional
	Socket string `json:"socket,omitempty"`
	// MAC optionally pins the interface MAC address. When empty a deterministic
	// MAC is generated. Honored for vhost-user (and bridge) interfaces.
	// +optional
	MAC string `json:"mac,omitempty"`
}

// NetworkReference references a Multus NetworkAttachmentDefinition.
type NetworkReference struct {
	// Name of the NetworkAttachmentDefinition.
	Name string `json:"name"`
	// Namespace of the NetworkAttachmentDefinition.
	// Defaults to the SwiftGuest's namespace if omitted.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// GuestRuntimeStatus holds runtime process information.
type GuestRuntimeStatus struct {
	PID        int64  `json:"pid,omitempty"`
	Hypervisor string `json:"hypervisor,omitempty"`
}

// GuestConsoleStatus holds console access information.
type GuestConsoleStatus struct {
	SerialSocket string `json:"serialSocket,omitempty"`
}

// GuestNetworkInterface represents a single network interface with its IP.
type GuestNetworkInterface struct {
	Name string `json:"name,omitempty"`
	MAC  string `json:"mac,omitempty"`
	IP   string `json:"ip,omitempty"`
}

// GuestNetworkSpec configures the primary interface's pod-network binding and
// the declarative service ports exposed from the guest.
// See docs/design/service-exposure.md.
type GuestNetworkSpec struct {
	// Binding selects the primary interface's relationship to the pod network:
	//   nat    (default) — VM behind the pod IP; ports are Service-exposable via
	//                      an in-pod DNAT (KubeVirt masquerade model).
	//   bridge          — primary rides a multi-node-L2 NAD (portable IP); ports
	//                      are NOT in-pod-DNAT'd (they reach the NAD IP). expose
	//                      is rejected for bridge; ports without expose are
	//                      allowed (for NetworkPolicy port targeting).
	// +kubebuilder:validation:Enum=nat;bridge
	// +kubebuilder:default=nat
	// +optional
	Binding string `json:"binding,omitempty"`
	// Ports declares guest service ports. On a nat-bound guest each port installs
	// an in-pod DNAT podIP:port -> vmIP:targetPort and a launcher containerPort;
	// set Expose on a port to mint a Service. Empty = no exposure (today).
	// +optional
	Ports []GuestPort `json:"ports,omitempty"`
}

// GuestPort declares one exposed guest service port.
type GuestPort struct {
	// Name is a DNS-label identifier; REQUIRED when more than one port is
	// declared (it becomes the Service port name).
	// +optional
	Name string `json:"name,omitempty"`
	// Port is the port reachable on the pod IP (and the Service port).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// TargetPort is the in-guest listening port. Defaults to Port when zero.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	TargetPort int32 `json:"targetPort,omitempty"`
	// Protocol is TCP (default), UDP, or SCTP.
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
	// Expose, when set, makes the controller mint ONE per-guest Service of this
	// type targeting the launcher pod (all exposed ports share that one Service).
	// Omitted = DNAT only (reachable pod->VM by the pod IP; no Service object).
	// Rejected when Binding is bridge.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	Expose string `json:"expose,omitempty"`
}

// GuestNetworkStatus holds discovered guest network information.
type GuestNetworkStatus struct {
	PrimaryIP  string                  `json:"primaryIP,omitempty"`
	Interface  string                  `json:"interface,omitempty"`
	Ready      bool                    `json:"ready,omitempty"`
	Interfaces []GuestNetworkInterface `json:"interfaces,omitempty"`
	// Egress reports verified VM->cluster-service reachability (populated by a
	// later phase; "" until probed). See docs/design/service-exposure.md §4.
	// +optional
	Egress string `json:"egress,omitempty"`
	// ExposedPorts echoes the service ports programmed for this guest.
	// +optional
	ExposedPorts []ExposedPortStatus `json:"exposedPorts,omitempty"`
	// ServiceRef names the per-guest Service the controller created, if any.
	// +optional
	ServiceRef *corev1.LocalObjectReference `json:"serviceRef,omitempty"`
}

// ExposedPortStatus echoes one programmed service port.
type ExposedPortStatus struct {
	Name       string          `json:"name,omitempty"`
	Port       int32           `json:"port"`
	TargetPort int32           `json:"targetPort"`
	Protocol   corev1.Protocol `json:"protocol,omitempty"`
}

// GPUResourceClaimSpec selects the DRA GPU allocation backend (see
// SwiftGuestSpec.GPUResourceClaim). Exactly one of ResourceClaimName /
// ResourceClaimTemplateName must be set (the webhook enforces this).
//
// Because there is no SwiftGPUProfile in DRA mode, the few VM-shape knobs the
// passthrough runtime still needs — which the profile otherwise carries — live
// here: Tier selects the hypervisor/firmware (pcie -> Cloud Hypervisor, hgx-* ->
// QEMU), and Hugepages sizes the GPU memory backing. DRA decides the device;
// KubeSwift still owns the VM. (DRA Phase 1 targets whole-GPU pcie passthrough;
// the hgx NUMA/pinning surface is a later, hardware-gated phase.)
type GPUResourceClaimSpec struct {
	// ResourceClaimName references a pre-created, shared ResourceClaim.
	// Mutually exclusive with ResourceClaimTemplateName.
	// +optional
	ResourceClaimName string `json:"resourceClaimName,omitempty"`
	// ResourceClaimTemplateName references a ResourceClaimTemplate; the
	// controller stamps it into pod.spec.resourceClaims so the scheduler mints
	// a per-pod ResourceClaim. Mutually exclusive with ResourceClaimName.
	// +optional
	ResourceClaimTemplateName string `json:"resourceClaimTemplateName,omitempty"`
	// RequestName is the device-request name within the claim to read the
	// allocation result back from. Defaults to "gpu" when empty.
	// +optional
	RequestName string `json:"requestName,omitempty"`
	// Tier selects the hypervisor/firmware exactly as SwiftGPUProfile.Tier does:
	// pcie -> Cloud Hypervisor (default), hgx-shared/hgx-full -> QEMU.
	// +kubebuilder:validation:Enum=pcie;hgx-shared;hgx-full
	// +kubebuilder:default=pcie
	// +optional
	Tier string `json:"tier,omitempty"`
	// Hugepages sizes the GPU memory hugepage backing ("1Gi", "2Mi", or "").
	// +optional
	Hugepages string `json:"hugepages,omitempty"`
}

// GPUStatus holds GPU allocation and runtime information for a SwiftGuest.
// Populated by the GPU allocation backend (native: spec.gpuProfileRef; dra:
// spec.gpuResourceClaim).
type GPUStatus struct {
	// Devices lists the PCI addresses of allocated GPUs (e.g. "0000:41:00.0").
	Devices []string `json:"devices,omitempty"`
	// PartitionID is the Fabric Manager partition ID for shared NVSwitch mode.
	// -1 means no partition (isolated or full passthrough mode).
	PartitionID int `json:"partitionId,omitempty"`
	// NUMANodes lists the NUMA node IDs the allocated GPUs are attached to.
	NUMANodes []int `json:"numaNodes,omitempty"`
	// Hypervisor is the resolved hypervisor for this guest ("cloud-hypervisor" or "qemu").
	Hypervisor string `json:"hypervisor,omitempty"`
	// NodeName is the Kubernetes node where GPUs were allocated.
	NodeName string `json:"nodeName,omitempty"`
}

// SwiftGuestStatus defines the observed state of SwiftGuest.
type SwiftGuestStatus struct {
	Phase           SwiftGuestPhase         `json:"phase,omitempty"`
	Conditions      []metav1.Condition      `json:"conditions,omitempty"`
	NodeName        string                  `json:"nodeName,omitempty"`
	PodRef          *corev1.ObjectReference `json:"podRef,omitempty"`
	Network         *GuestNetworkStatus     `json:"network,omitempty"`
	Runtime         *GuestRuntimeStatus     `json:"runtime,omitempty"`
	Console         *GuestConsoleStatus     `json:"console,omitempty"`
	RestartCount    int32                   `json:"restartCount,omitempty"`
	LastRestartTime *metav1.Time            `json:"lastRestartTime,omitempty"`
	// GPU contains GPU allocation and runtime status.
	// Populated when spec.gpuProfileRef is set.
	// +optional
	GPU *GPUStatus `json:"gpu,omitempty"`
	// Storage echoes the resolved storage spec actually used for
	// controller-created PVCs (today: the root-disk clone). Informational
	// only — useful for kubectl describe and operator debugging to confirm
	// the per-field merge resolved the way the operator expected.
	//
	// liveMigrationCapable is intentionally NOT a field here; see
	// ResolvedStorageStatus's doc comment for the write-back-race
	// rationale. Use IsLiveMigrationCapable on the resolved spec.
	// +optional
	Storage *ResolvedStorageStatus `json:"storage,omitempty"`
}

// SwiftGuest is the Schema for the swiftguests API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftguests,scope=Namespaced,shortName=sg
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.network.primaryIP`
// +kubebuilder:printcolumn:name="Hypervisor",type=string,JSONPath=`.status.runtime.hypervisor`,priority=1
// +kubebuilder:printcolumn:name="OS",type=string,JSONPath=`.spec.osType`,priority=1
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.status.network.serviceRef.name`,priority=1
// +kubebuilder:printcolumn:name="Egress",type=string,JSONPath=`.status.network.egress`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftGuest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SwiftGuestSpec   `json:"spec,omitempty"`
	Status SwiftGuestStatus `json:"status,omitempty"`
}

// SwiftGuestList contains a list of SwiftGuest.
// +kubebuilder:object:root=true
type SwiftGuestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftGuest `json:"items"`
}

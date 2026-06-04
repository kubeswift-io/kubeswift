package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunPolicy defines the desired run state of a guest.
// +kubebuilder:validation:Enum=Running;Stopped;RestartOnFailure;Always
type RunPolicy string

const (
	RunPolicyRunning          RunPolicy = "Running"
	RunPolicyStopped          RunPolicy = "Stopped"
	RunPolicyRestartOnFailure RunPolicy = "RestartOnFailure"
	RunPolicyAlways           RunPolicy = "Always"
)

// ConditionGPUAllocated is set on SwiftGuest when the SwiftGPU controller has
// allocated GPU devices and the guest is ready to be scheduled.
const ConditionGPUAllocated = "GPUAllocated"

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
	// GPUProfileRef references a SwiftGPUProfile for GPU passthrough.
	// When set, the SwiftGPU controller allocates GPUs before the pod is created.
	// Mutually exclusive with kernelRef (GPU boot requires disk boot with UEFI).
	// +optional
	GPUProfileRef *corev1.LocalObjectReference `json:"gpuProfileRef,omitempty"`
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
	// TopologySpreadConstraints applied to the launcher pod.
	// Typically set by SwiftGuestPool controller for fleet spread.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	// DataDiskRefs is a list of additional data disks.
	// Each entry references either a SwiftImage or a PVC directly.
	// +optional
	DataDiskRefs []DataDiskRef `json:"dataDiskRefs,omitempty"`
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
	if g.Spec.GPUProfileRef != nil {
		return true
	}
	return g.HasSRIOVInterface()
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

// InterfaceType constants for GuestInterface.Type.
const (
	InterfaceTypeBridge = "bridge"
	InterfaceTypeSRIOV  = "sriov"
)

// GuestInterface defines a single network interface for a SwiftGuest.
type GuestInterface struct {
	// Name is a unique identifier for this interface.
	// Used in status reporting and logging.
	Name string `json:"name"`
	// Type specifies the interface type.
	//   bridge: (default) tap+bridge, virtio-net in guest. Used for overlay and standard networks.
	//   sriov:  SR-IOV VF passthrough via VFIO. Guest sees hardware NIC. Requires SR-IOV NAD.
	// +kubebuilder:validation:Enum=bridge;sriov
	// +kubebuilder:default=bridge
	// +optional
	Type string `json:"type,omitempty"`
	// NetworkRef references a NetworkAttachmentDefinition for this interface.
	// If nil, this is the primary interface using KubeSwift's default tap+bridge networking.
	// If set, Multus attaches the pod to the referenced NAD.
	// +optional
	NetworkRef *NetworkReference `json:"networkRef,omitempty"`
	// ResourceName is the SR-IOV device plugin resource name (e.g., "intel.com/sriov_netdevice").
	// Required when type is "sriov". The device plugin allocates a VF and the controller
	// adds this resource to the pod's resource limits.
	// +optional
	ResourceName string `json:"resourceName,omitempty"`
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

// GuestNetworkStatus holds discovered guest network information.
type GuestNetworkStatus struct {
	PrimaryIP  string                  `json:"primaryIP,omitempty"`
	Interface  string                  `json:"interface,omitempty"`
	Ready      bool                    `json:"ready,omitempty"`
	Interfaces []GuestNetworkInterface `json:"interfaces,omitempty"`
}

// GPUStatus holds GPU allocation and runtime information for a SwiftGuest.
// Populated when spec.gpuProfileRef is set.
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

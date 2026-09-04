package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DiskFormat is the disk image format.
// +kubebuilder:validation:Enum=raw;qcow2
type DiskFormat string

const (
	DiskFormatRaw   DiskFormat = "raw"
	DiskFormatQcow2 DiskFormat = "qcow2"
)

// RootDiskSpec defines the root disk for a guest.
type RootDiskSpec struct {
	Size   resource.Quantity `json:"size"`
	Format DiskFormat        `json:"format"`
}

// CoreScheduling selects the Cloud Hypervisor vCPU core-scheduling policy
// (Cloud Hypervisor `--cpus core_scheduling=`), a defense against cross-thread SMT
// (hyper-threading) side channels without disabling SMT host-wide.
//
//	off  (default) no core-scheduling.
//	vm   all of the guest's vCPUs share one core-scheduling group, so a
//	     physical core's sibling threads run only this guest's vCPUs (never
//	     another tenant's) — the multi-tenant isolation setting.
//	vcpu each vCPU is its own group (strongest; siblings never co-run even
//	     within the guest).
//
// +kubebuilder:validation:Enum=off;vm;vcpu
type CoreScheduling string

const (
	CoreSchedulingOff  CoreScheduling = "off"
	CoreSchedulingVM   CoreScheduling = "vm"
	CoreSchedulingVCPU CoreScheduling = "vcpu"
)

// CPUPinning selects how a guest's vCPUs are placed on host CPUs
// (Cloud Hypervisor `--cpus affinity=`).
//
//	none   (default) vCPU threads float across every CPU the launcher pod is
//	       allowed to use; the host scheduler places them.
//	static each vCPU is pinned to one distinct host CPU, chosen from the
//	       launcher pod's OWN cpuset. Trades scheduler flexibility for
//	       run-to-run latency predictability.
//
// The candidate CPUs are always the launcher pod's effective cpuset, never the
// node's full CPU list: under the kubelet CPU Manager `static` policy that set
// is the pod's exclusive allocation, and pinning outside it would be clamped or
// rejected by the kernel. With the policy at `none` the set is simply every
// host CPU, so the same code is correct either way.
//
// +kubebuilder:validation:Enum=none;static
type CPUPinning string

const (
	CPUPinningNone   CPUPinning = "none"
	CPUPinningStatic CPUPinning = "static"
)

// SMTPolicy selects which SMT (hyper-thread) siblings a statically pinned
// guest's vCPUs land on. Ignored unless cpuPinning is static.
//
//	spread (default) use one thread per physical core before any core's
//	       second thread — each vCPU gets a core's full pipeline and L1/L2.
//	pack   fill both siblings of a core before moving to the next core —
//	       touches fewer cores, leaving whole cores free for other work.
//
// This is placement, and is independent of coreScheduling, which decides who
// may share a core (an isolation control). A guest can set both.
//
// +kubebuilder:validation:Enum=spread;pack
type SMTPolicy string

const (
	SMTPolicySpread SMTPolicy = "spread"
	SMTPolicyPack   SMTPolicy = "pack"
)

// SwiftGuestClassSpec defines the desired state of SwiftGuestClass.
type SwiftGuestClassSpec struct {
	CPU      resource.Quantity `json:"cpu"`
	Memory   resource.Quantity `json:"memory"`
	RootDisk RootDiskSpec      `json:"rootDisk"`
	// Storage is the cluster default for PVCs the SwiftGuest controller
	// creates (today: the root-disk clone PVC). Per-guest overrides on
	// SwiftGuest.spec.storage compose per-field on top of this. Nil/unset
	// keeps the legacy behaviour: ReadWriteOnce + Filesystem, with
	// StorageClassName inherited from the source SwiftImage's PVC.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`
	// CoreScheduling sets the vCPU core-scheduling policy for guests of this
	// class (SMT side-channel mitigation). Default off; use vm for multi-tenant
	// isolation. Empty is treated as off (no change to the CH --cpus args).
	// +kubebuilder:default=off
	// +optional
	CoreScheduling CoreScheduling `json:"coreScheduling,omitempty"`
	// CPUPinning pins guests of this class 1:1 onto host CPUs drawn from the
	// launcher pod's own cpuset. Default none (the host scheduler places
	// vCPUs). Requires at least as many CPUs in that cpuset as the class
	// requests vCPUs; the launcher refuses to start rather than pin partially.
	// +kubebuilder:default=none
	// +optional
	CPUPinning CPUPinning `json:"cpuPinning,omitempty"`
	// SMTPolicy selects sibling placement when cpuPinning is static, and is
	// ignored otherwise. Default spread.
	// +kubebuilder:default=spread
	// +optional
	SMTPolicy SMTPolicy `json:"smtPolicy,omitempty"`
}

// SwiftGuestClass is the Schema for the swiftguestclasses API.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=swiftguestclasses,scope=Cluster,shortName=sgc
// +kubebuilder:printcolumn:name="CPU",type=string,JSONPath=`.spec.cpu`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.spec.memory`
// +kubebuilder:printcolumn:name="AccessMode",type=string,JSONPath=`.spec.storage.accessMode`
// +kubebuilder:printcolumn:name="VolumeMode",type=string,JSONPath=`.spec.storage.volumeMode`
type SwiftGuestClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SwiftGuestClassSpec `json:"spec,omitempty"`
}

// SwiftGuestClassList contains a list of SwiftGuestClass.
// +kubebuilder:object:root=true
type SwiftGuestClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftGuestClass `json:"items"`
}

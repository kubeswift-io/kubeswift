package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// SwiftSandboxSpec defines an ephemeral, strongly-isolated microVM that runs an
// OCI image as its root filesystem (the mode-3 sandbox boot: a direct-kernel
// boot + a read-only OCI rootfs + a tmpfs overlay). See
// docs/sandbox/overview.md.
type SwiftSandboxSpec struct {
	// Image is the OCI image to run as the sandbox root filesystem. A digest
	// reference (repo@sha256:...) is strongly preferred for reproducibility and
	// provenance; a tag is accepted.
	Image string `json:"image"`

	// ImagePullSecret optionally names a docker-registry Secret in the sandbox's
	// namespace for pulling Image from a private registry.
	// +optional
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// VerifyKeySecretRef, when set, names a Secret in the sandbox's namespace
	// holding a cosign public key (key "cosign.pub"). Before materializing the
	// rootfs, sandbox-materialize cosign-verifies Image@digest against that key;
	// a missing or invalid signature fails the materialize step, so the sandbox
	// goes Failed and NEVER boots. Requires a TLS registry (cosign speaks HTTPS
	// only). Mirrors SwiftImage's spec.source.oci.verifyKeySecretRef.
	// +optional
	VerifyKeySecretRef *SecretObjectReference `json:"verifyKeySecretRef,omitempty"`

	// CPU is the number of vCPUs.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	CPU int32 `json:"cpu,omitempty"`

	// Memory is the guest RAM (e.g. "512Mi", "4Gi").
	// +kubebuilder:default="512Mi"
	Memory resource.Quantity `json:"memory"`

	// Command overrides the image's entrypoint. When empty, the image config
	// Entrypoint+Cmd is used.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args are appended to Command (or to the image entrypoint when Command is
	// empty).
	// +optional
	Args []string `json:"args,omitempty"`

	// Env are extra environment variables for the workload, merged over the image
	// config Env.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// WorkingDir overrides the image config working directory.
	// +optional
	WorkingDir string `json:"workingDir,omitempty"`

	// Timeout is the wall-clock run cap. Past startedAt+timeout the controller
	// force-terminates the sandbox to Failed(DeadlineExceeded). Unset = no cap.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// TTL, when set, makes the controller delete this SwiftSandbox once it has
	// been terminal (Completed/Failed) for at least ttl — keeping finished
	// sandboxes from accumulating. Unset = keep until manual deletion.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// Network controls sandbox connectivity. Defaults to restricted.
	// +optional
	Network SandboxNetwork `json:"network,omitempty"`

	// RootfsMode selects how the OCI rootfs is delivered to the guest:
	//   block    (default) — a node-local read-only ext4 image passed as a
	//                         virtio-blk disk; the bridge overlays a tmpfs upper.
	//   virtiofs           — the unpacked rootfs tree shared over virtio-fs
	//                         (tag "sandboxroot"); no ext4 sizing/mkfs, and the
	//                         host page cache is shared. Same RO-base + writable
	//                         tmpfs-overlay semantics as block.
	// +kubebuilder:validation:Enum=block;virtiofs
	// +kubebuilder:default=block
	// +optional
	RootfsMode SandboxRootfsMode `json:"rootfsMode,omitempty"`

	// KernelProfileRef names the SwiftKernel sandbox profile to boot. Defaults to
	// the well-known "sandbox" kernel when unset.
	// +optional
	KernelProfileRef *corev1.LocalObjectReference `json:"kernelProfileRef,omitempty"`

	// NodeSelector constrains the sandbox to matching (kernel) nodes.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// PoolRef, when set, satisfies this sandbox from a warm SwiftSandboxPool of the
	// same image (sub-second checkout: claim a pre-booted slot and inject this
	// sandbox's command/args/env into it over vsock) instead of the cold
	// materialize+boot path. If no warm slot is available the sandbox falls back to
	// the cold path automatically. The pool must be in the same namespace.
	// +optional
	PoolRef *corev1.LocalObjectReference `json:"poolRef,omitempty"`

	// GPUResourceClaim, when set, passes one or more GPUs into the sandbox via a
	// Kubernetes DRA ResourceClaim: the kube-scheduler allocates the device(s) and
	// the KubeSwift DRA driver injects them (CDI GPU_PCI_ADDRESSES), gpu-init binds
	// VFIO, and swiftletd synthesizes the Cloud Hypervisor --device from the env.
	// The guest OCI image ships the NVIDIA driver and loads it, so a GPU sandbox
	// needs the module-capable "gpu-sandbox" kernel profile — the controller selects
	// it automatically when kernelProfileRef is unset.
	//
	// A GPU sandbox boots COLD: a warm pool cannot cheaply hold a scarce GPU idle,
	// so gpuResourceClaim and poolRef are mutually exclusive. Mirrors
	// SwiftGuest.spec.gpuResourceClaim (the DRA allocation backend). The native
	// SwiftGPUProfile backend (gpuProfileRef) is a planned follow-up for sandboxes.
	// +optional
	GPUResourceClaim *swiftv1alpha1.GPUResourceClaimSpec `json:"gpuResourceClaim,omitempty"`
}

// UsesGPU reports whether the sandbox requests a GPU (currently the DRA backend
// only).
func (s *SwiftSandbox) UsesGPU() bool {
	return s.Spec.GPUResourceClaim != nil
}

// SecretObjectReference references a Secret by name (in the object's own
// namespace). Used by spec.verifyKeySecretRef for the cosign public key.
type SecretObjectReference struct {
	// Name of the Secret.
	Name string `json:"name"`
}

// SandboxRootfsMode selects how the OCI rootfs is delivered to the guest.
// +kubebuilder:validation:Enum=block;virtiofs
type SandboxRootfsMode string

const (
	// SandboxRootfsBlock (default) delivers the rootfs as a read-only ext4 disk.
	SandboxRootfsBlock SandboxRootfsMode = "block"
	// SandboxRootfsVirtiofs shares the unpacked rootfs tree over virtio-fs.
	SandboxRootfsVirtiofs SandboxRootfsMode = "virtiofs"
)

// SandboxNetworkMode selects the sandbox connectivity posture.
// +kubebuilder:validation:Enum=restricted;open;none
type SandboxNetworkMode string

const (
	// SandboxNetworkRestricted (the default) attaches the pod network with a
	// deny-ingress posture AND hardened egress: the guest reaches DNS + the public
	// internet but CANNOT reach cluster-internal pods/services or the cloud metadata
	// endpoint (169.254.169.254). The right posture for untrusted code.
	SandboxNetworkRestricted SandboxNetworkMode = "restricted"
	// SandboxNetworkOpen attaches the pod network with deny-ingress but unrestricted
	// egress (the guest can reach the whole cluster + internet). Opt-in for trusted
	// workloads that must talk to in-cluster services; NOT for untrusted code.
	SandboxNetworkOpen SandboxNetworkMode = "open"
	// SandboxNetworkNone attaches no network (detonation / pure compute).
	SandboxNetworkNone SandboxNetworkMode = "none"
)

// SandboxNetwork is the sandbox networking policy.
type SandboxNetwork struct {
	// Mode is "restricted" (default), "open", or "none".
	// +kubebuilder:default=restricted
	// +optional
	Mode SandboxNetworkMode `json:"mode,omitempty"`
}

// SwiftSandboxPhase is the lifecycle phase.
// +kubebuilder:validation:Enum=Pending;Materializing;Running;Completed;Failed
type SwiftSandboxPhase string

const (
	// SwiftSandboxPending — resolving image + kernel profile.
	SwiftSandboxPending SwiftSandboxPhase = "Pending"
	// SwiftSandboxMaterializing — the rootfs init container is producing the ext4.
	SwiftSandboxMaterializing SwiftSandboxPhase = "Materializing"
	// SwiftSandboxRunning — the guest is up.
	SwiftSandboxRunning SwiftSandboxPhase = "Running"
	// SwiftSandboxCompleted — the workload exited 0 (terminal).
	SwiftSandboxCompleted SwiftSandboxPhase = "Completed"
	// SwiftSandboxFailed — boot/materialize failure, non-zero exit, or timeout
	// (terminal).
	SwiftSandboxFailed SwiftSandboxPhase = "Failed"
)

// Condition types.
const (
	SwiftSandboxConditionResolved     = "Resolved"
	SwiftSandboxConditionRootfsReady  = "RootfsReady"
	SwiftSandboxConditionGuestRunning = "GuestRunning"
)

// SandboxRootfsStatus reports the materialized OCI rootfs.
type SandboxRootfsStatus struct {
	// Digest is the resolved image digest (sha256:...).
	// +optional
	Digest string `json:"digest,omitempty"`
	// SizeBytes is the materialized ext4 (or tree) size.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// CachePath is the node-local rootfs artifact path.
	// +optional
	CachePath string `json:"cachePath,omitempty"`
}

// SandboxRuntimeStatus reports the live guest runtime, mapped from the swiftletd
// pod annotations (the same reporting path SwiftGuest uses). Absent until swiftletd
// reaches CH-socket-ready and writes the annotations.
type SandboxRuntimeStatus struct {
	// PID is the host PID of the hypervisor process.
	// +optional
	PID int64 `json:"pid,omitempty"`
	// Hypervisor is the resolved VMM (always cloud-hypervisor for a sandbox).
	// +optional
	Hypervisor string `json:"hypervisor,omitempty"`
}

// SandboxNetworkStatus reports the guest network, mapped from the swiftletd
// lease-poller pod annotation. Absent for network:none sandboxes.
type SandboxNetworkStatus struct {
	// PrimaryIP is the guest's DHCP-assigned IP (network:restricted only).
	// +optional
	PrimaryIP string `json:"primaryIP,omitempty"`
}

// SwiftSandboxStatus is the observed state.
type SwiftSandboxStatus struct {
	// +optional
	Phase SwiftSandboxPhase `json:"phase,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// PodRef is the launcher pod name.
	// +optional
	PodRef string `json:"podRef,omitempty"`
	// +optional
	Rootfs *SandboxRootfsStatus `json:"rootfs,omitempty"`
	// Runtime is the live guest runtime (pid/hypervisor), reported by swiftletd.
	// +optional
	Runtime *SandboxRuntimeStatus `json:"runtime,omitempty"`
	// Network is the guest network (primaryIP), reported by the swiftletd lease
	// poller. Absent for network:none sandboxes.
	// +optional
	Network *SandboxNetworkStatus `json:"network,omitempty"`
	// StartedAt is when the guest began running.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// TerminalAt is when the sandbox first reached a terminal phase
	// (Completed/Failed); the anchor for spec.ttl-driven deletion.
	// +optional
	TerminalAt *metav1.Time `json:"terminalAt,omitempty"`
	// ExitCode is the workload/guest exit code when known.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// SwiftSandbox is an ephemeral OCI-rootfs microVM.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=swiftsandboxes,scope=Namespaced,shortName=sbox
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.network.primaryIP`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftSandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftSandboxSpec   `json:"spec,omitempty"`
	Status            SwiftSandboxStatus `json:"status,omitempty"`
}

// SwiftSandboxList is a list of SwiftSandbox.
// +kubebuilder:object:root=true
type SwiftSandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftSandbox `json:"items"`
}

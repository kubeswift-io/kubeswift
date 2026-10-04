package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	// SwiftGuest.spec.gpuResourceClaim (the DRA allocation backend).
	// +optional
	GPUResourceClaim *swiftv1alpha1.GPUResourceClaimSpec `json:"gpuResourceClaim,omitempty"`

	// GPUProfileRef selects the NATIVE SwiftGPU allocation backend (a
	// SwiftGPUProfile in the same namespace), the sandbox analogue of
	// SwiftGuest.spec.gpuProfileRef. Unlike DRA, the KubeSwift SwiftGPU
	// controller allocates the device(s) at CONTROLLER time and stamps
	// status.gpu; the sandbox pod is then pinned to status.gpu.nodeName and
	// gpu-init binds the specific BDFs from status.gpu.devices. Mutually
	// exclusive with gpuResourceClaim (pick one GPU backend) and — like
	// gpuResourceClaim — with poolRef (a GPU sandbox boots cold). Selecting a
	// GPU still switches the sandbox to the module-capable "gpu-sandbox" kernel
	// unless kernelProfileRef is set.
	// +optional
	GPUProfileRef *corev1.LocalObjectReference `json:"gpuProfileRef,omitempty"`

	// ScratchDisk attaches ONE secondary block disk to the sandbox guest — a
	// large or persistent scratch volume for build caches, dataset staging,
	// checkpoints, or (for GPU inference) a model/weight cache, so the sandbox is
	// not limited to its ephemeral RAM-backed rootfs overlay. The disk is
	// attached as a RAW block device (the same v0.4.2 blank-data-disk runtime
	// path SwiftGuest uses); the workload runs mkfs + mount, or uses it raw.
	// +optional
	ScratchDisk *SandboxScratchDisk `json:"scratchDisk,omitempty"`

	// Model mounts a read-only, pool-shareable model artifact (an OCI image whose
	// filesystem holds the weights) into the sandbox at MountPath over virtio-fs.
	// It is materialized once per node (digest-keyed cache, cosign-verifiable via
	// verifyKeySecretRef) and shared read-only from the host page cache — so the
	// weights are resident before the workload runs. This is the OCI-native
	// alternative to baking the model into the image or mounting it via a
	// scratchDisk PVC: content-addressed, signed, portable, and deduplicated
	// across every sandbox on the node. On a SwiftSandboxPool it makes every warm
	// slot carry the model, so a checkout starts inference sub-second.
	// +optional
	Model *SandboxModel `json:"model,omitempty"`

	// PodMetadata adds labels and annotations to the launcher pod, for example
	// so a Service selects the sandbox. Keys under kubeswift.io or any
	// *.kubeswift.io domain are refused, as are the pod-network annotations
	// KubeSwift wires itself (k8s.v1.cni.cncf.io/, v1.multus-cni.io/,
	// k8s.ovn.org/). A warm-pool checkout applies them to the slot it claims.
	// The launcher pod is privileged: metadata that makes another controller
	// mutate it (a mesh sidecar, for example) is not supported.
	// +optional
	PodMetadata *SandboxPodMetadata `json:"podMetadata,omitempty"`
}

// SandboxScratchDisk describes the sandbox's secondary block disk. Exactly one
// of blank / pvcRef must be set.
type SandboxScratchDisk struct {
	// Blank provisions a new, empty, sized Block PVC OWNED by the sandbox
	// (deleted with it — an ephemeral-but-large, non-RAM scratch). The workload
	// mkfs+mounts the raw device. VolumeMode must be Block (Filesystem is not
	// supported for sandbox scratch disks in v1).
	// +optional
	Blank *swiftv1alpha1.BlankDiskSpec `json:"blank,omitempty"`
	// PVCRef attaches an EXISTING PersistentVolumeClaim (Block volumeMode) as the
	// raw disk. It PERSISTS beyond the sandbox (not owned by it) — the case for a
	// durable cache reused across sandboxes. Exactly one of blank / pvcRef.
	// +optional
	PVCRef *corev1.LocalObjectReference `json:"pvcRef,omitempty"`
}

// SandboxModel is a read-only, node-shared model artifact mounted into a sandbox
// over virtio-fs. It reuses spec.verifyKeySecretRef (and spec.imagePullSecret)
// for cosign verification and private-registry pulls — the model is expected to
// come from the same trust domain as the rootfs image.
type SandboxModel struct {
	// ImageRef is the OCI image whose filesystem holds the model (e.g. weights
	// under the image root, or under MountPath's basename). A digest reference
	// (repo@sha256:...) is strongly preferred: it pins the content and keeps the
	// per-node cache stable across pool churn.
	ImageRef string `json:"imageRef"`

	// MountPath is the read-only in-guest mount point for the model tree. The
	// workload reads its weights from here.
	// +kubebuilder:default=/model
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}

// ModelMountPath returns the effective in-guest model mount point (MountPath,
// defaulting to /model). Callers use it whether or not the CRD default fired.
func (m *SandboxModel) ModelMountPath() string {
	if m != nil && m.MountPath != "" {
		return m.MountPath
	}
	return "/model"
}

// UsesGPU reports whether the sandbox requests a GPU by either backend.
func (s *SwiftSandbox) UsesGPU() bool {
	return s.Spec.GPUResourceClaim != nil || s.Spec.GPUProfileRef != nil
}

// GPUBackend returns the GPU allocation backend the sandbox selects: "native"
// (gpuProfileRef), "dra" (gpuResourceClaim), or "" (no GPU). Mirrors
// SwiftGuest.GPUBackend so the shared gpualloc/swiftgpu seam treats both
// workload kinds uniformly. gpuProfileRef wins if both are set (the webhook
// rejects that combination, so it cannot happen in practice).
func (s *SwiftSandbox) GPUBackend() string {
	switch {
	case s.Spec.GPUProfileRef != nil:
		return swiftv1alpha1.GPUBackendNative
	case s.Spec.GPUResourceClaim != nil:
		return swiftv1alpha1.GPUBackendDRA
	default:
		return ""
	}
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

	// Egress lets a restricted sandbox reach destinations the restricted mode
	// blocks, such as one in-cluster Service, without opening everything.
	// Only valid with mode restricted.
	// +optional
	Egress *SandboxEgress `json:"egress,omitempty"`

	// Ports exposes guest ports. Each is a named containerPort on the launcher
	// pod, forwarded to the same port in the guest, so a Service can target the
	// sandbox by port name. They are the only inbound traffic the sandbox's
	// NetworkPolicy admits. Not valid with mode none.
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	// +optional
	Ports []SandboxPort `json:"ports,omitempty"`

	// Ingress narrows who may reach Ports. Without it, any source may.
	// +optional
	Ingress *SandboxIngress `json:"ingress,omitempty"`
}

// SandboxPort is one exposed guest port.
type SandboxPort struct {
	// Name is an IANA service name (lowercase letters, digits and '-', at most
	// 15 characters, at least one letter), unique within the sandbox. A Service
	// targets it with targetPort: <name>.
	// +kubebuilder:validation:MaxLength=15
	Name string `json:"name"`
	// Port is both the launcher pod's containerPort and the guest port it is
	// forwarded to.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// Protocol is TCP, the only protocol supported.
	// +kubebuilder:validation:Enum=TCP
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// SandboxIngress limits the sources allowed to reach a sandbox's ports.
type SandboxIngress struct {
	// From lists the allowed sources with NetworkPolicy peer semantics
	// (podSelector, namespaceSelector, ipBlock). Empty allows every source.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	From []networkingv1.NetworkPolicyPeer `json:"from,omitempty"`
}

// SandboxPodMetadata is metadata for a sandbox's launcher pod.
type SandboxPodMetadata struct {
	// Labels to add to the launcher pod.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Annotations to add to the launcher pod.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SandboxEgress refines the restricted egress posture.
type SandboxEgress struct {
	// Allow lists destinations the guest may reach in addition to DNS and the
	// public internet. The link-local range 169.254.0.0/16 (the cloud metadata
	// endpoint) stays blocked whatever is allowed, and IPv6 stays blocked.
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	Allow []SandboxEgressRule `json:"allow,omitempty"`
}

// SandboxEgressRule allows one destination: a Service or an IPv4 CIDR,
// optionally narrowed to ports. Exactly one of service and cidr is set.
type SandboxEgressRule struct {
	// Service allows the ClusterIP of a Service. It is resolved when the
	// sandbox's launcher is created (for a pool, each time the pool reconciles:
	// warm slots holding an older address are replaced). A Service that is
	// missing, headless or has no IPv4 ClusterIP keeps the sandbox Pending.
	// +optional
	Service *SandboxEgressService `json:"service,omitempty"`

	// CIDR allows an IPv4 range, e.g. 10.20.0.0/24 or 10.20.0.5/32.
	// +optional
	CIDR string `json:"cidr,omitempty"`

	// Ports narrows the rule. Empty allows every port: for a Service, every
	// port the Service declares; for a CIDR, any port and protocol.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Ports []SandboxEgressPort `json:"ports,omitempty"`
}

// SandboxEgressService names a Service whose ClusterIP the sandbox may reach.
type SandboxEgressService struct {
	// Name of the Service.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Namespace of the Service; defaults to the sandbox's.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SandboxEgressPort is one allowed destination port.
type SandboxEgressPort struct {
	// Port number. For a Service, a port the Service declares.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// Protocol is TCP (default) or UDP.
	// +kubebuilder:validation:Enum=TCP;UDP
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// SandboxEgressAllowed is one rule the launcher enforces, as resolved.
type SandboxEgressAllowed struct {
	// CIDR is the destination, e.g. 10.96.0.12/32 for a Service's ClusterIP.
	CIDR string `json:"cidr"`
	// Protocol and Port narrow it; both empty allow every port.
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
	// +optional
	Port int32 `json:"port,omitempty"`
	// From is the spec entry this came from: "service <namespace>/<name>"
	// or "cidr <cidr>".
	From string `json:"from"`
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
	// SwiftSandboxConditionGPUAllocated is True once the native SwiftGPU backend
	// (spec.gpuProfileRef) has allocated the device(s) and stamped status.gpu;
	// False with reason ProfileNotFound / NoCapacity while it cannot. Absent for
	// the DRA backend and non-GPU sandboxes.
	SwiftSandboxConditionGPUAllocated = "GPUAllocated"
	// SwiftSandboxConditionScratchDiskReady is True once spec.scratchDisk's PVC
	// is Bound and attachable; False while provisioning/binding. Absent when no
	// scratchDisk is requested.
	SwiftSandboxConditionScratchDiskReady = "ScratchDiskReady"
)

// Resolved=False reasons, on a SwiftSandbox or a SwiftSandboxPool, while the
// kernel profile its launcher would boot cannot be booted. No launcher pod is
// created until it can. Neither is terminal: the SwiftKernel may still be
// created, or still be pulling its artifacts to the node.
const (
	// SwiftSandboxReasonKernelNotFound: no SwiftKernel of the kernel profile's
	// name exists in the namespace.
	SwiftSandboxReasonKernelNotFound = "KernelNotFound"
	// SwiftSandboxReasonKernelNotReady: the SwiftKernel exists but is not Ready
	// on the node the launcher is pinned to or, when it is not pinned, overall.
	SwiftSandboxReasonKernelNotReady = "KernelNotReady"
)

// SandboxScratchDiskStatus reports the attached scratch disk.
type SandboxScratchDiskStatus struct {
	// PVCName is the bound PVC (sandbox-owned for blank, the operator's for pvcRef).
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// DevicePath is the launcher-side host device path (/dev/kubeswift-data-scratch).
	// Inside the guest it is a raw virtio-blk device (typically /dev/vdc).
	// +optional
	DevicePath string `json:"devicePath,omitempty"`
	// Bound is true once the PVC is Bound.
	// +optional
	Bound bool `json:"bound,omitempty"`
}

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

// SandboxModelStatus reports the resolved read-only model artifact.
type SandboxModelStatus struct {
	// Digest is the resolved model image digest (sha256:...).
	// +optional
	Digest string `json:"digest,omitempty"`
	// MountPath is the read-only in-guest mount point.
	// +optional
	MountPath string `json:"mountPath,omitempty"`
	// CachePath is the node-local model tree path (virtio-fs source).
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
	// PrimaryIP is the guest's DHCP-assigned IP (absent for network:none).
	// +optional
	PrimaryIP string `json:"primaryIP,omitempty"`
	// PrimaryIPScope says where primaryIP can be reached from, as on a
	// SwiftGuest. A sandbox's guest always sits behind its launcher's nat, so
	// it is Pod whenever primaryIP is set: the address is on the launcher
	// pod's private network, repeats across sandboxes, and is reachable only
	// from inside that launcher pod.
	// +optional
	PrimaryIPScope swiftv1alpha1.PrimaryIPScope `json:"primaryIPScope,omitempty"`
	// PodIP is the IP of the launcher pod running the sandbox. Unlike
	// primaryIP it is unique in the cluster.
	// +optional
	PodIP string `json:"podIP,omitempty"`
	// EgressAllowed is the egress allowlist the launcher enforces, from
	// spec.network.egress.allow with every Service resolved to its address.
	// +optional
	EgressAllowed []SandboxEgressAllowed `json:"egressAllowed,omitempty"`
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
	// GPU is the native SwiftGPU allocation (devices, node, NUMA), populated by
	// the SwiftGPU controller when spec.gpuProfileRef is set. Absent for the DRA
	// backend (the device identity lives in the pod's ResourceClaim) and for
	// non-GPU sandboxes.
	// +optional
	GPU *swiftv1alpha1.GPUStatus `json:"gpu,omitempty"`
	// ScratchDisk reports the attached scratch disk once its PVC is Bound.
	// Absent when spec.scratchDisk is unset.
	// +optional
	ScratchDisk *SandboxScratchDiskStatus `json:"scratchDisk,omitempty"`
	// Model reports the materialized model artifact once resolved. Absent when
	// spec.model is unset.
	// +optional
	Model *SandboxModelStatus `json:"model,omitempty"`
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
// +kubebuilder:printcolumn:name="Guest IP",type=string,JSONPath=`.status.network.primaryIP`
// +kubebuilder:printcolumn:name="Pod IP",type=string,JSONPath=`.status.network.podIP`
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

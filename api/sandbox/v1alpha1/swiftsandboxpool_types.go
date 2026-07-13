package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftSandboxPoolSpec defines a warm buffer of pre-booted, workload-less microVMs of
// one OCI image, kept Ready so a SwiftSandbox that references the pool (via
// spec.poolRef) checks out in sub-second time instead of paying the cold
// materialize+boot. All warm slots of the image on a node share one materialized
// rootfs. Warming is node-local: a checkout that finds no free warm slot on its target
// node degrades to the normal cold path — a miss is never a silent failure.
//
// The fields below are the SLOT SHAPE (a workload-less SwiftSandbox). The workload
// (command/args/env/timeout/ttl) is NOT set here — it belongs to the individual
// SwiftSandbox that checks a slot out and is injected post-boot.
type SwiftSandboxPoolSpec struct {
	// Image is the OCI image every warm slot boots as its root filesystem. A digest
	// reference (repo@sha256:...) is preferred for reproducibility. A SwiftSandbox must
	// request the same image (and slot shape) to claim a slot from this pool.
	Image string `json:"image"`

	// ImagePullSecret optionally names a docker-registry Secret in the pool's namespace
	// for pulling Image from a private registry.
	// +optional
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// VerifyKeySecretRef, when set, names a Secret in the pool's namespace holding a
	// cosign public key (key "cosign.pub"). Every warm slot cosign-verifies Image
	// against it before materializing; an unsigned/tampered image fails the slot so
	// the pool never warms an unverified rootfs. Requires a TLS registry.
	// +optional
	VerifyKeySecretRef *SecretObjectReference `json:"verifyKeySecretRef,omitempty"`

	// CPU is the vCPU count of each warm slot. A claiming SwiftSandbox must match.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	CPU int32 `json:"cpu,omitempty"`

	// Memory is the RAM of each warm slot (e.g. "512Mi"). A claiming SwiftSandbox must
	// match. Note this is held per warm slot, so a pool of N slots holds N*Memory idle.
	// +kubebuilder:default="512Mi"
	Memory resource.Quantity `json:"memory"`

	// Network is the slot networking posture (default restricted), the same modes as
	// SwiftSandbox: restricted | open | none.
	// +optional
	Network SandboxNetwork `json:"network,omitempty"`

	// KernelProfileRef names the SwiftKernel sandbox profile to boot; defaults to the
	// well-known "sandbox" kernel when unset.
	// +optional
	KernelProfileRef *corev1.LocalObjectReference `json:"kernelProfileRef,omitempty"`

	// NodeSelector constrains which (kernel) nodes the pool warms slots on, merged with
	// the required kubeswift.io/kernel-node=true label.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// MinWarm is the desired number of Ready (pre-booted, unclaimed) warm slots the pool
	// keeps. Because warming is node-local, the pool spreads these across the schedulable
	// kernel-nodes in scope; a claim that lands on a node with no free slot falls back to
	// the cold path.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinWarm int32 `json:"minWarm,omitempty"`

	// MaxWarm caps the total warm slots the pool will hold (back-pressure). 0 means no
	// cap beyond minWarm.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxWarm int32 `json:"maxWarm,omitempty"`

	// IdleTTL, when set, lets the pool scale unclaimed slots back toward zero after they
	// have sat idle at least this long — so a quiet pool stops holding RAM. Unset = hold
	// minWarm indefinitely.
	// +optional
	IdleTTL *metav1.Duration `json:"idleTTL,omitempty"`
}

// SwiftSandboxPoolPhase is the pool lifecycle phase.
// +kubebuilder:validation:Enum=Pending;Warming;Ready;Degraded
type SwiftSandboxPoolPhase string

const (
	// SwiftSandboxPoolPending — resolving the image + kernel profile.
	SwiftSandboxPoolPending SwiftSandboxPoolPhase = "Pending"
	// SwiftSandboxPoolWarming — bringing warm slots up toward minWarm.
	SwiftSandboxPoolWarming SwiftSandboxPoolPhase = "Warming"
	// SwiftSandboxPoolReady — the warm buffer is at target.
	SwiftSandboxPoolReady SwiftSandboxPoolPhase = "Ready"
	// SwiftSandboxPoolDegraded — the pool cannot reach minWarm (e.g. no schedulable
	// kernel-node, image pull failing).
	SwiftSandboxPoolDegraded SwiftSandboxPoolPhase = "Degraded"
)

// Pool condition types.
const (
	SwiftSandboxPoolConditionResolved = "Resolved"
	SwiftSandboxPoolConditionWarm     = "Warm"
)

// SwiftSandboxPoolStatus is the observed state.
type SwiftSandboxPoolStatus struct {
	// +optional
	Phase SwiftSandboxPoolPhase `json:"phase,omitempty"`
	// WarmReplicas is the number of Ready, pre-booted, unclaimed slots.
	// +optional
	WarmReplicas int32 `json:"warmReplicas,omitempty"`
	// ClaimedReplicas is the number of slots currently checked out to a SwiftSandbox.
	// +optional
	ClaimedReplicas int32 `json:"claimedReplicas,omitempty"`
	// Rootfs reports the resolved+materialized image shared by every slot.
	// +optional
	Rootfs *SandboxRootfsStatus `json:"rootfs,omitempty"`
	// ImageEnv is the pool image's config env ("KEY=VAL"), resolved once at
	// materialize. A checkout merges its SwiftSandbox.spec.env over this so the
	// injected workload sees the image env too — parity with a cold sandbox,
	// without a per-checkout registry pull.
	// +optional
	ImageEnv []string `json:"imageEnv,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the pool generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Selector is the pool's warm-slot label selector serialized as a string.
	// Required by the scale subresource so `kubectl scale` and an HPA can target
	// the pool; it selects the warm-slot pods (sandbox.kubeswift.io/pool=<name>).
	// +optional
	Selector string `json:"selector,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// SwiftSandboxPool maintains a warm buffer of pre-booted sandbox microVMs for
// sub-second SwiftSandbox checkout. See docs/sandbox/overview.md.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=swiftsandboxpools,scope=Namespaced,shortName=sboxpool
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.minWarm,statuspath=.status.warmReplicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="MinWarm",type=integer,JSONPath=`.spec.minWarm`
// +kubebuilder:printcolumn:name="Warm",type=integer,JSONPath=`.status.warmReplicas`
// +kubebuilder:printcolumn:name="Claimed",type=integer,JSONPath=`.status.claimedReplicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftSandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftSandboxPoolSpec   `json:"spec,omitempty"`
	Status            SwiftSandboxPoolStatus `json:"status,omitempty"`
}

// SwiftSandboxPoolList is a list of SwiftSandboxPool.
// +kubebuilder:object:root=true
type SwiftSandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftSandboxPool `json:"items"`
}

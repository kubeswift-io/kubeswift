package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftArtifactPlacementSpec names OCI artifacts to prepare in the node cache
// of every node that hosts a slot of a SwiftSandboxPool, so a checkout that
// mounts one does not fetch it first. It changes nothing in the pool or its
// slots: the slots keep running, and a checkout still names its artifacts in
// its own spec.artifacts, with its own credentials and key.
type SwiftArtifactPlacementSpec struct {
	// PoolRef names the SwiftSandboxPool, in this namespace, whose slots'
	// nodes receive the artifacts. A node is a target while it runs at least
	// one of the pool's slots.
	PoolRef corev1.LocalObjectReference `json:"poolRef"`

	// Artifacts to prepare, each pinned by digest.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Artifacts []PlacementArtifact `json:"artifacts"`
}

// PlacementArtifact is one artifact to prepare. A checkout uses the prepared
// entry when its own spec.artifacts names the same repository and digest,
// layout and verification key.
type PlacementArtifact struct {
	// Name identifies the artifact within the placement: a DNS label of at
	// most 40 characters.
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Ref is the artifact's reference, pinned by digest (repo@sha256:...).
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[a-f0-9]{64}$`
	Ref string `json:"ref"`
	// Layout is oci (default) or unpacked, as in a SwiftSandbox artifact:
	// the two are separate entries in the node cache.
	// +kubebuilder:validation:Enum=oci;unpacked
	// +kubebuilder:default=oci
	// +optional
	Layout string `json:"layout,omitempty"`
	// PullSecretRef names a docker-registry Secret in this namespace for the
	// artifact's registry. The registry must authorize these credentials,
	// as it must a checkout's own.
	// +optional
	PullSecretRef *corev1.LocalObjectReference `json:"pullSecretRef,omitempty"`
	// VerifyKeySecretRef names a Secret holding a cosign public key
	// (cosign.pub). The artifact is verified with it before it is cached; a
	// checkout that verifies with the same key then needs no fetch.
	// +optional
	VerifyKeySecretRef *SecretObjectReference `json:"verifyKeySecretRef,omitempty"`
	// Claims and RequirePlatformVerification are as in a SwiftSandbox
	// artifact: a checkout uses the prepared entry only when its own
	// artifact asks for the same claims.
	// +kubebuilder:validation:MaxProperties=8
	// +optional
	Claims map[string]string `json:"claims,omitempty"`
	// +optional
	RequirePlatformVerification bool `json:"requirePlatformVerification,omitempty"`
}

// ArtifactLayout is the layout, defaulted.
func (a *PlacementArtifact) ArtifactLayout() string {
	if a.Layout == "" {
		return "oci"
	}
	return a.Layout
}

// SwiftArtifactPlacementStatus reports where the artifacts are prepared.
type SwiftArtifactPlacementStatus struct {
	// ObservedGeneration is the spec generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// TargetNodes is how many nodes host a slot of the pool.
	// +optional
	TargetNodes int32 `json:"targetNodes"`
	// PreparedNodes is how many of them hold every artifact.
	// +optional
	PreparedNodes int32 `json:"preparedNodes"`
	// Nodes is the state of each target node.
	// +listType=map
	// +listMapKey=name
	// +optional
	Nodes []PlacementNodeStatus `json:"nodes,omitempty"`
	// Artifacts is the state of each artifact.
	// +listType=map
	// +listMapKey=name
	// +optional
	Artifacts []PlacementArtifactStatus `json:"artifacts,omitempty"`
	// Conditions: Prepared is True when every target node holds every
	// artifact, False while any is not (reason Preparing, PrepareFailed,
	// ArtifactRefused, PoolNotFound or NoTargetNodes).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PlacementNodeStatus is one target node.
type PlacementNodeStatus struct {
	// Name is the node.
	Name string `json:"name"`
	// Prepared is true when the node's cache held every artifact of the
	// observed generation, verified and recorded as read from its
	// repository, the last time it was checked (PreparedAt).
	Prepared bool `json:"prepared"`
	// ObservedGeneration is the spec generation Prepared refers to.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// PreparedAt is when the node was last found prepared.
	// +optional
	PreparedAt *metav1.Time `json:"preparedAt,omitempty"`
	// Reason is Prepared, Preparing, VerificationFailed, ClaimMismatch or
	// PrepareFailed.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message says what failed, when something did.
	// +optional
	Message string `json:"message,omitempty"`
	// LastFailureTime is when preparing last failed; the next attempt waits
	// out a backoff from it.
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`
	// Failures counts failed attempts since the node was last prepared; the
	// backoff doubles with each, from 30s to 10m.
	// +optional
	Failures int32 `json:"failures,omitempty"`
	// Requirements identifies the platform verification the node was
	// prepared under (a hash of every artifact's governing policies, keys and
	// claims). When a policy change alters it, the node is prepared again.
	// +optional
	Requirements string `json:"requirements,omitempty"`
}

// PlacementArtifactStatus is one artifact.
type PlacementArtifactStatus struct {
	// Name is the artifact's name in spec.artifacts.
	Name string `json:"name"`
	// Digest is the artifact's digest, once the registry has authorized it.
	// +optional
	Digest string `json:"digest,omitempty"`
	// PreparedNodes is how many target nodes hold it.
	// +optional
	PreparedNodes int32 `json:"preparedNodes"`
	// Reason is Authorized, Resolving, ArtifactRefused or PolicyDenied.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message says why the registry or a policy refused it.
	// +optional
	Message string `json:"message,omitempty"`
	// Policies are the SwiftArtifactVerificationPolicies that govern it.
	// +listType=map
	// +listMapKey=name
	// +optional
	Policies []ArtifactPolicyReference `json:"policies,omitempty"`
}

// Condition and reasons of a SwiftArtifactPlacement.
const (
	PlacementConditionPrepared = "Prepared"

	PlacementReasonPrepared           = "Prepared"
	PlacementReasonPreparing          = "Preparing"
	PlacementReasonPrepareFailed      = "PrepareFailed"
	PlacementReasonVerificationFailed = "VerificationFailed"
	PlacementReasonArtifactRefused    = "ArtifactRefused"
	PlacementReasonPoolNotFound       = "PoolNotFound"
	PlacementReasonNoTargetNodes      = "NoTargetNodes"
	PlacementReasonAuthorized         = "Authorized"
	PlacementReasonResolving          = "Resolving"
)

// SwiftArtifactPlacement prepares OCI artifacts on the nodes of a
// SwiftSandboxPool before a checkout asks for them. See
// docs/sandbox/warm-pool.md.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=swiftartifactplacements,scope=Namespaced,shortName=sap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
// +kubebuilder:printcolumn:name="Prepared",type=integer,JSONPath=`.status.preparedNodes`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.targetNodes`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Prepared")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftArtifactPlacement struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftArtifactPlacementSpec   `json:"spec,omitempty"`
	Status            SwiftArtifactPlacementStatus `json:"status,omitempty"`
}

// SwiftArtifactPlacementList is a list of SwiftArtifactPlacement.
// +kubebuilder:object:root=true
type SwiftArtifactPlacementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftArtifactPlacement `json:"items"`
}

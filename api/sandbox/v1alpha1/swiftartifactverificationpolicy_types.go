package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftArtifactVerificationPolicySpec is a platform rule for the OCI
// artifacts sandboxes and placements mount: in the namespaces it selects,
// every artifact from a repository it governs must carry a signature by one
// of its authorities, with its claims, before it is materialized. The
// workload cannot choose, skip or weaken it: which policies apply is decided
// by the namespace's labels and the artifact's repository, never by a field
// of the workload. Every policy that governs an artifact must be satisfied,
// and so must the artifact's own verifyKeySecretRef. See
// docs/sandbox/artifact-verification-policy.md.
// +kubebuilder:validation:XValidation:rule="self.authorities.all(a, self.authorities.exists_one(b, b.name == a.name))",message="authority names must be unique"
// +kubebuilder:validation:XValidation:rule="!has(self.claims) || self.claims.all(c, self.claims.exists_one(d, d.key == c.key))",message="claim keys must be unique"
type SwiftArtifactVerificationPolicySpec struct {
	// NamespaceSelector selects the namespaces the policy applies in. {}
	// selects every namespace. Select on labels only administrators can set
	// (kubernetes.io/metadata.name always is).
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// Repositories the policy governs, as globs over the canonical
	// repository (registry host and path, for example
	// registry.example.com/caches/app): * matches within one path segment,
	// ** across segments. Docker Hub repositories are index.docker.io/....
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9*][a-z0-9._:*-]*(/[a-z0-9*][a-z0-9._*-]*)*$`
	Repositories []string `json:"repositories"`

	// OtherRepositories decides what happens, in the selected namespaces, to
	// an artifact that no policy governs: Allow (default) mounts it as
	// before, with only the workload's own verification; Deny refuses it.
	// +kubebuilder:validation:Enum=Allow;Deny
	// +kubebuilder:default=Allow
	// +optional
	OtherRepositories string `json:"otherRepositories,omitempty"`

	// DigestPinning is Required (default): a governed artifact must be
	// referenced by digest (repo@sha256:...), so the artifact a workload
	// names cannot change after it is reviewed; a tag is refused. Optional
	// allows tags, which are resolved to a digest and verified there.
	// +kubebuilder:validation:Enum=Required;Optional
	// +kubebuilder:default=Required
	// +optional
	DigestPinning string `json:"digestPinning,omitempty"`

	// Authorities that may sign governed artifacts: a signature by any one of
	// them, carrying every claim, satisfies the policy. Add the new key
	// before removing the old one to rotate without a flag day.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Authorities []ArtifactAuthority `json:"authorities"`

	// Claims the signature must carry: signed annotations (cosign sign -a
	// key=value), read from the same signature the authority verified. Their
	// meaning is the signer's; KubeSwift only compares strings.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Claims []ArtifactClaimRequirement `json:"claims,omitempty"`
}

// ArtifactAuthority is a signer the policy trusts. Exactly one kind is set;
// key is the only kind in v1alpha1.
// +kubebuilder:validation:XValidation:rule="has(self.key)",message="key is required"
type ArtifactAuthority struct {
	// Name identifies the authority in status and Events.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Key is a public key signature (cosign sign --key).
	// +optional
	Key *KeyAuthority `json:"key,omitempty"`
}

// KeyAuthority is a public key.
type KeyAuthority struct {
	// PublicKey is one PEM "PUBLIC KEY" block: ECDSA P-256 or P-384, Ed25519,
	// or RSA of at least 2048 bits. Never a private key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	PublicKey string `json:"publicKey"`
}

// ArtifactClaimRequirement is one signed annotation the signature must carry.
// Exactly one of value, values, fromNamespace and fromArtifact is set.
// +kubebuilder:validation:XValidation:rule="(has(self.value) ? 1 : 0) + (has(self.values) ? 1 : 0) + (has(self.fromNamespace) && self.fromNamespace ? 1 : 0) + (has(self.fromArtifact) ? 1 : 0) == 1",message="set exactly one of value, values, fromNamespace, fromArtifact"
type ArtifactClaimRequirement struct {
	// Key is the annotation's key.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._/-]*$`
	Key string `json:"key"`
	// Value is the only accepted value.
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._:/@+-]+$`
	// +optional
	Value *string `json:"value,omitempty"`
	// Values are the accepted values (any one).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9._:/@+-]+$`
	// +optional
	Values []string `json:"values,omitempty"`
	// FromNamespace: the value must be the name of the namespace the artifact
	// is mounted in, binding a signed artifact to its consumer.
	// +optional
	FromNamespace bool `json:"fromNamespace,omitempty"`
	// FromArtifact names the entry of the artifact's claims (in the
	// SwiftSandbox or SwiftArtifactPlacement) that holds the expected value:
	// the workload says which signed artifact it means, and the signature
	// must agree. An artifact that does not supply it is refused.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._/-]*$`
	// +optional
	FromArtifact *string `json:"fromArtifact,omitempty"`
}

// OtherRepositories values.
const (
	OtherRepositoriesAllow = "Allow"
	OtherRepositoriesDeny  = "Deny"
)

// DigestPinning values.
const (
	DigestPinningRequired = "Required"
	DigestPinningOptional = "Optional"
)

// AllowsOtherRepositories reports whether ungoverned artifacts may mount in
// the namespaces the policy selects.
func (s *SwiftArtifactVerificationPolicySpec) AllowsOtherRepositories() bool {
	return s.OtherRepositories != OtherRepositoriesDeny
}

// RequiresDigest reports whether governed artifacts must be digest-pinned.
func (s *SwiftArtifactVerificationPolicySpec) RequiresDigest() bool {
	return s.DigestPinning != DigestPinningOptional
}

// SwiftArtifactVerificationPolicyStatus reports whether the policy can be
// enforced.
type SwiftArtifactVerificationPolicyStatus struct {
	// ObservedGeneration is the spec generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Authorities lists each authority's key identity.
	// +optional
	Authorities []ArtifactAuthorityStatus `json:"authorities,omitempty"`
	// Namespaces is how many namespaces the selector matches.
	// +optional
	Namespaces int32 `json:"namespaces"`
	// Conditions: Ready is True when the policy is valid. While it is not,
	// every artifact it governs is refused (PolicyInvalid), never mounted
	// unverified.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ArtifactAuthorityStatus identifies an authority's key.
type ArtifactAuthorityStatus struct {
	// Name is the authority's name.
	Name string `json:"name"`
	// KeyID is the sha256 of the key's SubjectPublicKeyInfo (DER), hex.
	// +optional
	KeyID string `json:"keyID,omitempty"`
	// Algorithm is ECDSA-P256, ECDSA-P384, Ed25519 or RSA-<bits>.
	// +optional
	Algorithm string `json:"algorithm,omitempty"`
}

// Condition and reasons of a SwiftArtifactVerificationPolicy.
const (
	VerificationPolicyConditionReady = "Ready"

	VerificationPolicyReasonValid      = "Valid"
	VerificationPolicyReasonInvalidKey = "InvalidKey"
	VerificationPolicyReasonInvalid    = "Invalid"
)

// SwiftArtifactVerificationPolicy is a cluster-wide, administrator-owned rule
// for verifying the OCI artifacts sandboxes mount. See
// docs/sandbox/artifact-verification-policy.md.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=swiftartifactverificationpolicies,scope=Cluster,shortName=savp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Namespaces",type=integer,JSONPath=`.status.namespaces`
// +kubebuilder:printcolumn:name="Other",type=string,JSONPath=`.spec.otherRepositories`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftArtifactVerificationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftArtifactVerificationPolicySpec   `json:"spec,omitempty"`
	Status            SwiftArtifactVerificationPolicyStatus `json:"status,omitempty"`
}

// SwiftArtifactVerificationPolicyList is a list of
// SwiftArtifactVerificationPolicy.
// +kubebuilder:object:root=true
type SwiftArtifactVerificationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftArtifactVerificationPolicy `json:"items"`
}

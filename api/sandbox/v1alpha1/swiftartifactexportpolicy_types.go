package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftArtifactExportPolicySpec is a platform rule for SwiftSandbox outputs:
// which namespaces may export, to which repositories, and with which push
// credential. A workload never names a credential; it can only name a
// repository, and a repository is used only if exactly one policy that selects
// the sandbox's namespace allows it (or several that use the same credential).
// No policy: outputs are refused. See docs/sandbox/outputs.md.
type SwiftArtifactExportPolicySpec struct {
	// NamespaceSelector selects the namespaces that may export under this
	// policy. Select on labels only administrators can set
	// (kubernetes.io/metadata.name always is).
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// Repositories a selected namespace may push to, as globs over the
	// canonical repository (registry host and path): * matches within one
	// path segment, ** across segments. The host is literal. A path segment
	// that is exactly {namespace} matches the sandbox's namespace, so one
	// policy can give every namespace its own path.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=255
	Repositories []string `json:"repositories"`

	// DefaultRepository is where an output without repository goes, followed
	// by /<output name>: a literal repository (no globs) that may contain a
	// {namespace} segment. It must itself be allowed by repositories.
	// +kubebuilder:validation:MaxLength=200
	// +optional
	DefaultRepository string `json:"defaultRepository,omitempty"`

	// CredentialsSecretRef names a kubernetes.io/dockerconfigjson Secret in
	// the KubeSwift namespace used to push. Only the export Job mounts it;
	// tenants, launchers and guests never see it. The Secret must carry the
	// label kubeswift.io/export-credentials=true, so a policy can only name a
	// Secret an administrator set aside for this.
	CredentialsSecretRef ExportCredentialsReference `json:"credentialsSecretRef"`

	// MaxSize is the default and ceiling of an output's maxSize (bytes of
	// files). Default 1Gi, at most 10Gi.
	// +optional
	MaxSize *resource.Quantity `json:"maxSize,omitempty"`

	// MaxEntries is the default and ceiling of an output's maxEntries (files
	// and directories). Default 100000, at most 1048576.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1048576
	// +optional
	MaxEntries *int64 `json:"maxEntries,omitempty"`

	// UploadTimeout bounds one output's push. Default 10m, at most 1h.
	// +optional
	UploadTimeout *metav1.Duration `json:"uploadTimeout,omitempty"`
}

// ExportCredentialsReference names a Secret in the KubeSwift namespace.
type ExportCredentialsReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ExportCredentialsLabel must be "true" on a Secret an export policy names.
const ExportCredentialsLabel = "kubeswift.io/export-credentials"

// The export policy's Ready condition and its reasons.
const (
	ExportPolicyConditionReady = "Ready"
	ExportPolicyReasonValid    = "Valid"
	// ExportPolicyReasonInvalid: the spec does not compile; namespaces it
	// selects have their outputs refused.
	ExportPolicyReasonInvalid = "Invalid"
	// ExportPolicyReasonCredentialsMissing: the credential Secret is absent
	// from the KubeSwift namespace, not labelled for exports, or not a
	// docker config.
	ExportPolicyReasonCredentialsMissing = "CredentialsMissing"
)

// SwiftArtifactExportPolicyStatus reports whether the policy is usable.
type SwiftArtifactExportPolicyStatus struct {
	// ObservedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Namespaces is how many namespaces the policy selects.
	// +optional
	Namespaces int32 `json:"namespaces,omitempty"`
	// Conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SwiftArtifactExportPolicy is a cluster-wide, administrator-owned rule for
// where SwiftSandbox outputs may be pushed and with which credential. See
// docs/sandbox/outputs.md.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=swiftartifactexportpolicies,scope=Cluster,shortName=saep
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Namespaces",type=integer,JSONPath=`.status.namespaces`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftArtifactExportPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftArtifactExportPolicySpec   `json:"spec,omitempty"`
	Status            SwiftArtifactExportPolicyStatus `json:"status,omitempty"`
}

// SwiftArtifactExportPolicyList is a list of SwiftArtifactExportPolicy.
// +kubebuilder:object:root=true
type SwiftArtifactExportPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftArtifactExportPolicy `json:"items"`
}

package v1alpha1

import (
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SandboxOutput is a directory the workload writes and KubeSwift exports, after
// the workload exits 0, as an OCI artifact: pushed with platform credentials
// (the guest never sees them) to a repository a SwiftArtifactExportPolicy
// allows, and reported by digest in status.outputs. Export creates an immutable
// artifact; it does not make its content trusted. See docs/sandbox/outputs.md.
type SandboxOutput struct {
	// Name identifies the output: a DNS label of at most 40 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	Name string `json:"name"`

	// Path is the guest directory exported, absolute, of letters, digits and
	// . _ - /. It must not be /, a directory the guest itself owns (/proc,
	// /sys, /dev, ...), or overlap another output, an artifact mountPath or
	// the model. The workload finds it empty and writable.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=255
	Path string `json:"path"`

	// Repository receives the artifact: registry host and path, no tag or
	// digest. Unset: the selecting policy's defaultRepository followed by
	// /<name>. Either way a SwiftArtifactExportPolicy must allow it.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Repository string `json:"repository,omitempty"`

	// MaxSize caps the bytes of the exported files (sum of regular file
	// sizes). Default and ceiling: the policy's maxSize.
	// +optional
	MaxSize *resource.Quantity `json:"maxSize,omitempty"`

	// MaxEntries caps the files and directories exported. Default and
	// ceiling: the policy's maxEntries.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1048576
	// +optional
	MaxEntries *int64 `json:"maxEntries,omitempty"`

	// ArtifactType is the manifest's artifactType (a media type). Default
	// application/vnd.kubeswift.sandbox-output.v1.
	// +kubebuilder:validation:MaxLength=127
	// +optional
	ArtifactType string `json:"artifactType,omitempty"`

	// Annotations copied onto the manifest. They are the workload author's
	// statements, not facts KubeSwift checked: a signer must not trust them.
	// Keys under org.opencontainers.image. and io.kubeswift. are reserved.
	// +kubebuilder:validation:MaxProperties=8
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// DefaultOutputArtifactType is SandboxOutput.ArtifactType when unset.
const DefaultOutputArtifactType = "application/vnd.kubeswift.sandbox-output.v1"

// Output export limits.
const (
	// MaxOutputs is the most outputs a sandbox declares.
	MaxOutputs = 4
	// MaxOutputEntries is the ceiling of maxEntries (what a consumer of the
	// artifact accepts).
	MaxOutputEntries int64 = 1 << 20
	// DefaultOutputMaxEntries is a policy's maxEntries when unset.
	DefaultOutputMaxEntries int64 = 100000
	// MaxOutputUploadTimeout is the longest uploadTimeout accepted.
	MaxOutputUploadTimeout = time.Hour
	// DefaultOutputUploadTimeout is a policy's uploadTimeout when unset.
	DefaultOutputUploadTimeout = 10 * time.Minute
)

var (
	// MaxOutputSize is the ceiling of maxSize.
	MaxOutputSize = resource.MustParse("10Gi")
	// DefaultOutputMaxSize is a policy's maxSize when unset.
	DefaultOutputMaxSize = resource.MustParse("1Gi")
)

// SandboxOutputState is where an output is in its export.
// +kubebuilder:validation:Enum=Pending;Collecting;Packaged;Uploading;Exported;Skipped;Failed
type SandboxOutputState string

const (
	// SandboxOutputPending: declared; the workload has not finished.
	SandboxOutputPending SandboxOutputState = "Pending"
	// SandboxOutputCollecting: the workload exited 0 and the microVM is
	// gone; the launcher is reading and packaging the directory.
	SandboxOutputCollecting SandboxOutputState = "Collecting"
	// SandboxOutputPackaged: validated and packaged on the node, waiting to
	// be pushed.
	SandboxOutputPackaged SandboxOutputState = "Packaged"
	// SandboxOutputUploading: being pushed.
	SandboxOutputUploading SandboxOutputState = "Uploading"
	// SandboxOutputExported: pushed; digest and reference are set (terminal).
	SandboxOutputExported SandboxOutputState = "Exported"
	// SandboxOutputSkipped: not exported because the workload did not succeed
	// (terminal).
	SandboxOutputSkipped SandboxOutputState = "Skipped"
	// SandboxOutputFailed: the export failed; reason says why (terminal).
	SandboxOutputFailed SandboxOutputState = "Failed"
)

// SandboxOutputStatus reports one spec.outputs entry.
type SandboxOutputStatus struct {
	Name  string             `json:"name"`
	State SandboxOutputState `json:"state"`
	// Reason is a CamelCase word when Skipped or Failed.
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Repository the output is pushed to, fixed when the sandbox is admitted.
	// +optional
	Repository string `json:"repository,omitempty"`
	// Reference is repository@digest once Exported.
	// +optional
	Reference string `json:"reference,omitempty"`
	// Digest is the manifest digest: the artifact's immutable identity.
	// +optional
	Digest string `json:"digest,omitempty"`
	// ContentDigest is the sha256 of the uncompressed archive of the
	// directory; the same files give the same content digest.
	// +optional
	ContentDigest string `json:"contentDigest,omitempty"`
	// Size is the bytes of the pushed layer.
	// +optional
	Size int64 `json:"size,omitempty"`
	// ContentBytes is the sum of the exported files' sizes.
	// +optional
	ContentBytes int64 `json:"contentBytes,omitempty"`
	// Entries is the files and directories exported.
	// +optional
	Entries int64 `json:"entries,omitempty"`
	// Policy is the SwiftArtifactExportPolicy that allowed the destination.
	// +optional
	Policy *ExportPolicyReference `json:"policy,omitempty"`
	// PackagedAt is when the launcher finished packaging.
	// +optional
	PackagedAt *metav1.Time `json:"packagedAt,omitempty"`
	// ExportedAt is when the push completed.
	// +optional
	ExportedAt *metav1.Time `json:"exportedAt,omitempty"`
}

// ExportPolicyReference names the policy an export used, by UID, so a policy
// deleted and recreated under the same name is not taken for it.
type ExportPolicyReference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// Output reasons (status.outputs[].reason and the sandbox reason).
const (
	// OutputReasonNotSupported: this KubeSwift does not export outputs yet.
	OutputReasonNotSupported = "OutputsNotSupported"
	// OutputReasonExportFailed: a required output failed (sandbox reason).
	OutputReasonExportFailed = "OutputExportFailed"
	// OutputReasonWorkloadFailed: the workload did not exit 0; nothing exported.
	OutputReasonWorkloadFailed = "WorkloadFailed"
	// OutputReasonDestinationDenied: no export policy allows the repository.
	OutputReasonDestinationDenied = "OutputDestinationDenied"
	// OutputReasonDestinationAmbiguous: policies with different credentials
	// allow the repository.
	OutputReasonDestinationAmbiguous = "OutputDestinationAmbiguous"
)

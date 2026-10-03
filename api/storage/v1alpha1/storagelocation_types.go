package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultCredentialsSecretName is the kubernetes.io/dockerconfigjson Secret a
// namespace provides for a location that names none.
const DefaultCredentialsSecretName = "kubeswift-registry"

// Condition types on a storage location.
const (
	// ConditionValid is True when the spec is well formed: the repository
	// parses, and the CA bundle and verify key are valid PEM.
	ConditionValid = "Valid"
	// ConditionReady is True when the location is Valid and, if it is a
	// default, the only default at its level.
	ConditionReady = "Ready"
	// ConditionReachable reports whether the registry answered. Set on a
	// SwiftClusterStorageLocation only: the controller does not probe hosts
	// a namespace chose.
	ConditionReachable = "Reachable"
)

// StorageLocationSpec is the spec SwiftStorageLocation and
// SwiftClusterStorageLocation share.
// +kubebuilder:validation:XValidation:rule="has(self.oci) || has(self.csi)",message="a location sets oci, csi, or both"
type StorageLocationSpec struct {
	// Default makes this the location used by objects that name none: the
	// cluster's for a SwiftClusterStorageLocation, the namespace's for a
	// SwiftStorageLocation (which wins over the cluster's). Two defaults at
	// the same level are refused, never one picked.
	// +optional
	Default bool `json:"default,omitempty"`

	// OCI is a registry that snapshots (and later images and golden images)
	// are pushed to and pulled from.
	// +optional
	OCI *OCILocation `json:"oci,omitempty"`

	// CSI sets the VolumeSnapshotClass a csi-volume-snapshot SwiftSnapshot
	// that names none uses, instead of the cluster-wide default class.
	// +optional
	CSI *CSILocation `json:"csi,omitempty"`
}

// OCILocation is a registry repository KubeSwift pushes artifacts below.
// +kubebuilder:validation:XValidation:rule="!(has(self.anonymous) && self.anonymous && has(self.credentialsSecretName) && size(self.credentialsSecretName) > 0)",message="anonymous and credentialsSecretName are mutually exclusive"
type OCILocation struct {
	// Repository is the repository prefix, without a tag or digest, e.g.
	// registry.example.com/kubeswift. Artifacts go below it: a cluster
	// location adds <namespace>/, then snapshots/ or images/<image>.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Repository string `json:"repository"`

	// Insecure allows a plaintext (http) registry. UNSAFE: credentials and
	// artifacts cross the network unencrypted. For an in-cluster or test
	// registry on a trusted network only.
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// CABundle is PEM-encoded CA certificates to trust for this registry, in
	// addition to the system roots, for a registry behind a private CA.
	// +optional
	CABundle string `json:"caBundle,omitempty"`

	// CredentialsSecretName names the kubernetes.io/dockerconfigjson Secret
	// holding this registry's credentials. It is looked up in the namespace
	// of each object that uses the location, never in another; an object
	// whose namespace lacks it waits, naming it. Empty means
	// "kubeswift-registry".
	// +kubebuilder:validation:MaxLength=253
	// +optional
	CredentialsSecretName string `json:"credentialsSecretName,omitempty"`

	// Anonymous says the registry needs no credentials: no Secret is looked
	// up.
	// +optional
	Anonymous bool `json:"anonymous,omitempty"`

	// SigningKeySecretName, when set, signs every artifact pushed through
	// this location with the cosign key pair in the Secret of that name
	// (keys cosign.key and cosign.password), looked up in the namespace of
	// each object that uses the location.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SigningKeySecretName string `json:"signingKeySecretName,omitempty"`

	// VerifyKey is a PEM-encoded cosign public key. Artifacts pulled through
	// this location must carry a valid signature for it.
	// +optional
	VerifyKey string `json:"verifyKey,omitempty"`
}

// Kinds a StorageLocationRef can name.
const (
	KindSwiftStorageLocation        = "SwiftStorageLocation"
	KindSwiftClusterStorageLocation = "SwiftClusterStorageLocation"
)

// StorageLocationRef names a storage location. A SwiftStorageLocation is
// looked up in the referencing object's namespace; there is no namespace
// field.
type StorageLocationRef struct {
	// Kind is SwiftStorageLocation (the default) or
	// SwiftClusterStorageLocation.
	// +kubebuilder:validation:Enum=SwiftStorageLocation;SwiftClusterStorageLocation
	// +kubebuilder:default=SwiftStorageLocation
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name is the location's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// CSILocation configures csi-volume-snapshot snapshots.
type CSILocation struct {
	// VolumeSnapshotClassName is the class a csi-volume-snapshot SwiftSnapshot
	// that names none uses.
	// +kubebuilder:validation:MinLength=1
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName"`
}

// StorageLocationStatus reports whether a location can be used.
type StorageLocationStatus struct {
	// ObservedGeneration is the spec generation the conditions describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Valid, Ready, and, for a SwiftClusterStorageLocation,
	// Reachable.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SwiftClusterStorageLocation is a cluster-wide storage location, managed by
// the cluster admin. One may be the cluster default.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftclusterstoragelocations,scope=Cluster,shortName=csloc
// +kubebuilder:printcolumn:name="Default",type=boolean,JSONPath=`.spec.default`
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.oci.repository`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=="Reachable")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftClusterStorageLocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageLocationSpec   `json:"spec"`
	Status StorageLocationStatus `json:"status,omitempty"`
}

// SwiftClusterStorageLocationList contains a list of SwiftClusterStorageLocation.
// +kubebuilder:object:root=true
type SwiftClusterStorageLocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftClusterStorageLocation `json:"items"`
}

// SwiftStorageLocation is a namespace's own storage location, managed by the
// namespace's users and usable only by objects in that namespace. One may be
// the namespace default, which wins over the cluster default.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftstoragelocations,scope=Namespaced,shortName=sloc
// +kubebuilder:printcolumn:name="Default",type=boolean,JSONPath=`.spec.default`
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.oci.repository`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftStorageLocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageLocationSpec   `json:"spec"`
	Status StorageLocationStatus `json:"status,omitempty"`
}

// SwiftStorageLocationList contains a list of SwiftStorageLocation.
// +kubebuilder:object:root=true
type SwiftStorageLocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftStorageLocation `json:"items"`
}

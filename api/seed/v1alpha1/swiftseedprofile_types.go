package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DatasourceType is the cloud-init datasource type.
// +kubebuilder:validation:Enum=NoCloud
type DatasourceType string

const (
	DatasourceNoCloud DatasourceType = "NoCloud"
)

// SeedDataField holds inline content or a reference to Secret/ConfigMap. Exactly one of Value or ValueFrom should be set.
type SeedDataField struct {
	Value     string             `json:"value,omitempty"`
	ValueFrom *SeedDataValueFrom `json:"valueFrom,omitempty"`
}

// SeedDataValueFrom holds a reference to Secret or ConfigMap. Exactly one should be set.
type SeedDataValueFrom struct {
	SecretKeyRef    *corev1.SecretKeySelector    `json:"secretKeyRef,omitempty"`
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`
}

// SwiftSeedProfileSpec defines the desired state of SwiftSeedProfile.
type SwiftSeedProfileSpec struct {
	Datasource   DatasourceType     `json:"datasource"`
	UserData     string             `json:"userData"` // Inline; use UserDataFrom for ref
	UserDataFrom *SeedDataValueFrom `json:"userDataFrom,omitempty"`
	// MetaData is inline NoCloud instance metadata (use MetaDataFrom for a ref).
	//
	// Optional because it is DEFAULTED, not because NoCloud works without it: a
	// NoCloud seed disk with no meta-data file is not recognised as a datasource
	// at all, and cloud-init then discards userData wholesale while the guest
	// still boots and reports Ready (#457). When neither this nor MetaDataFrom is
	// set, the controller synthesizes `instance-id: <namespace>-<guest>` +
	// `local-hostname: <guest>`. Set it only to override that.
	MetaData        string             `json:"metaData,omitempty"`
	MetaDataFrom    *SeedDataValueFrom `json:"metaDataFrom,omitempty"`
	NetworkData     string             `json:"networkData,omitempty"` // Inline; use NetworkDataFrom for ref
	NetworkDataFrom *SeedDataValueFrom `json:"networkDataFrom,omitempty"`
}

// SwiftSeedProfile is the Schema for the swiftseedprofiles API.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=swiftseedprofiles,scope=Namespaced,shortName=ssp
// +kubebuilder:printcolumn:name="Datasource",type=string,JSONPath=`.spec.datasource`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftSeedProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SwiftSeedProfileSpec `json:"spec,omitempty"`
}

// SwiftSeedProfileList contains a list of SwiftSeedProfile.
// +kubebuilder:object:root=true
type SwiftSeedProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftSeedProfile `json:"items"`
}

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftGuestPool manages a fleet of identical SwiftGuest replicas.
// It creates, deletes, and monitors SwiftGuests to maintain the desired count.
// Similar to ReplicaSet but for VMs -- stable names (<pool>-<index>), highest-index-first
// scale-down, automatic replacement of failed VMs, rolling updates, topology spread,
// and per-replica persistent storage.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=sgpool
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=`.status.updatedReplicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftGuestPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftGuestPoolSpec   `json:"spec"`
	Status            SwiftGuestPoolStatus `json:"status,omitempty"`
}

// SwiftGuestPoolSpec defines the desired state of a SwiftGuestPool.
type SwiftGuestPoolSpec struct {
	// Replicas is the desired number of SwiftGuest replicas.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas"`

	// Template describes the SwiftGuest to create for each replica.
	Template SwiftGuestTemplateSpec `json:"template"`

	// UpdateStrategy controls how VMs are replaced when the template changes.
	// +optional
	UpdateStrategy *UpdateStrategy `json:"updateStrategy,omitempty"`

	// SpreadPolicy is a shorthand for topology spread.
	// "Spread" adds a hostname-based topology constraint to each replica.
	// "Pack" (default) adds no constraints.
	// TopologySpreadConstraints takes precedence if both are set.
	// +kubebuilder:validation:Enum=Pack;Spread
	// +kubebuilder:default=Pack
	// +optional
	SpreadPolicy string `json:"spreadPolicy,omitempty"`

	// TopologySpreadConstraints specifies how to spread replicas across topology domains.
	// Applied to each SwiftGuest created by the pool. Takes precedence over SpreadPolicy.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// VolumeClaimTemplates defines PVCs to create per replica.
	// Each replica gets its own PVC named <template-name>-<pool-name>-<index>.
	// PVCs survive VM replacement and are deleted when the pool is deleted.
	// +optional
	VolumeClaimTemplates []PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
}

// UpdateStrategy controls how VMs are replaced during template changes.
type UpdateStrategy struct {
	// Type of update strategy: RollingUpdate or Recreate.
	// +kubebuilder:validation:Enum=RollingUpdate;Recreate
	// +kubebuilder:default=RollingUpdate
	Type string `json:"type,omitempty"`

	// RollingUpdate configuration. Only used when type=RollingUpdate.
	// +optional
	RollingUpdate *RollingUpdateConfig `json:"rollingUpdate,omitempty"`
}

// RollingUpdateConfig controls the pace of rolling updates.
type RollingUpdateConfig struct {
	// MaxUnavailable is the max number of VMs that can be unavailable during update.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	MaxUnavailable int32 `json:"maxUnavailable"`

	// MaxSurge is the max number of VMs above desired count during update.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	MaxSurge int32 `json:"maxSurge"`
}

// PersistentVolumeClaimTemplate describes a PVC to create per replica.
type PersistentVolumeClaimTemplate struct {
	// Metadata for the PVC. Name is required and used as a prefix in the PVC name:
	// <metadata.name>-<pool-name>-<index>.
	Metadata PoolObjectMeta `json:"metadata"`

	// Spec is the PVC spec.
	Spec corev1.PersistentVolumeClaimSpec `json:"spec"`
}

// SwiftGuestTemplateSpec describes the SwiftGuest that will be created per replica.
type SwiftGuestTemplateSpec struct {
	// Metadata to apply to each SwiftGuest (labels, annotations).
	// The controller adds pool-management labels in addition to these.
	// +optional
	Metadata PoolObjectMeta `json:"metadata,omitempty"`

	// Spec is the SwiftGuestSpec for each replica.
	Spec SwiftGuestSpec `json:"spec"`
}

// PoolObjectMeta contains metadata fields applied to each SwiftGuest.
// Subset of metav1.ObjectMeta -- only labels and annotations.
type PoolObjectMeta struct {
	// Name is used as a prefix for generated resource names (PVCs).
	// +optional
	Name string `json:"name,omitempty"`

	// Labels to apply to each SwiftGuest.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations to apply to each SwiftGuest.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SwiftGuestPoolStatus defines the observed state of SwiftGuestPool.
type SwiftGuestPoolStatus struct {
	// Replicas is the total number of SwiftGuests owned by this pool.
	Replicas int32 `json:"replicas"`

	// ReadyReplicas is the number of SwiftGuests with phase=Running and GuestRunning=True.
	ReadyReplicas int32 `json:"readyReplicas"`

	// AvailableReplicas is the number of available replicas (same as readyReplicas).
	AvailableReplicas int32 `json:"availableReplicas"`

	// FailedReplicas is the number of SwiftGuests with phase=Failed.
	FailedReplicas int32 `json:"failedReplicas"`

	// UpdatedReplicas is the number of SwiftGuests matching the current template hash.
	UpdatedReplicas int32 `json:"updatedReplicas"`

	// CurrentTemplateHash is the hash of the current template spec.
	CurrentTemplateHash string `json:"currentTemplateHash,omitempty"`

	// Conditions represent the latest observations of the pool's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types for SwiftGuestPool.
const (
	PoolConditionAvailable   = "Available"
	PoolConditionProgressing = "Progressing"
	PoolConditionUpdated     = "Updated"
)

// Update strategy types.
const (
	UpdateStrategyRollingUpdate = "RollingUpdate"
	UpdateStrategyRecreate      = "Recreate"
)

// Spread policy types.
const (
	SpreadPolicyPack   = "Pack"
	SpreadPolicySpread = "Spread"
)

// Pool label and annotation keys set by the controller.
const (
	LabelPoolName          = "swift.kubeswift.io/pool"
	LabelPoolIndex         = "swift.kubeswift.io/pool-index"
	AnnotationTemplateHash = "swift.kubeswift.io/template-hash"
)

// SwiftGuestPoolList contains a list of SwiftGuestPool.
// +kubebuilder:object:root=true
type SwiftGuestPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftGuestPool `json:"items"`
}

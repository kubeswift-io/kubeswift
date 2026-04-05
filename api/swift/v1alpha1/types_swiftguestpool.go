package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftGuestPool manages a fleet of identical SwiftGuest replicas.
// It creates, deletes, and monitors SwiftGuests to maintain the desired count.
// Similar to ReplicaSet but for VMs — stable names (<pool>-<index>), highest-index-first
// scale-down, automatic replacement of failed VMs.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=sgpool
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
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

	// Conditions represent the latest observations of the pool's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types for SwiftGuestPool.
const (
	PoolConditionAvailable   = "Available"
	PoolConditionProgressing = "Progressing"
)

// Pool label keys set by the controller on each SwiftGuest.
const (
	LabelPoolName  = "swift.kubeswift.io/pool"
	LabelPoolIndex = "swift.kubeswift.io/pool-index"
)

// SwiftGuestPoolList contains a list of SwiftGuestPool.
// +kubebuilder:object:root=true
type SwiftGuestPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftGuestPool `json:"items"`
}

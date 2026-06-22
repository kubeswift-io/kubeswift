package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterSpec describes a member cluster that the kubeswift-gateway hub
// federates. The hub watches Cluster objects, builds an impersonation-capable
// client + informer cache per member (the client pool), and fans out the
// read/write/telemetry/console planes across the fleet, stamping a `cluster`
// dimension on every result. See docs/design/ui-backend-enablement.md (D2/D2a).
//
// Cluster objects are namespaced and live in the hub cluster alongside their
// credential Secret (the Cluster API model). The kubeswift-gateway — NOT the
// controller-manager — is the reconciler for this resource; the controller-
// manager merely needs the type registered for serialization.
type ClusterSpec struct {
	// Server is the member cluster's API server URL (https://host:port).
	// Optional when the credential Secret carries a full kubeconfig whose
	// current-context already names the server.
	// +optional
	Server string `json:"server,omitempty"`

	// CredentialSecretRef names a Secret in the same namespace holding the
	// hub's credential for this member. The Secret must carry EITHER a
	// `kubeconfig` key (a full kubeconfig — the simplest path) OR a `token`
	// key (a bearer token) plus an optional `ca.crt` key (the API server CA).
	//
	// The credential must be able to impersonate users: the gateway sets
	// Impersonate-User / Impersonate-Group per request (decision D1), so the
	// member's RBAC authorizes the end user, not the gateway. For that to
	// authorize uniformly across the fleet, the members should share an
	// identity provider; where they do not, impersonation is only valid where
	// the subject is bound (documented degradation, surfaced per-cluster).
	CredentialSecretRef corev1.LocalObjectReference `json:"credentialSecretRef"`

	// PrometheusEndpoint is the base URL of this member's Prometheus (or a
	// query frontend / Thanos) used for per-VM telemetry (decision D4). The
	// gateway joins series on the swift.kubeswift.io/guest label (never pod
	// name) and stamps the `cluster` dimension. Empty means telemetry is
	// unavailable for this member; the UI degrades that panel, it does not
	// fail the view.
	// +optional
	PrometheusEndpoint string `json:"prometheusEndpoint,omitempty"`

	// DisplayName is an optional human-friendly label for the UI's cluster
	// selector. The gateway falls back to metadata.name when empty.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// InsecureSkipTLSVerify disables API server certificate verification for
	// this member. UNSAFE — intended only for a trusted-network / dev member
	// whose CA is not yet wired. Prefer shipping a `ca.crt` key in the
	// credential Secret.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// Standard condition types exposed by Cluster.
const (
	// ClusterConditionReady is True when the gateway holds a healthy,
	// authenticated client and a synced informer cache for this member.
	ClusterConditionReady = "Ready"
	// ClusterConditionReachable is True when the member API server last
	// answered a discovery/health probe. It is set False (with a reason) when
	// the member is unreachable, so the UI surfaces a per-cluster error rather
	// than failing the whole-fleet query (no silent failures).
	ClusterConditionReachable = "Reachable"
)

// ClusterStatus is populated by the kubeswift-gateway as it connects to and
// syncs the member cluster.
type ClusterStatus struct {
	// Conditions follow the standard Kubernetes pattern (Ready, Reachable).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastConnected is the last time the gateway successfully reached this
	// member's API server.
	// +optional
	LastConnected *metav1.Time `json:"lastConnected,omitempty"`

	// KubernetesVersion is the member's discovered API server version
	// (e.g. "v1.34.3"), shown in the UI's cluster detail.
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// GuestCount is the number of SwiftGuests observed on this member at the
	// last sync — a cheap roll-up for the cluster selector. Omitted until the
	// gateway has synced the member at least once.
	// +optional
	GuestCount *int32 `json:"guestCount,omitempty"`

	// ObservedGeneration is the spec generation the gateway last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Cluster is a member cluster federated by the kubeswift-gateway hub.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=clusters,scope=Namespaced,shortName=ksc
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.server`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="K8s",type=string,JSONPath=`.status.kubernetesVersion`
// +kubebuilder:printcolumn:name="Guests",type=integer,JSONPath=`.status.guestCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

// ClusterList contains a list of Cluster.
// +kubebuilder:object:root=true
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

package seed

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	KeyUserData      = "user-data"
	KeyMetaData      = "meta-data"
	KeyNetworkConfig = "network-config"
)

// BuildConfigMap creates a ConfigMap with NoCloud-standard keys.
// Omit keys for empty values. Caller should set OwnerReferences for garbage collection.
// BuildSecret renders the seed as a Secret.
//
// cloud-init user-data routinely carries SSH authorized keys, passwords and
// cluster join tokens, and a SwiftSeedProfile can source it from a Secret
// (valueFrom.secretKeyRef). Rendering that into a ConfigMap re-materialized the
// credential in the clear for anyone holding `get configmaps` in the namespace —
// a strictly weaker grant than `get secrets`, and one that RBAC policies
// routinely hand out. Same keys and same layout as BuildConfigMap, so nothing
// downstream of the mount changes.
func BuildSecret(name, namespace string, userData, metaData, networkData string) *corev1.Secret {
	data := make(map[string]string)
	if userData != "" {
		data[KeyUserData] = userData
	}
	if metaData != "" {
		data[KeyMetaData] = metaData
	}
	if networkData != "" {
		data[KeyNetworkConfig] = networkData
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		StringData: data,
	}
}

// BuildConfigMap is the legacy renderer, kept only so the migration path can
// recognise and retire a pre-existing seed ConfigMap. New guests get a Secret.
//
// Deprecated: use BuildSecret.
func BuildConfigMap(name, namespace string, userData, metaData, networkData string) *corev1.ConfigMap {
	data := make(map[string]string)
	if userData != "" {
		data[KeyUserData] = userData
	}
	if metaData != "" {
		data[KeyMetaData] = metaData
	}
	if networkData != "" {
		data[KeyNetworkConfig] = networkData
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}
}

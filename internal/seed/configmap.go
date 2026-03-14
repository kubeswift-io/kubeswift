package seed

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)


const (
	KeyUserData    = "user-data"
	KeyMetaData    = "meta-data"
	KeyNetworkConfig = "network-config"
)

// BuildConfigMap creates a ConfigMap with NoCloud-standard keys.
// Omit keys for empty values. Caller should set OwnerReferences for garbage collection.
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

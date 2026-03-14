package seed

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
)

// Resolve resolves a SeedDataField to a string. If ValueFrom is set, fetches from Secret or ConfigMap.
func Resolve(ctx context.Context, c client.Client, namespace string, value string, from *seedv1alpha1.SeedDataValueFrom) (string, error) {
	if from == nil {
		return value, nil
	}
	if from.SecretKeyRef != nil {
		return resolveSecret(ctx, c, namespace, from.SecretKeyRef)
	}
	if from.ConfigMapKeyRef != nil {
		return resolveConfigMap(ctx, c, namespace, from.ConfigMapKeyRef)
	}
	return value, nil
}

func resolveSecret(ctx context.Context, c client.Client, namespace string, ref *corev1.SecretKeySelector) (string, error) {
	if ref == nil {
		return "", fmt.Errorf("secretKeyRef is nil")
	}
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return "", fmt.Errorf("secret %s/%s: %w", namespace, ref.Name, err)
	}
	if secret.Data == nil {
		return "", fmt.Errorf("secret %s/%s has no data", namespace, ref.Name)
	}
	val, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s missing key %q", namespace, ref.Name, ref.Key)
	}
	return string(val), nil
}

func resolveConfigMap(ctx context.Context, c client.Client, namespace string, ref *corev1.ConfigMapKeySelector) (string, error) {
	if ref == nil {
		return "", fmt.Errorf("configMapKeyRef is nil")
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &cm); err != nil {
		return "", fmt.Errorf("configmap %s/%s: %w", namespace, ref.Name, err)
	}
	if cm.Data == nil {
		return "", fmt.Errorf("configmap %s/%s has no data", namespace, ref.Name)
	}
	val, ok := cm.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("configmap %s/%s missing key %q", namespace, ref.Name, ref.Key)
	}
	return val, nil
}


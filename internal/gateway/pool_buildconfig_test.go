package gateway

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	fleetv1alpha1 "github.com/kubeswift-io/kubeswift/api/fleet/v1alpha1"
)

// buildConfig's spec.local validation runs before any hub read or
// InClusterConfig call, so a zero-value pool exercises it.
func TestBuildConfig_LocalValidation(t *testing.T) {
	p := &ClientPool{}

	// local + credentialSecretRef -> mutually exclusive.
	c := &fleetv1alpha1.Cluster{Spec: fleetv1alpha1.ClusterSpec{
		Local:               true,
		CredentialSecretRef: &corev1.LocalObjectReference{Name: "x"},
	}}
	if _, err := p.buildConfig(context.Background(), c); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("local+credentialSecretRef: want mutual-exclusion error, got %v", err)
	}

	// neither local nor credentialSecretRef -> required.
	if _, err := p.buildConfig(context.Background(), &fleetv1alpha1.Cluster{}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("neither: want required error, got %v", err)
	}

	// local alone gets past validation to InClusterConfig (which errors outside a
	// pod) — the point is it does NOT report the validation errors above.
	if _, err := p.buildConfig(context.Background(), &fleetv1alpha1.Cluster{Spec: fleetv1alpha1.ClusterSpec{Local: true}}); err == nil {
		t.Fatalf("local alone outside a cluster: want an InClusterConfig error, got nil")
	} else if strings.Contains(err.Error(), "mutually exclusive") || strings.Contains(err.Error(), "required") {
		t.Fatalf("local alone must reach InClusterConfig, not validation: %v", err)
	}
}

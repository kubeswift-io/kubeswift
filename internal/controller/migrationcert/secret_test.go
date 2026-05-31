package migrationcert

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func secretScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	return s
}

func sourceSecret(ns, nodeName string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: MigrationNodeSecretName(nodeName), Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data:       data,
	}
}

var tlsData = map[string][]byte{
	migrationSecretTLSCert: []byte("CERT"),
	migrationSecretTLSKey:  []byte("KEY"),
	migrationSecretCACert:  []byte("CA"),
}

// TestEnsureMigrationIdentitySecret_SameNamespaceNoop pins the fast-path:
// when the guest runs in the system namespace, cert-manager already wrote
// the Secret — copying would be redundant (and would risk fighting
// cert-manager's ownership).
func TestEnsureMigrationIdentitySecret_SameNamespaceNoop(t *testing.T) {
	scheme := secretScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	// No source secret present; same-namespace must still be a no-op
	// (it returns before any Get).
	if err := EnsureMigrationIdentitySecret(ctx, c, testSystemNS, testSystemNS, "miles"); err != nil {
		t.Fatalf("same-namespace must be a no-op, got: %v", err)
	}
}

// TestEnsureMigrationIdentitySecret_SourceMissingErrors pins the
// precondition contract: a missing source Secret (per-node Certificate not
// issued/ready yet) is an error, NOT a silent skip. PR 3's Validating-live
// phase relies on this to refuse a migration that would otherwise fall
// back to a broken/insecure channel.
func TestEnsureMigrationIdentitySecret_SourceMissingErrors(t *testing.T) {
	scheme := secretScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	if err := EnsureMigrationIdentitySecret(ctx, c, testSystemNS, "team-a", "miles"); err == nil {
		t.Fatal("expected error when source secret missing (precondition not ready)")
	}
}

func TestEnsureMigrationIdentitySecret_CopiesToTarget(t *testing.T) {
	scheme := secretScheme(t)
	src := sourceSecret(testSystemNS, "miles", tlsData)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(src).Build()
	ctx := context.Background()

	if err := EnsureMigrationIdentitySecret(ctx, c, testSystemNS, "team-a", "miles"); err != nil {
		t.Fatalf("EnsureMigrationIdentitySecret: %v", err)
	}

	var copied corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "kubeswift-migration-node-miles"}, &copied); err != nil {
		t.Fatalf("expected copied secret in team-a: %v", err)
	}
	if copied.Type != corev1.SecretTypeTLS {
		t.Errorf("copied secret Type = %q, want kubernetes.io/tls", copied.Type)
	}
	for k, v := range tlsData {
		if string(copied.Data[k]) != string(v) {
			t.Errorf("copied data[%q] = %q, want %q", k, copied.Data[k], v)
		}
	}
	// No ownerRef: the copy outlives any single guest/migration in the
	// namespace (same invariant as the swiftletd-reporter RoleBinding).
	if len(copied.OwnerReferences) != 0 {
		t.Errorf("copied secret must carry no ownerRef, got %d", len(copied.OwnerReferences))
	}
	if copied.Labels["app.kubernetes.io/managed-by"] != "kubeswift-controller-manager" {
		t.Errorf("copied secret missing managed-by label")
	}
}

// TestEnsureMigrationIdentitySecret_UpdatesOnRotation pins that when
// cert-manager rotates the source keypair, the next call refreshes the
// copy rather than leaving a stale cert in the guest namespace.
func TestEnsureMigrationIdentitySecret_UpdatesOnRotation(t *testing.T) {
	scheme := secretScheme(t)
	src := sourceSecret(testSystemNS, "miles", tlsData)
	staleCopy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeswift-migration-node-miles", Namespace: "team-a"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			migrationSecretTLSCert: []byte("OLD-CERT"),
			migrationSecretTLSKey:  []byte("OLD-KEY"),
			migrationSecretCACert:  []byte("CA"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(src, staleCopy).Build()
	ctx := context.Background()

	if err := EnsureMigrationIdentitySecret(ctx, c, testSystemNS, "team-a", "miles"); err != nil {
		t.Fatalf("EnsureMigrationIdentitySecret (rotation): %v", err)
	}

	var refreshed corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "kubeswift-migration-node-miles"}, &refreshed); err != nil {
		t.Fatalf("get refreshed: %v", err)
	}
	if string(refreshed.Data[migrationSecretTLSCert]) != "CERT" {
		t.Errorf("rotation not propagated: tls.crt = %q, want CERT", refreshed.Data[migrationSecretTLSCert])
	}
}

// TestEnsureMigrationIdentitySecret_IdempotentWhenCurrent pins that a
// second call with matching data performs no update (no churn).
func TestEnsureMigrationIdentitySecret_IdempotentWhenCurrent(t *testing.T) {
	scheme := secretScheme(t)
	src := sourceSecret(testSystemNS, "miles", tlsData)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(src).Build()
	ctx := context.Background()

	if err := EnsureMigrationIdentitySecret(ctx, c, testSystemNS, "team-a", "miles"); err != nil {
		t.Fatalf("first copy: %v", err)
	}
	if err := EnsureMigrationIdentitySecret(ctx, c, testSystemNS, "team-a", "miles"); err != nil {
		t.Fatalf("second copy (idempotent): %v", err)
	}
	var copied corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "kubeswift-migration-node-miles"}, &copied); err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(copied.Data[migrationSecretTLSCert]) != "CERT" {
		t.Errorf("data corrupted on idempotent call")
	}
}

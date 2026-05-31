package migrationcert

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Standard kubernetes.io/tls Secret keys cert-manager writes, plus the CA
// bundle. stunnel needs all three: tls.crt + tls.key to present the node's
// identity, ca.crt to verify the peer's chain.
const (
	migrationSecretTLSCert = "tls.crt"
	migrationSecretTLSKey  = "tls.key"
	migrationSecretCACert  = "ca.crt"
)

// EnsureMigrationIdentitySecret distributes a node's already-issued
// migration identity Secret into a guest namespace.
//
// This is DISTRIBUTION, not issuance: cert-manager mints the per-node
// Certificate in the system namespace ahead of time (§3.B(a), a
// precondition); the Secret it produces is copied verbatim here. No
// cert-manager call sits on the migration path (§4.4) — the design
// requires identities exist BEFORE a migration starts; this helper merely
// places an existing one where the launcher pod can mount it.
//
// Same-namespace fast-path: when the guest runs in the system namespace,
// cert-manager already wrote the Secret there — nothing to copy.
//
// Source-missing is a precondition failure, surfaced as an error. PR 3's
// Validating-live phase calls this; a missing source Secret means the
// node's Certificate has not been issued/ready yet, and the migration must
// not proceed (it would fall back to an insecure or broken channel).
//
// The copied Secret carries NO ownerRef: like the swiftletd-reporter
// RoleBinding it is shared across all guests/migrations in the namespace
// and must outlive any single one. Create-if-absent, update-if-differs
// keeps it current across cert-manager rotation.
//
// NOTE: wired by PR 3 (controller sidecar integration). PR 1 ships and
// tests it but does not call it from any reconcile path.
func EnsureMigrationIdentitySecret(ctx context.Context, c client.Client, systemNamespace, targetNamespace, nodeName string) error {
	if targetNamespace == systemNamespace {
		// cert-manager already wrote the Secret in this namespace.
		return nil
	}

	secretName := MigrationNodeSecretName(nodeName)
	srcKey := types.NamespacedName{Namespace: systemNamespace, Name: secretName}

	var source corev1.Secret
	if err := c.Get(ctx, srcKey, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("migration identity secret %s not yet provisioned (cert-manager precondition not ready): %w", srcKey, err)
		}
		return fmt.Errorf("get source migration identity secret %s: %w", srcKey, err)
	}

	desiredData := map[string][]byte{}
	for _, k := range []string{migrationSecretTLSCert, migrationSecretTLSKey, migrationSecretCACert} {
		if v, ok := source.Data[k]; ok {
			desiredData[k] = v
		}
	}

	targetKey := types.NamespacedName{Namespace: targetNamespace, Name: secretName}
	var existing corev1.Secret
	err := c.Get(ctx, targetKey, &existing)
	if err == nil {
		if existing.Type == source.Type && secretDataEqual(existing.Data, desiredData) {
			return nil
		}
		existing.Type = source.Type
		existing.Data = desiredData
		if uerr := c.Update(ctx, &existing); uerr != nil {
			return fmt.Errorf("update copied migration identity secret %s: %w", targetKey, uerr)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get target migration identity secret %s: %w", targetKey, err)
	}

	copySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: targetNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "migration-mtls",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
				migrationNodeLabel:             nodeName,
			},
		},
		Type: source.Type,
		Data: desiredData,
	}
	if cerr := c.Create(ctx, copySecret); cerr != nil {
		if apierrors.IsAlreadyExists(cerr) {
			return nil
		}
		return fmt.Errorf("create copied migration identity secret %s: %w", targetKey, cerr)
	}
	return nil
}

func secretDataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !bytes.Equal(av, bv) {
			return false
		}
	}
	return true
}

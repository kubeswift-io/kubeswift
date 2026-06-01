// Package migrationcert provisions the per-node TLS identities used by
// the Phase 3c live-migration mTLS transport (stunnel sidecar).
//
// Design contract: docs/design/live-migration-phase-3c.md, Option B
// (per-node identity + SAN pinning). Each worker node gets one
// cert-manager Certificate whose SAN (and CN) is the node name; the
// stunnel sidecar on a migration's source/destination pod presents
// that cert and the peer pins it via `verifyChain = yes` + `checkHost =
// <peer-node-name>`. A bare `verifyChain` (verify=2 without subject
// checks) is insufficient (W-3c-4) — the SAN pin is what proves "this
// is the legitimate src/dst node for THIS migration", not merely "the
// peer holds a CA-signed cert". (Not literal stunnel `verify = 4`,
// which ignores the CA chain and pins the exact leaf, breaking
// cert-manager rotation — see docs/design/live-migration-phase-3c.md
// §3 directive note.)
//
// This file owns the per-node Certificate lifecycle:
//   - newNodeCertificate builds the cert-manager Certificate object,
//   - ensureNodeCertificate creates it idempotently,
//   - deleteNodeCertificate garbage-collects it when a node goes away
//     or is (re)labeled control-plane.
//
// Per §3.B(a) the per-node Certificate is a long-lived precondition —
// issued once when a worker joins, NOT minted mid-migration (§4.4: no
// cert-manager call sits on any migration path). The migration data
// path only ever consumes the already-issued Secret (copied into guest
// namespaces by secret.go).
//
// cert-manager is a prerequisite when migration mTLS is enabled (same
// dependency as the webhook serving cert). The reconciler that drives
// these helpers is only constructed when --migration-mtls-enabled=true,
// so clusters without cert-manager are unaffected by default.
//
// cert-manager types are addressed as unstructured.Unstructured rather
// than importing the cert-manager Go module: it keeps cert-manager out
// of go.mod (it is an optional, operator-installed dependency) and the
// Certificate shape we emit is small and stable.
package migrationcert

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	certManagerGroup      = "cert-manager.io"
	certManagerVersion    = "v1"
	certManagerAPIVersion = certManagerGroup + "/" + certManagerVersion
	certificateKind       = "Certificate"

	// MigrationCAIssuerName is the namespaced cert-manager Issuer (in the
	// controller's system namespace) that signs per-node migration leaf
	// certificates. The Helm chart's migration issuer template
	// (charts/kubeswift/templates/migration/issuer.yaml) creates it as a
	// CA Issuer backed by the kubeswift-migration-ca Secret. Exported so
	// PR 3's sidecar-wiring code can reference the same name.
	MigrationCAIssuerName = "kubeswift-migration-ca-issuer"

	// migrationNodeLabel records which node a Certificate/Secret belongs
	// to (used for filtering and human inspection).
	migrationNodeLabel = "kubeswift.io/migration-node"

	// certNamePrefix is prepended to the node name to form the per-node
	// Certificate (and Secret) name.
	certNamePrefix = "kubeswift-migration-node-"

	// certDuration / certRenewBefore mirror the webhook serving-cert
	// cadence (1y lifetime, renew ~15d early). cert-manager handles the
	// rotation; secret.go re-copies the rotated material on the next
	// migration that needs it.
	certDuration    = "8760h"
	certRenewBefore = "360h"
)

var certificateGVK = schema.GroupVersionKind{
	Group:   certManagerGroup,
	Version: certManagerVersion,
	Kind:    certificateKind,
}

// MigrationNodeCertName returns the cert-manager Certificate name for a node.
func MigrationNodeCertName(nodeName string) string { return certNamePrefix + nodeName }

// MigrationNodeSecretName returns the name of the Secret holding a node's
// migration identity (tls.crt / tls.key / ca.crt). It matches the
// Certificate's secretName — cert-manager writes the issued keypair there.
// PR 3 copies this Secret into guest namespaces (see secret.go).
func MigrationNodeSecretName(nodeName string) string { return certNamePrefix + nodeName }

// MigrationNodeCertSAN returns the SAN (and CN) stamped into a node's
// migration certificate. Option B pins peer identity on this value via
// stunnel `checkHost`; it is exactly the node name.
func MigrationNodeCertSAN(nodeName string) string { return nodeName }

// newCertShell returns an empty Certificate object with its GVK set, for
// use as the receiver of a Get or the target of a Delete.
func newCertShell() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(certificateGVK)
	return u
}

func nodeCertLabels(nodeName string) map[string]interface{} {
	return map[string]interface{}{
		"app.kubernetes.io/name":       "kubeswift",
		"app.kubernetes.io/component":  "migration-mtls",
		"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
		migrationNodeLabel:             nodeName,
	}
}

// newNodeCertificate builds the cert-manager Certificate for a worker node.
// SAN/CN = nodeName (the Option B identity); usages cover both server auth
// (the node acts as the dst stunnel TLS server) and client auth (the node
// acts as the src stunnel TLS client). Signed by the namespaced CA Issuer.
func newNodeCertificate(systemNamespace, nodeName string) *unstructured.Unstructured {
	san := MigrationNodeCertSAN(nodeName)
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": certManagerAPIVersion,
			"kind":       certificateKind,
			"metadata": map[string]interface{}{
				"name":      MigrationNodeCertName(nodeName),
				"namespace": systemNamespace,
				"labels":    nodeCertLabels(nodeName),
			},
			"spec": map[string]interface{}{
				"secretName":  MigrationNodeSecretName(nodeName),
				"duration":    certDuration,
				"renewBefore": certRenewBefore,
				"commonName":  san,
				"dnsNames":    []interface{}{san},
				// Node is both the TLS client (src) and TLS server (dst)
				// across different migrations, so it needs both usages.
				"usages": []interface{}{"server auth", "client auth"},
				"issuerRef": map[string]interface{}{
					"name":  MigrationCAIssuerName,
					"kind":  "Issuer",
					"group": certManagerGroup,
				},
			},
		},
	}
}

// ensureNodeCertificate creates the per-node Certificate if absent.
// Idempotent: returns nil when the Certificate already exists (fast-path
// Get, plus an AlreadyExists tolerance for the create race). cert-manager
// owns the Certificate's contents thereafter; this helper never mutates an
// existing one.
func ensureNodeCertificate(ctx context.Context, c client.Client, systemNamespace, nodeName string) error {
	key := types.NamespacedName{Namespace: systemNamespace, Name: MigrationNodeCertName(nodeName)}

	existing := newCertShell()
	err := c.Get(ctx, key, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get migration certificate %s: %w", key, err)
	}

	cert := newNodeCertificate(systemNamespace, nodeName)
	if err := c.Create(ctx, cert); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create migration certificate %s: %w", key, err)
	}
	return nil
}

// deleteNodeCertificate removes the per-node Certificate. Tolerates
// NotFound (already gone). Deleting the Certificate causes cert-manager to
// remove the backing Secret in the system namespace; copied per-namespace
// Secrets are independent and outlive this (PR 3 owns their lifecycle).
func deleteNodeCertificate(ctx context.Context, c client.Client, systemNamespace, nodeName string) error {
	cert := newCertShell()
	cert.SetNamespace(systemNamespace)
	cert.SetName(MigrationNodeCertName(nodeName))
	if err := c.Delete(ctx, cert); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

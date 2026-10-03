// Package storagelocation holds the rules for storage.kubeswift.io locations
// that the controller, the admission webhook and the resolvers share, so a
// location's validity means one thing everywhere.
package storagelocation

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"oras.land/oras-go/v2/registry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
)

// Validate checks what the CRD schema cannot: the repository is a registry
// reference with no tag or digest, the CA bundle holds at least one
// certificate, the verify key is a PEM public key, and the Secret names are
// valid object names.
func Validate(spec *storagev1alpha1.StorageLocationSpec) error {
	if spec.OCI == nil && spec.CSI == nil {
		return fmt.Errorf("a location sets oci, csi, or both")
	}
	if o := spec.OCI; o != nil {
		if _, err := Registry(o.Repository); err != nil {
			return err
		}
		if o.Anonymous && o.CredentialsSecretName != "" {
			return fmt.Errorf("spec.oci: anonymous and credentialsSecretName are mutually exclusive")
		}
		for field, name := range map[string]string{"credentialsSecretName": o.CredentialsSecretName, "signingKeySecretName": o.SigningKeySecretName} {
			if name == "" {
				continue
			}
			if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
				return fmt.Errorf("spec.oci.%s %q is not a valid Secret name: %s", field, name, strings.Join(errs, "; "))
			}
		}
		if o.CABundle != "" {
			if _, err := CAPool(o.CABundle); err != nil {
				return err
			}
		}
		if o.VerifyKey != "" {
			if err := parsePublicKey(o.VerifyKey); err != nil {
				return err
			}
		}
	}
	if c := spec.CSI; c != nil {
		if errs := validation.IsDNS1123Subdomain(c.VolumeSnapshotClassName); len(errs) > 0 {
			return fmt.Errorf("spec.csi.volumeSnapshotClassName %q is not a valid name: %s", c.VolumeSnapshotClassName, strings.Join(errs, "; "))
		}
	}
	return nil
}

// Registry returns the registry host (with any port) of a repository prefix,
// which must name a registry and carry no tag or digest.
func Registry(repository string) (string, error) {
	ref, err := registry.ParseReference(repository)
	if err != nil {
		return "", fmt.Errorf("spec.oci.repository %q is not a registry/repository reference: %w", repository, err)
	}
	if ref.Reference != "" {
		return "", fmt.Errorf("spec.oci.repository %q carries a tag or digest; give the repository only", repository)
	}
	return ref.Registry, nil
}

// CAPool returns the system roots plus the certificates in bundle, which must
// hold at least one and nothing that fails to parse.
func CAPool(bundle string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	rest := []byte(bundle)
	n := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("spec.oci.caBundle holds a %q PEM block; only CERTIFICATE blocks belong there", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("spec.oci.caBundle: certificate %d does not parse: %w", n+1, err)
		}
		pool.AddCert(cert)
		n++
	}
	if n == 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, fmt.Errorf("spec.oci.caBundle must be one or more PEM CERTIFICATE blocks and nothing else")
	}
	return pool, nil
}

func parsePublicKey(key string) error {
	block, rest := pem.Decode([]byte(key))
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return fmt.Errorf("spec.oci.verifyKey must be one PEM public key")
	}
	if _, err := x509.ParsePKIXPublicKey(block.Bytes); err != nil {
		return fmt.Errorf("spec.oci.verifyKey does not parse as a public key: %w", err)
	}
	return nil
}

// ClusterDefaults lists the names of the cluster locations marked default.
func ClusterDefaults(ctx context.Context, c client.Reader) ([]string, error) {
	var list storagev1alpha1.SwiftClusterStorageLocationList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []string
	for i := range list.Items {
		if list.Items[i].Spec.Default && list.Items[i].DeletionTimestamp == nil {
			out = append(out, list.Items[i].Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// NamespaceDefaults lists the names of the locations in namespace marked default.
func NamespaceDefaults(ctx context.Context, c client.Reader, namespace string) ([]string, error) {
	var list storagev1alpha1.SwiftStorageLocationList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var out []string
	for i := range list.Items {
		if list.Items[i].Spec.Default && list.Items[i].DeletionTimestamp == nil {
			out = append(out, list.Items[i].Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Others returns names without self.
func Others(names []string, self string) []string {
	var out []string
	for _, n := range names {
		if n != self {
			out = append(out, n)
		}
	}
	return out
}

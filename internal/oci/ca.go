package oci

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// RegistryCAEnv carries PEM CA certificates a transfer Job must trust for the
// registry, in addition to the system roots: a registry behind a private CA.
// The controller sets it from the snapshot's recorded storage location.
const RegistryCAEnv = "KUBESWIFT_REGISTRY_CA_BUNDLE"

// systemCertDir is where the images keep their CA certificates; cosign keeps
// trusting it when SSL_CERT_DIR names another directory too.
const systemCertDir = "/etc/ssl/certs"

var (
	caMu sync.Mutex
	// registryTransport, when set, is the transport every registry
	// connection uses: the system roots plus the configured bundle.
	registryTransport *http.Transport
	// caBundle is the configured bundle, kept for the cosign subprocess.
	caBundle string
)

// SetRegistryCA makes every registry connection this process opens, and
// every cosign it runs, trust the PEM certificates in bundle in addition to
// the system roots. It never replaces them, so a public registry keeps
// working. An empty bundle changes nothing; one with no certificate is an
// error.
func SetRegistryCA(bundle string) error {
	if bundle == "" {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(bundle)) {
		return fmt.Errorf("%s holds no PEM certificate", RegistryCAEnv)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	caMu.Lock()
	defer caMu.Unlock()
	registryTransport, caBundle = tr, bundle
	return nil
}

func configuredTransport() *http.Transport {
	caMu.Lock()
	defer caMu.Unlock()
	return registryTransport
}

// cosignEnv is the environment cosign runs with. With a CA bundle configured,
// it is written to a file in a new directory under TMPDIR and SSL_CERT_DIR
// names that directory after the system one: Go, which cosign is written in,
// reads every certificate file in each listed directory on top of its default
// bundle file, whereas SSL_CERT_FILE would replace the system roots. cleanup
// removes the directory.
func cosignEnv() ([]string, func(), error) {
	env := os.Environ()
	caMu.Lock()
	bundle := caBundle
	caMu.Unlock()
	if bundle == "" {
		return env, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "kubeswift-registry-ca-*")
	if err != nil {
		return nil, nil, fmt.Errorf("write the registry CA bundle for cosign: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.WriteFile(filepath.Join(dir, "registry-ca.pem"), []byte(bundle), 0o600); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("write the registry CA bundle for cosign: %w", err)
	}
	return append(env, "SSL_CERT_DIR="+systemCertDir+":"+dir), cleanup, nil
}

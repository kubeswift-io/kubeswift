package materialize

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/kubeswift-io/kubeswift/internal/oci"
)

// Registry request bounds. go-containerregistry's default transport bounds
// the dial (30 s) and the TLS handshake (10 s) but not the wait for a
// response, and no request carried a context: a registry that accepted a
// connection and never answered held the caller forever. In the controller
// that caller was the only SwiftSandbox reconcile worker, so one such registry
// stopped every sandbox in the cluster.
//
// Every request now has a dial, handshake and response-header bound, and
// Options.Context, which the caller bounds overall (a body that stalls after
// its headers is bounded only by that).
const (
	registryDialTimeout           = 5 * time.Second
	registryTLSHandshakeTimeout   = 10 * time.Second
	registryResponseHeaderTimeout = 20 * time.Second
)

var registryTransport http.RoundTripper = newRegistryTransport()

func newRegistryTransport() *http.Transport {
	t := remote.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: registryDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = registryTLSHandshakeTimeout
	t.ResponseHeaderTimeout = registryResponseHeaderTimeout
	return t
}

// remoteOptions are the options for every registry request made for opts:
// its credentials, its context and the bounded transport.
func (opts Options) remoteOptions() []remote.Option {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return []remote.Option{opts.authOption(), remote.WithContext(ctx), remote.WithTransport(registryTransport)}
}

// SetRegistryCA makes every registry request of this process, and every
// cosign it runs, trust the PEM certificates in bundle on top of the system
// roots (a registry behind a private CA, #839). Call it once at start,
// before any registry request. An empty bundle changes nothing; one with no
// certificate is an error.
func SetRegistryCA(bundle string) error {
	if bundle == "" {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(bundle)) {
		return fmt.Errorf("the registry CA bundle holds no PEM certificate")
	}
	if err := oci.SetRegistryCA(bundle); err != nil {
		return err
	}
	t := newRegistryTransport()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	registryTransport = t
	return nil
}

// RegistryTransport is the bounded transport every registry request uses.
func RegistryTransport() http.RoundTripper { return registryTransport }

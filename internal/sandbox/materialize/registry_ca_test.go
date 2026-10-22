package materialize

import (
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// A registry behind a private CA is trusted once its CA is configured, in
// addition to the system roots, never instead of them (#839).
func TestSetRegistryCA(t *testing.T) {
	saved := registryTransport
	t.Cleanup(func() { registryTransport = saved })

	srv := httptest.NewTLSServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	ref, err := name.NewRepository(strings.TrimPrefix(srv.URL, "https://") + "/x/y")
	if err != nil {
		t.Fatal(err)
	}
	list := func() error {
		_, err := remote.List(ref, remote.WithTransport(RegistryTransport()))
		return err
	}
	if err := list(); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("an untrusted registry was accepted: %v", err)
	}

	if err := SetRegistryCA("not a certificate"); err == nil {
		t.Error("a bundle with no certificate was accepted")
	}
	if err := SetRegistryCA(""); err != nil || RegistryTransport() != saved {
		t.Errorf("an empty bundle changed the transport (err %v)", err)
	}

	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	if err := SetRegistryCA(ca); err != nil {
		t.Fatal(err)
	}
	// The registry answers (the repository is empty): TLS is trusted.
	if err := list(); err != nil && strings.Contains(err.Error(), "certificate") {
		t.Errorf("the configured CA is not trusted: %v", err)
	}
	tr := RegistryTransport().(*http.Transport)
	if tr.ResponseHeaderTimeout != registryResponseHeaderTimeout || tr.TLSHandshakeTimeout != registryTLSHandshakeTimeout {
		t.Error("the CA transport lost the request bounds")
	}
}

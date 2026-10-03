package oci

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetCA(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		caMu.Lock()
		registryTransport, caBundle = nil, ""
		caMu.Unlock()
	})
}

// tlsRegistry is a registry stub behind a certificate from its own CA: it
// answers every manifest request with 404, which is enough to tell a TLS
// failure from a request that reached it.
func tlsRegistry(t *testing.T) (host, caPEM string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

func TestSetRegistryCA_Validates(t *testing.T) {
	resetCA(t)
	if err := SetRegistryCA(""); err != nil || configuredTransport() != nil {
		t.Errorf("an empty bundle must change nothing: err=%v", err)
	}
	if err := SetRegistryCA("not pem"); err == nil || !strings.Contains(err.Error(), RegistryCAEnv) {
		t.Errorf("a bundle without a certificate must be refused, naming the env var: %v", err)
	}
}

// A registry behind a private CA is refused until its CA is configured, and
// reached once it is.
func TestNewRepository_PrivateCA(t *testing.T) {
	resetCA(t)
	host, ca := tlsRegistry(t)
	resolve := func() error {
		repo, err := NewRepository(host+"/kubeswift/snapshots", false)
		if err != nil {
			t.Fatal(err)
		}
		_, err = repo.Resolve(context.Background(), "v1")
		return err
	}
	if err := resolve(); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("without the CA: err = %v, want a certificate error", err)
	}
	if err := SetRegistryCA(ca); err != nil {
		t.Fatal(err)
	}
	if err := resolve(); err == nil || strings.Contains(err.Error(), "certificate") {
		t.Fatalf("with the CA: err = %v, want the registry's not-found, not a certificate error", err)
	}
}

// cosign is a separate process: it gets the bundle as a file in a directory
// SSL_CERT_DIR lists after the system one, so its own roots are kept. The
// default CosignRun is exercised against a stand-in cosign on PATH.
func TestCosignRun_PassesTheCA(t *testing.T) {
	resetCA(t)
	_, ca := tlsRegistry(t)

	bin := t.TempDir()
	out := filepath.Join(t.TempDir(), "seen")
	script := "#!/bin/sh\necho \"$SSL_CERT_DIR\" > " + out + "\ncat \"${SSL_CERT_DIR##*:}\"/registry-ca.pem >> " + out + " 2>/dev/null\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "cosign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TMPDIR", t.TempDir())

	if err := CosignRun(context.Background(), []string{"version"}); err != nil {
		t.Fatal(err)
	}
	if seen, _ := os.ReadFile(out); strings.Contains(string(seen), "CERTIFICATE") {
		t.Fatalf("cosign saw a CA with none configured: %q", seen)
	}

	if err := SetRegistryCA(ca); err != nil {
		t.Fatal(err)
	}
	if err := CosignRun(context.Background(), []string{"version"}); err != nil {
		t.Fatal(err)
	}
	seen, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(seen), "\n", 2)
	if !strings.HasPrefix(lines[0], systemCertDir+":") {
		t.Errorf("SSL_CERT_DIR = %q, want the system directory first", lines[0])
	}
	if len(lines) < 2 || !strings.Contains(lines[1], "BEGIN CERTIFICATE") {
		t.Errorf("cosign could not read the bundle: %q", seen)
	}
	dir := strings.TrimPrefix(lines[0], systemCertDir+":")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the bundle directory %s outlived the cosign run", dir)
	}
}

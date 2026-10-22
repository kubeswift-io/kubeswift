package export

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type entry struct {
	name string
	dir  bool
	mode int64
	body string
	mut  func(*tar.Header)
}

// canonical writes entries the way swiftletd does.
func canonical(t *testing.T, entries []entry, trailer []byte) string {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, ModTime: zeroTime, Format: tar.FormatUSTAR}
		if e.dir {
			h.Typeflag = tar.TypeDir
		} else {
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.body))
		}
		if e.mut != nil {
			e.mut(h)
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if !e.dir {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	b.Write(trailer)
	p := filepath.Join(t.TempDir(), "out.tar")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func good() []entry {
	return []entry{
		{name: "bin/", dir: true, mode: 0o755},
		{name: "bin/tool", mode: 0o755, body: "#!/bin/sh\necho hi\n"},
		{name: "cache/", dir: true, mode: 0o755},
		{name: "cache/a.bin", mode: 0o644, body: "compiled bytes"},
	}
}

func tlsRegistry(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(srv.URL, "https://")
}

func TestPush_RoundTripAndDeterminism(t *testing.T) {
	srv, host := tlsRegistry(t)
	archive := canonical(t, good(), nil)
	spec := Spec{Archive: archive, Repository: host + "/platform/outputs/team-a/compiled", Tag: "uid.compiled",
		ArtifactType: "application/vnd.example.cache.v1", Annotations: map[string]string{"example.com/input": "sha256:abc"},
		MaxBytes: 1 << 20, MaxEntries: 100}
	res, err := Push(context.Background(), spec, authn.Anonymous, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 4 || res.ContentBytes != int64(len("#!/bin/sh\necho hi\n")+len("compiled bytes")) {
		t.Errorf("result %+v", res)
	}

	// Consumable as spec.artifacts layout: unpacked reads it: config parses,
	// the layer extracts to the files.
	ref, _ := name.ParseReference(res.Reference)
	img, err := remote.Image(ref, remote.WithTransport(srv.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img.ConfigFile(); err != nil {
		t.Fatalf("config does not parse as an image config: %v", err)
	}
	m, _ := img.Manifest()
	if string(m.Config.MediaType) != EmptyMediaType || len(m.Layers) != 1 || string(m.Layers[0].MediaType) != LayerMediaType {
		t.Errorf("manifest %+v", m)
	}
	if _, ok := m.Annotations["org.opencontainers.image.created"]; ok {
		t.Error("the manifest carries a creation time")
	}
	tr := tar.NewReader(mutate.Extract(img))
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
	if files["cache/a.bin"] != "compiled bytes" || files["bin/tool"] == "" {
		t.Errorf("extracted %v", files)
	}

	// The same archive under another tag: the same digest.
	spec.Tag = "other.compiled"
	again, err := Push(context.Background(), spec, authn.Anonymous, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if again.Digest != res.Digest {
		t.Errorf("digest %s, then %s: not deterministic", res.Digest, again.Digest)
	}
}

func TestVerifyArchive_Refuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []entry
		trailer []byte
		reason  string
	}{
		{"symlink", append(good(), entry{name: "cache/link", mode: 0o777, mut: func(h *tar.Header) { h.Typeflag = tar.TypeSymlink; h.Linkname = "/etc/shadow" }}), nil, ReasonInvalidEntry},
		{"hard link entry", append(good(), entry{name: "cache/hl", mode: 0o644, mut: func(h *tar.Header) { h.Typeflag = tar.TypeLink; h.Linkname = "cache/a.bin" }}), nil, ReasonInvalidEntry},
		{"device", append(good(), entry{name: "cache/dev", mode: 0o644, mut: func(h *tar.Header) { h.Typeflag = tar.TypeChar }}), nil, ReasonInvalidEntry},
		{"absolute", []entry{{name: "/etc/passwd", mode: 0o644, body: "x"}}, nil, ReasonInvalidEntry},
		{"traversal", []entry{{name: "../escape", mode: 0o644, body: "x"}}, nil, ReasonInvalidEntry},
		{"whiteout", append(good(), entry{name: "cache/.wh.a.bin", mode: 0o644, body: ""}), nil, ReasonInvalidEntry},
		{"duplicate", append(good(), entry{name: "cache/a.bin", mode: 0o644, body: "again"}), nil, ReasonInvalidEntry},
		{"orphan", []entry{{name: "nodir/file", mode: 0o644, body: "x"}}, nil, ReasonInvalidEntry},
		{"owner", []entry{{name: "f", mode: 0o644, body: "x", mut: func(h *tar.Header) { h.Uid = 1000 }}}, nil, ReasonInvalidEntry},
		{"setuid", []entry{{name: "f", mode: 0o4755, body: "x"}}, nil, ReasonInvalidEntry},
		{"trailing data", good(), []byte("smuggled"), ReasonInvalidEntry},
	} {
		f, err := os.Open(canonical(t, tc.entries, tc.trailer))
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifyArchive(f, 1<<20, 100)
		f.Close()
		var e *Error
		if !errors.As(err, &e) || e.Reason != tc.reason {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.reason)
		}
	}
	f, _ := os.Open(canonical(t, good(), nil))
	if _, err := VerifyArchive(f, 4, 100); err == nil || !strings.Contains(err.Error(), ReasonTooLarge) {
		t.Errorf("over maxBytes: %v", err)
	}
	f.Close()
	f, _ = os.Open(canonical(t, good(), nil))
	if _, err := VerifyArchive(f, 1<<20, 3); err == nil || !strings.Contains(err.Error(), ReasonTooMany) {
		t.Errorf("over maxEntries: %v", err)
	}
	f.Close()
}

// The platform credential never goes over plain HTTP.
func TestPush_HTTPSOnly(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	_, err := Push(context.Background(), Spec{Archive: canonical(t, good(), nil), Repository: host + "/x/y", Tag: "t",
		MaxBytes: 1 << 20, MaxEntries: 100}, authn.FromConfig(authn.AuthConfig{Username: "u", Password: "p"}), http.DefaultTransport)
	if err == nil || !strings.Contains(err.Error(), "HTTPS only") {
		t.Errorf("err = %v, want an HTTPS-only refusal", err)
	}
}

func TestPush_UnauthorizedIsDefinitive(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	_, err := Push(context.Background(), Spec{Archive: canonical(t, good(), nil), Repository: host + "/x/y", Tag: "t",
		MaxBytes: 1 << 20, MaxEntries: 100}, authn.Anonymous, srv.Client().Transport)
	var e *Error
	if !errors.As(err, &e) || e.Reason != ReasonUnauthorized {
		t.Errorf("err = %v, want %s", err, ReasonUnauthorized)
	}
}

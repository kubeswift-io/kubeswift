package materialize

import (
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host
}

// An ORAS-style artifact: a non-image config and a non-tar layer, as a Spin
// application is pushed. It must come through untouched.
func wasmArtifact(t *testing.T) v1.Image {
	t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, "application/vnd.fermyon.spin.application.v1+config")
	img, err := mutate.Append(img, mutate.Addendum{
		Layer:     static.NewLayer([]byte("\x00asm\x01\x00\x00\x00"), "application/vnd.wasm.content.layer.v1+wasm"),
		MediaType: "application/vnd.wasm.content.layer.v1+wasm",
	})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestMaterializeLayout_ArtifactAsPushed(t *testing.T) {
	host := testRegistry(t)
	ref, _ := name.ParseReference(host + "/team/hello-http:1")
	art := wasmArtifact(t)
	if err := remote.Write(ref, art); err != nil {
		t.Fatal(err)
	}
	want, _ := art.Digest()
	cache := writableCache(t)

	_, digest, err := ResolveDescriptor(Options{ImageRef: ref.String()})
	if err != nil || digest != want.String() {
		t.Fatalf("ResolveDescriptor = %s, %v; want %s", digest, err, want)
	}
	res, err := MaterializeLayout(Options{ImageRef: ref.String(), CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != want.String() || res.CacheHit || res.RootfsPath != filepath.Join(cache, "sha256-"+want.Hex+".oci") {
		t.Fatalf("result = %+v", res)
	}
	for _, f := range []string{"oci-layout", "index.json", "blobs/sha256/" + want.Hex} {
		if _, err := os.Stat(filepath.Join(res.RootfsPath, f)); err != nil {
			t.Errorf("layout lacks %s: %v", f, err)
		}
	}
	lp, err := layout.FromPath(res.RootfsPath)
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := lp.ImageIndex()
	m, _ := idx.IndexManifest()
	if len(m.Manifests) != 1 || m.Manifests[0].Digest != want ||
		m.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != ref.String() {
		t.Fatalf("index.json = %+v", m.Manifests)
	}
	got, _ := lp.Image(want)
	layers, _ := got.Layers()
	mt, _ := layers[0].MediaType()
	if mt != "application/vnd.wasm.content.layer.v1+wasm" {
		t.Errorf("layer media type = %s", mt)
	}

	again, err := MaterializeLayout(Options{ImageRef: ref.String(), CacheDir: cache})
	if err != nil || !again.CacheHit {
		t.Errorf("second materialize: %+v, %v; want a cache hit", again, err)
	}
}

// An index stays an index: the layout holds the index and every image in it.
func TestMaterializeLayout_IndexIsKept(t *testing.T) {
	host := testRegistry(t)
	ref, _ := name.ParseReference(host + "/team/multi:1")
	idx, err := random.Index(64, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	want, _ := idx.Digest()
	res, err := MaterializeLayout(Options{ImageRef: ref.String(), CacheDir: writableCache(t)})
	if err != nil || res.Digest != want.String() {
		t.Fatalf("got %+v, %v; want the index digest %s", res, err, want)
	}
	lp, _ := layout.FromPath(res.RootfsPath)
	top, _ := lp.ImageIndex()
	m, _ := top.IndexManifest()
	if len(m.Manifests) != 1 || m.Manifests[0].Digest != want || !m.Manifests[0].MediaType.IsIndex() {
		t.Fatalf("index.json = %+v", m.Manifests)
	}
	inner, err := lp.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	im, _ := inner.ImageIndex(want)
	children, _ := im.IndexManifest()
	for _, c := range children.Manifests {
		if _, err := os.Stat(filepath.Join(res.RootfsPath, "blobs/sha256", c.Digest.Hex)); err != nil {
			t.Errorf("child manifest %s missing: %v", c.Digest, err)
		}
	}
}

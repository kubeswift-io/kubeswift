package materialize

import (
	"archive/tar"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// manifestOnlyRegistry serves one manifest for any repository and tag, and no
// blob: someone who copied a manifest they may read nowhere else and hosts it
// themselves.
func manifestOnlyRegistry(t *testing.T, img v1.Image) string {
	t.Helper()
	raw, _ := img.RawManifest()
	mt, _ := img.MediaType()
	d, _ := img.Digest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", string(mt))
			w.Header().Set("Docker-Content-Digest", d.String())
			w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
			if r.Method == http.MethodGet {
				w.Write(raw)
			}
		default:
			http.Error(w, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host
}

// Tenant A's artifact is cached. Tenant B resolves the same digest from a
// repository that hosts only a copy of its manifest: B's materialize fails
// and records nothing, so B can never be handed A's content. From a
// repository that serves the blobs too, it succeeds and is recorded.
func TestMaterializeLayout_CachedContentNeedsItsRepository(t *testing.T) {
	img, _ := random.Image(512, 2)
	d, _ := img.Digest()
	a, _ := name.ParseReference(testRegistry(t) + "/tenant-a/app:1")
	if err := remote.Write(a, img); err != nil {
		t.Fatal(err)
	}
	cache := writableCache(t)
	if _, err := MaterializeLayout(Options{ImageRef: a.String(), CacheDir: cache}); err != nil {
		t.Fatal(err)
	}
	if !exists(OriginMarkerPath(cache, d.String(), a.Context().Name())) {
		t.Fatal("no origin marker for the repository the entry came from")
	}

	b, _ := name.ParseReference(manifestOnlyRegistry(t, img) + "/tenant-b/copy@" + d.String())
	_, err := MaterializeLayout(Options{ImageRef: b.String(), CacheDir: cache})
	if err == nil || !strings.Contains(err.Error(), "did not serve its content") {
		t.Fatalf("manifest-only repository: err = %v", err)
	}
	if exists(OriginMarkerPath(cache, d.String(), b.Context().Name())) {
		t.Fatal("origin recorded for a repository that never served the blobs")
	}

	c, _ := name.ParseReference(testRegistry(t) + "/tenant-c/mirror:1")
	if err := remote.Write(c, img); err != nil {
		t.Fatal(err)
	}
	res, err := MaterializeLayout(Options{ImageRef: c.String(), CacheDir: cache})
	if err != nil || !res.CacheHit || !exists(OriginMarkerPath(cache, d.String(), c.Context().Name())) {
		t.Fatalf("a repository with the blobs: %+v, %v", res, err)
	}
	if fi, _ := os.Stat(cache); fi.Mode().Perm() != 0o700 {
		t.Errorf("cache root mode %v, want 0700", fi.Mode().Perm())
	}
}

// The same for an unpacked artifact.
func TestMaterialize_TreeCachedContentNeedsItsRepository(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	img, _ := treeImage(t, []*tar.Header{{Name: "app", Mode: 0o755, Size: 8, Typeflag: tar.TypeReg}})
	d, _ := img.Digest()
	a, _ := name.ParseReference(testRegistry(t) + "/tenant-a/tree:1")
	if err := remote.Write(a, img); err != nil {
		t.Fatal(err)
	}
	cache := writableCache(t)
	opts := Options{CacheDir: cache, Mode: ModeTree, ReadOnlyArtifact: true}
	opts.ImageRef = a.String()
	if _, err := Materialize(opts, nil); err != nil {
		t.Fatal(err)
	}
	// It fails as early as the image config, a blob too.
	b, _ := name.ParseReference(manifestOnlyRegistry(t, img) + "/tenant-b/copy@" + d.String())
	opts.ImageRef = b.String()
	if _, err := Materialize(opts, nil); err == nil {
		t.Fatal("manifest-only repository accepted")
	}
	if exists(OriginMarkerPath(cache, d.String(), b.Context().Name())) {
		t.Fatal("origin recorded for a repository that never served the blobs")
	}
}

// manifestAndConfigRegistry serves one image's manifest and config blob for
// any repository, and none of its layers: someone who copied both.
func manifestAndConfigRegistry(t *testing.T, img v1.Image) string {
	t.Helper()
	raw, _ := img.RawManifest()
	mt, _ := img.MediaType()
	d, _ := img.Digest()
	cfgName, _ := img.ConfigName()
	cfg, _ := img.RawConfigFile()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", string(mt))
			w.Header().Set("Docker-Content-Digest", d.String())
			w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
			if r.Method == http.MethodGet {
				w.Write(raw)
			}
		case strings.HasSuffix(r.URL.Path, "/blobs/"+cfgName.String()):
			w.Header().Set("Content-Length", strconv.Itoa(len(cfg)))
			if r.Method == http.MethodGet {
				w.Write(cfg)
			}
		default:
			http.Error(w, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host
}

// A rootfs (block or tree) and a model tree are shared by every namespace on
// the node like an artifact, and bound to their repository the same way.
// Tenant B, holding copies of tenant A's manifest and config but none of its
// layers, is refused; a repository that serves the layers is accepted.
func TestMaterialize_RootfsCachedContentNeedsItsRepository(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	for _, mode := range []Mode{ModeTree, ModeBlock} {
		t.Run(string(mode), func(t *testing.T) {
			if _, err := exec.LookPath("mkfs.ext4"); mode == ModeBlock && err != nil {
				t.Skip("mkfs.ext4 not available")
			}
			img, _ := treeImage(t, []*tar.Header{{Name: "bin/app", Mode: 0o755, Size: 8, Typeflag: tar.TypeReg}})
			d, _ := img.Digest()
			reg := testRegistry(t)
			a, _ := name.ParseReference(reg + "/tenant-a/private:1")
			if err := remote.Write(a, img); err != nil {
				t.Fatal(err)
			}
			cache := writableCache(t)
			opts := Options{CacheDir: cache, Mode: mode, ImageRef: a.String()}
			if _, err := Materialize(opts, nil); err != nil {
				t.Fatal(err)
			}
			if !exists(OriginMarkerPath(cache, d.String(), a.Context().Name())) {
				t.Fatal("no origin marker for the repository the entry came from")
			}

			b, _ := name.ParseReference(manifestAndConfigRegistry(t, img) + "/tenant-b/copy@" + d.String())
			opts.ImageRef = b.String()
			res, err := Materialize(opts, nil)
			if err == nil || !strings.Contains(err.Error(), "did not serve its content") {
				t.Fatalf("manifest and config only: res %+v, err %v", res, err)
			}
			if exists(OriginMarkerPath(cache, d.String(), b.Context().Name())) {
				t.Fatal("origin recorded for a repository that never served the layers")
			}

			c, _ := name.ParseReference(reg + "/tenant-c/mirror:1")
			if err := remote.Write(c, img); err != nil {
				t.Fatal(err)
			}
			opts.ImageRef = c.String()
			res, err = Materialize(opts, nil)
			if err != nil || !res.CacheHit || !exists(OriginMarkerPath(cache, d.String(), c.Context().Name())) {
				t.Fatalf("a repository with the layers: %+v, %v", res, err)
			}
		})
	}
}

// layerCounter wraps a registry and counts GETs of one blob.
type layerCounter struct {
	h      http.Handler
	digest string
	gets   int
}

func (l *layerCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blobs/"+l.digest) {
		l.gets++
	}
	l.h.ServeHTTP(w, r)
}

// An entry cached before entries carried markers is used after one read of
// its layers from the requesting repository, and from the marker after that:
// the upgrade costs one download per repository and entry, not a re-extract.
func TestMaterialize_UnmarkedEntryIsCheckedOnce(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	img, _ := treeImage(t, []*tar.Header{{Name: "app", Mode: 0o644, Size: 8, Typeflag: tar.TypeReg}})
	layers, _ := img.Layers()
	ld, _ := layers[0].Digest()
	counter := &layerCounter{h: registry.New(registry.Logger(log.New(io.Discard, "", 0))), digest: ld.String()}
	srv := httptest.NewServer(counter)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	ref, _ := name.ParseReference(u.Host + "/team/model:1")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	cache := writableCache(t)
	opts := Options{CacheDir: cache, Mode: ModeTree, ImageRef: ref.String()}
	if _, err := Materialize(opts, nil); err != nil {
		t.Fatal(err)
	}
	marker := OriginMarkerPath(cache, d.String(), ref.Context().Name())
	if err := os.Remove(marker); err != nil { // an entry from before markers
		t.Fatal(err)
	}
	counter.gets = 0
	res, err := Materialize(opts, nil)
	if err != nil || !res.CacheHit || !exists(marker) || counter.gets != 1 {
		t.Fatalf("unmarked entry: %+v, %v, marker %v, layer reads %d", res, err, exists(marker), counter.gets)
	}
	res, err = Materialize(opts, nil)
	if err != nil || !res.CacheHit || counter.gets != 1 {
		t.Fatalf("marked entry: %+v, %v, layer reads %d (want no new read)", res, err, counter.gets)
	}
}

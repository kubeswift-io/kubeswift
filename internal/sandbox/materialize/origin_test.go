package materialize

import (
	"archive/tar"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
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

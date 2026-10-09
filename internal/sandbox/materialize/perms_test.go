package materialize

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// writableCache returns a cache dir whose read-only entries t.TempDir can
// still remove when the test is not root.
func writableCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	return dir
}

// modes maps each path under root (relative, "." for root) to its mode.
func modes(t *testing.T, root string) map[string]fs.FileMode {
	t.Helper()
	out := map[string]fs.FileMode{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = fi.Mode()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A layout is readable by everyone and writable by no one, whatever mode each
// blob was written with. Layer blobs used to be 0600: unreadable to an
// unprivileged guest process.
func TestMaterializeLayout_ReadOnlyModes(t *testing.T) {
	host := testRegistry(t)
	ref, _ := name.ParseReference(host + "/team/app:1")
	img, err := random.Image(512, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	res, err := MaterializeLayout(Options{ImageRef: ref.String(), CacheDir: writableCache(t)})
	if err != nil {
		t.Fatal(err)
	}
	got := modes(t, res.RootfsPath)
	if len(got) < 8 { // ., blobs, blobs/sha256, 5 blobs, index.json, oci-layout
		t.Fatalf("layout too small: %v", got)
	}
	for p, m := range got {
		want := fs.FileMode(0o444)
		if m.IsDir() {
			want = fs.ModeDir | 0o555
		}
		if m != want {
			t.Errorf("%s: mode %v, want %v", p, m, want)
		}
	}
}

// An entry published by an older release (layer blobs 0600) is repaired on
// the next cache hit, which still reports a hit.
func TestMaterializeLayout_RepairsOldEntry(t *testing.T) {
	host := testRegistry(t)
	ref, _ := name.ParseReference(host + "/team/app:1")
	img, _ := random.Image(256, 1)
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	cache := writableCache(t)
	res, err := MaterializeLayout(Options{ImageRef: ref.String(), CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	// Make it look like an old entry: its own modes, and no seal marker.
	_ = filepath.WalkDir(res.RootfsPath, func(p string, d fs.DirEntry, _ error) error {
		if d.IsDir() {
			return os.Chmod(p, 0o755)
		}
		return os.Chmod(p, 0o600)
	})
	if err := os.Remove(SealedMarkerPath(res.RootfsPath)); err != nil {
		t.Fatal(err)
	}
	again, err := MaterializeLayout(Options{ImageRef: ref.String(), CacheDir: cache})
	if err != nil || !again.CacheHit {
		t.Fatalf("second materialize: %+v, %v; want a cache hit", again, err)
	}
	for p, m := range modes(t, res.RootfsPath) {
		if m.Perm()&0o444 != 0o444 || m.Perm()&0o222 != 0 {
			t.Errorf("%s not repaired: %v", p, m)
		}
	}
}

func treeImage(t *testing.T, hdrs []*tar.Header) (v1.Image, string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = tw.Close()
	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	return img, d.String()
}

var treeEntries = []*tar.Header{
	{Name: "bin/", Mode: 0o700, Typeflag: tar.TypeDir},
	{Name: "bin/tool", Mode: 0o4750, Size: 4, Typeflag: tar.TypeReg},
	{Name: "data/", Mode: 0o777, Typeflag: tar.TypeDir},
	{Name: "data/secret-looking", Mode: 0o600, Size: 2, Typeflag: tar.TypeReg},
	{Name: "data/shared", Mode: 0o666, Size: 2, Typeflag: tar.TypeReg},
	{Name: "data/link", Linkname: "shared", Typeflag: tar.TypeSymlink},
	{Name: "data/pipe", Mode: 0o644, Typeflag: tar.TypeFifo},
}

// An unpacked artifact keeps execute permission, loses write, setuid and
// special files, and is readable by everyone.
func TestMaterialize_TreeReadOnlyArtifact(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	img, digest := treeImage(t, treeEntries)
	res, err := Materialize(Options{ImageRef: "reg/x", CacheDir: writableCache(t), Mode: ModeTree, ReadOnlyArtifact: true}, stubPull(img, digest))
	if err != nil {
		t.Fatal(err)
	}
	got := modes(t, res.RootfsPath)
	want := map[string]fs.FileMode{
		".":                   fs.ModeDir | 0o555,
		"bin":                 fs.ModeDir | 0o555,
		"bin/tool":            0o555,
		"data":                fs.ModeDir | 0o555,
		"data/secret-looking": 0o444,
		"data/shared":         0o444,
	}
	for p, m := range want {
		if got[p] != m {
			t.Errorf("%s: mode %v, want %v", p, got[p], m)
		}
	}
	if got["data/link"]&fs.ModeSymlink == 0 {
		t.Errorf("symlink not kept: %v", got["data/link"])
	}
	if _, ok := got["data/pipe"]; ok {
		t.Errorf("FIFO kept in a read-only artifact")
	}
}

// The rootfs is never normalized: an image's own modes are part of it.
func TestMaterialize_TreeRootfsKeepsModes(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	img, digest := treeImage(t, treeEntries[2:5])
	res, err := Materialize(Options{ImageRef: "reg/x", CacheDir: writableCache(t), Mode: ModeTree}, stubPull(img, digest))
	if err != nil {
		t.Fatal(err)
	}
	got := modes(t, res.RootfsPath)
	if got["data/secret-looking"].Perm() != 0o600 {
		t.Errorf("rootfs file mode changed: %v", got["data/secret-looking"])
	}
}

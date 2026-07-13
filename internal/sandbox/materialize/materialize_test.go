package materialize

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestCachePathFor(t *testing.T) {
	const dig = "sha256:abc123"
	if got, want := CachePathFor("/cache", dig, ModeBlock), "/cache/sha256-abc123.ext4"; got != want {
		t.Errorf("block: got %q want %q", got, want)
	}
	if got, want := CachePathFor("/cache", dig, ModeTree), "/cache/sha256-abc123"; got != want {
		t.Errorf("tree: got %q want %q", got, want)
	}
	// The ':' from the digest must not survive into the filename.
	if p := CachePathFor("/cache", dig, ModeBlock); strings.Contains(filepath.Base(p), ":") {
		t.Errorf("digest colon leaked into filename: %q", p)
	}
}

func TestExt4SizeBytes(t *testing.T) {
	// Floor: tiny trees still get the 64 MiB minimum.
	if got := ext4SizeBytes(0); got < ext4FloorBytes {
		t.Errorf("ext4SizeBytes(0) = %d, want >= floor %d", got, ext4FloorBytes)
	}
	// Headroom: 100 MiB tree -> 100 + 50 (1.5x) + 64 (floor) = 214 MiB.
	if got, want := ext4SizeBytes(100*mib), int64(214*mib); got != want {
		t.Errorf("ext4SizeBytes(100MiB) = %d, want %d", got, want)
	}
	// Always a whole number of MiB (mkfs.ext4 takes an integer-MiB size).
	for _, tb := range []int64{1, 3, mib + 1, 123456789} {
		if got := ext4SizeBytes(tb); got%mib != 0 {
			t.Errorf("ext4SizeBytes(%d) = %d not MiB-aligned", tb, got)
		}
	}
}

// imageWithFiles builds a single-layer image whose files are world-readable
// (mode 0644) so the extract -> mkfs.ext4 path runs unit-level without root; a
// real container image's root-owned/restricted files need the root the init
// container actually runs as.
func imageWithFiles(t *testing.T, files map[string]string, cfg v1.Config) (v1.Image, string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	if err != nil {
		t.Fatalf("layer: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	img, err = mutate.Config(img, cfg)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	dig, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return img, dig.String()
}

func stubPull(img v1.Image, digest string) Puller {
	return func(_ Options) (v1.Image, string, error) { return img, digest, nil }
}

func TestConfigFromImage(t *testing.T) {
	img, _ := imageWithFiles(t, map[string]string{"app": "x"}, v1.Config{
		Entrypoint: []string{"/bin/app"},
		Cmd:        []string{"--serve"},
		Env:        []string{"A=1", "B=2"},
		WorkingDir: "/work",
		User:       "1000",
	})
	c, err := configFromImage(img)
	if err != nil {
		t.Fatalf("configFromImage: %v", err)
	}
	if len(c.Entrypoint) != 1 || c.Entrypoint[0] != "/bin/app" {
		t.Errorf("entrypoint = %v", c.Entrypoint)
	}
	if len(c.Cmd) != 1 || c.Cmd[0] != "--serve" {
		t.Errorf("cmd = %v", c.Cmd)
	}
	if len(c.Env) != 2 || c.WorkingDir != "/work" || c.User != "1000" {
		t.Errorf("config = %+v", c)
	}
}

func TestMaterialize_CacheHit(t *testing.T) {
	dir := t.TempDir()
	img, digest := imageWithFiles(t, map[string]string{"app": "x"}, v1.Config{Entrypoint: []string{"/x"}})
	// Pre-seed the cache artifact at the digest key.
	p := CachePathFor(dir, digest, ModeBlock)
	if err := os.WriteFile(p, []byte("pretend-ext4"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Materialize(Options{ImageRef: "reg/x", CacheDir: dir, Mode: ModeBlock}, stubPull(img, digest))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if !res.CacheHit {
		t.Error("expected CacheHit=true when the digest artifact already exists")
	}
	if res.RootfsPath != p {
		t.Errorf("rootfsPath = %q, want %q", res.RootfsPath, p)
	}
	if res.Digest != digest {
		t.Errorf("digest = %q, want %q", res.Digest, digest)
	}
	// The resolved config is reported even on a cache hit (the controller needs it).
	if len(res.Config.Entrypoint) != 1 || res.Config.Entrypoint[0] != "/x" {
		t.Errorf("config not reported on cache hit: %+v", res.Config)
	}
}

// TestMaterialize_BlockMissPath exercises the full flatten -> tar -> mkfs.ext4
// path with a synthetic image. Skipped where the tools are unavailable (minimal
// CI); the world-readable synthetic files let it run without root.
func TestMaterialize_BlockMissPath(t *testing.T) {
	for _, bin := range []string{"mkfs.ext4", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available; skipping full materialize path", bin)
		}
	}
	dir := t.TempDir()
	img, digest := imageWithFiles(t, map[string]string{
		"etc/hostname": "sandbox\n",
		"bin/run":      "#!/bin/sh\necho hi\n",
	}, v1.Config{Entrypoint: []string{"/bin/run"}})

	res, err := Materialize(Options{ImageRef: "reg/x", CacheDir: dir, Mode: ModeBlock}, stubPull(img, digest))
	if err != nil {
		t.Fatalf("Materialize (miss): %v", err)
	}
	if res.CacheHit {
		t.Error("first materialize must be a cache miss")
	}
	if fi, err := os.Stat(res.RootfsPath); err != nil || fi.Size() == 0 {
		t.Fatalf("ext4 not produced at %s: err=%v", res.RootfsPath, err)
	}
	if res.SizeBytes < ext4FloorBytes {
		t.Errorf("size %d below floor", res.SizeBytes)
	}
	// No partial temp dirs left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".materialize-") {
			t.Errorf("leftover temp dir: %s", e.Name())
		}
	}

	// Second call = cache hit (no re-extract).
	res2, err := Materialize(Options{ImageRef: "reg/x", CacheDir: dir, Mode: ModeBlock}, stubPull(img, digest))
	if err != nil {
		t.Fatalf("Materialize (hit): %v", err)
	}
	if !res2.CacheHit {
		t.Error("second materialize of the same digest must be a cache hit")
	}
}

// TestLockDigest_Serializes verifies the node-local per-digest lock actually
// blocks a second holder until the first releases (the warm-pool dedup guard).
func TestLockDigest_Serializes(t *testing.T) {
	dir := t.TempDir()
	const dig = "sha256:deadbeef"

	unlock1, err := lockDigest(dir, dig)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	got := make(chan struct{})
	go func() {
		unlock2, err := lockDigest(dir, dig)
		if err != nil {
			t.Errorf("second lock: %v", err)
			return
		}
		close(got)
		unlock2()
	}()

	// While the first lock is held, the second must block.
	select {
	case <-got:
		t.Fatal("second lock acquired while first still held")
	case <-time.After(150 * time.Millisecond):
	}

	// Release the first; the second must now acquire promptly.
	unlock1()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock did not acquire after first released")
	}
}

// TestLockDigest_DifferentDigests confirms independent digests do not block each
// other (only same-image same-node materializes serialize).
func TestLockDigest_DifferentDigests(t *testing.T) {
	dir := t.TempDir()
	u1, err := lockDigest(dir, "sha256:aaaa")
	if err != nil {
		t.Fatalf("lock a: %v", err)
	}
	defer u1()
	done := make(chan struct{})
	go func() {
		u2, err := lockDigest(dir, "sha256:bbbb")
		if err == nil {
			u2()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("distinct digest lock blocked unexpectedly")
	}
}

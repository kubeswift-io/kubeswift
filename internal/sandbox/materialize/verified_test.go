package materialize

import (
	"archive/tar"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func TestRecordVerified_KeyedByTheKeyBytes(t *testing.T) {
	cache := t.TempDir()
	keyA := filepath.Join(t.TempDir(), "a.pub")
	keyB := filepath.Join(t.TempDir(), "b.pub")
	_ = os.WriteFile(keyA, []byte("-----BEGIN PUBLIC KEY-----\nA\n"), 0o600)
	_ = os.WriteFile(keyB, []byte("-----BEGIN PUBLIC KEY-----\nB\n"), 0o600)
	if err := RecordVerified(cache, testDigest, keyA); err != nil {
		t.Fatal(err)
	}
	if err := RecordVerified(cache, testDigest, keyA); err != nil { // idempotent
		t.Fatal(err)
	}
	a, _ := os.ReadFile(keyA)
	b, _ := os.ReadFile(keyB)
	fi, err := os.Stat(VerifiedMarkerPath(cache, testDigest, KeyFingerprint(a)))
	if err != nil || fi.Mode().Perm() != 0o444 {
		t.Fatalf("marker for key A: %v %v", fi, err)
	}
	if _, err := os.Stat(VerifiedMarkerPath(cache, testDigest, KeyFingerprint(b))); err == nil {
		t.Error("a check with key A satisfies key B")
	}
	if !strings.Contains(VerifiedMarkerPath(cache, testDigest, "f"), ".verified/sha256-1111") {
		t.Error("marker path layout changed (swiftletd reads it)")
	}
	if err := RecordVerified(cache, "sha256:../../etc", keyA); err == nil {
		t.Error("accepted a digest that is not sha256 hex")
	}
}

// An OCI layout larger than the cap is refused before a blob is written.
func TestMaterializeLayout_SizeCap(t *testing.T) {
	host := testRegistry(t)
	ref, _ := name.ParseReference(host + "/team/big:1")
	img, _ := random.Image(4096, 2)
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	cache := writableCache(t)
	_, err := MaterializeLayout(Options{ImageRef: ref.String(), CacheDir: cache, MaxBytes: 1024})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if entries, _ := os.ReadDir(cache); len(entries) > 1 { // the lock file only
		t.Errorf("cache written: %v", entries)
	}
	if _, err := MaterializeLayout(Options{ImageRef: ref.String(), CacheDir: cache, MaxBytes: 1 << 20}); err != nil {
		t.Errorf("under the cap: %v", err)
	}
}

// An unpacked artifact is stopped at the cap while extracting: a layer small
// on the wire can unpack to far more (a decompression bomb).
func TestMaterialize_TreeSizeCap(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	img, digest := treeImage(t, []*tar.Header{{Name: "zeros", Mode: 0o644, Size: 4 << 20, Typeflag: tar.TypeReg}})
	_, err := Materialize(Options{ImageRef: "reg/x", CacheDir: writableCache(t), Mode: ModeTree, ReadOnlyArtifact: true, MaxBytes: 1 << 20}, stubPull(img, digest))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// The rootfs is never capped.
	if _, err := Materialize(Options{ImageRef: "reg/x", CacheDir: writableCache(t), Mode: ModeTree, MaxBytes: 1 << 20}, stubPull(img, digest)); err != nil {
		t.Errorf("rootfs tree capped: %v", err)
	}
}

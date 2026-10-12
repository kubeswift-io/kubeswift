package materialize

import (
	"archive/tar"
	"errors"
	"io"
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
	a := []byte("-----BEGIN PUBLIC KEY-----\nA\n")
	b := []byte("-----BEGIN PUBLIC KEY-----\nB\n")
	if err := RecordVerified(cache, testDigest, a); err != nil {
		t.Fatal(err)
	}
	if err := RecordVerified(cache, testDigest, a); err != nil { // idempotent
		t.Fatal(err)
	}
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
	if err := RecordVerified(cache, "sha256:../../etc", a); err == nil {
		t.Error("accepted a digest that is not sha256 hex")
	}
	if err := RecordVerified(cache, testDigest, nil); err == nil {
		t.Error("recorded a verification with no key")
	}
}

// What a check reads is a private copy of the caller's bytes: replacing the
// original file afterwards (a Secret volume updated mid-run) changes neither
// the copy the check used nor the fingerprint recorded for it.
func TestPinFile_IsAPrivateCopy(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "cosign.pub")
	keyA := []byte("-----BEGIN PUBLIC KEY-----\nA\n")
	_ = os.WriteFile(orig, keyA, 0o644)
	read, _ := os.ReadFile(orig)
	pinned, err := PinFile(dir, "verify-key-*.pub", read)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(orig, []byte("-----BEGIN PUBLIC KEY-----\nVICTIM\n"), 0o644)
	got, _ := os.ReadFile(pinned)
	if string(got) != string(keyA) {
		t.Fatalf("pinned copy changed with the original: %q", got)
	}
	fi, _ := os.Stat(pinned)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("pinned copy mode %v, want 0600", fi.Mode().Perm())
	}
	cache := t.TempDir()
	if err := RecordVerified(cache, testDigest, read); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(VerifiedMarkerPath(cache, testDigest, KeyFingerprint([]byte("-----BEGIN PUBLIC KEY-----\nVICTIM\n")))); err == nil {
		t.Error("the replaced key's fingerprint was recorded")
	}
}

// A pull Secret's credential helpers are dropped: only auths survive.
func TestSanitizeDockerConfig_DropsHelpers(t *testing.T) {
	in := []byte(`{"auths":{"reg.example":{"auth":"dTpw"}},"credsStore":"evil","credHelpers":{"reg.example":"evil"},"proxies":{}}`)
	out, err := SanitizeDockerConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "credsStore") || strings.Contains(s, "credHelpers") || strings.Contains(s, "evil") || strings.Contains(s, "proxies") {
		t.Errorf("helpers survived: %s", s)
	}
	if !strings.Contains(s, `"reg.example"`) {
		t.Errorf("auths lost: %s", s)
	}
	if out, err := SanitizeDockerConfig([]byte(`{"credsStore":"evil"}`)); err != nil || string(out) != `{"auths":{}}` {
		t.Errorf("no auths: %s %v", out, err)
	}
	if _, err := SanitizeDockerConfig([]byte("not json")); err == nil {
		t.Error("accepted invalid JSON")
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

// Exactly the cap is allowed; one byte more is not.
func TestCappedReader_ExactlyTheCap(t *testing.T) {
	for _, c := range []struct {
		n    int
		fail bool
	}{{100, false}, {101, true}, {0, false}} {
		r := &cappedReader{r: strings.NewReader(strings.Repeat("x", c.n)), left: 100}
		_, err := io.ReadAll(r)
		if (err != nil) != c.fail || r.exceeded != c.fail {
			t.Errorf("%d bytes under a 100 cap: err %v exceeded %v", c.n, err, r.exceeded)
		}
	}
}

// An unpacked artifact of many empty files is stopped at the entry cap,
// however few bytes it is.
func TestMaterialize_TreeEntryCap(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	var hdrs []*tar.Header
	for i := 0; i < 50; i++ {
		hdrs = append(hdrs, &tar.Header{Name: "f" + strings.Repeat("x", i), Mode: 0o644, Typeflag: tar.TypeReg})
	}
	img, digest := treeImage(t, hdrs)
	opts := Options{ImageRef: "reg/x", Mode: ModeTree, ReadOnlyArtifact: true, MaxEntries: 20}
	opts.CacheDir = writableCache(t)
	if _, err := Materialize(opts, stubPull(img, digest)); !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("err = %v, want ErrTooManyEntries", err)
	}
	opts.CacheDir, opts.MaxEntries = writableCache(t), 50
	if _, err := Materialize(opts, stubPull(img, digest)); err != nil {
		t.Errorf("at the cap: %v", err)
	}
}

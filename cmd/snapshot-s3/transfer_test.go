package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeStore is an in-memory objectStore for round-trip + idempotency tests.
type fakeStore struct {
	objs map[string][]byte
	meta map[string]string // key -> recorded sha256 (mirrors x-amz-meta-sha256)
	puts int
	gets int
}

func newFakeStore() *fakeStore {
	return &fakeStore{objs: map[string][]byte{}, meta: map[string]string{}}
}

func (f *fakeStore) stat(_ context.Context, key string) (int64, string, bool, error) {
	b, ok := f.objs[key]
	if !ok {
		return 0, "", false, nil
	}
	return int64(len(b)), f.meta[key], true, nil
}
func (f *fakeStore) put(_ context.Context, key string, r io.Reader, _ int64, sha256 string) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objs[key] = data
	f.meta[key] = sha256
	f.puts++
	return nil
}
func (f *fakeStore) get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := f.objs[key]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s", key)
	}
	f.gets++
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f *fakeStore) remove(_ context.Context, key string) error { delete(f.objs, key); return nil }
func (f *fakeStore) list(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for k := range f.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func writeFile(t *testing.T, dir, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedSnapshotDir(t *testing.T) (dir string, files map[string][]byte) {
	t.Helper()
	dir = t.TempDir()
	files = map[string][]byte{
		"config.json":    []byte(`{"cpus":2}`),
		"memory.img":     bytes.Repeat([]byte("M"), 4096),
		"disks/root.raw": bytes.Repeat([]byte("D"), 8192),
	}
	for rel, c := range files {
		writeFile(t, dir, rel, c)
	}
	return dir, files
}

func TestUploadDownload_RoundTrip(t *testing.T) {
	ctx := context.Background()
	src, files := seedSnapshotDir(t)
	store := newFakeStore()

	if _, err := runUpload(ctx, store, src, "ns/snap", "ns/snap", true); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// manifest + 3 artifacts uploaded.
	if _, ok := store.objs["ns/snap/manifest.json"]; !ok {
		t.Fatal("manifest not uploaded")
	}
	if store.puts != 4 {
		t.Errorf("expected 4 puts (3 artifacts + manifest); got %d", store.puts)
	}

	dst := t.TempDir()
	if _, err := runDownload(ctx, store, dst, "ns/snap"); err != nil {
		t.Fatalf("download: %v", err)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s content mismatch after round-trip", rel)
		}
	}
}

// TestTransferStats verifies the byte report the controller reads back: the
// first upload transfers everything; a resumed upload skips everything; a
// download mirrors it. transferred + skipped == total throughout.
func TestTransferStats(t *testing.T) {
	ctx := context.Background()
	src, files := seedSnapshotDir(t)
	var total int64
	for _, c := range files {
		total += int64(len(c))
	}
	store := newFakeStore()

	up, err := runUpload(ctx, store, src, "ns/snap", "ns/snap", false)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if up.TransferredBytes != total || up.SkippedBytes != 0 || up.TotalBytes != total {
		t.Errorf("first upload stats = %+v, want transferred=%d skipped=0 total=%d", up, total, total)
	}

	up2, err := runUpload(ctx, store, src, "ns/snap", "ns/snap", false)
	if err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if up2.TransferredBytes != 0 || up2.SkippedBytes != total {
		t.Errorf("resumed upload stats = %+v, want transferred=0 skipped=%d", up2, total)
	}

	dst := t.TempDir()
	dl, err := runDownload(ctx, store, dst, "ns/snap")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if dl.TransferredBytes != total || dl.SkippedBytes != 0 {
		t.Errorf("download stats = %+v, want transferred=%d skipped=0", dl, total)
	}
	if dl.TransferredBytes+dl.SkippedBytes != dl.TotalBytes {
		t.Errorf("invariant transferred+skipped==total violated: %+v", dl)
	}
}

func TestUpload_IdempotentSkipsExisting(t *testing.T) {
	ctx := context.Background()
	src, _ := seedSnapshotDir(t)
	store := newFakeStore()
	if _, err := runUpload(ctx, store, src, "ns/snap", "ns/snap", false); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	first := store.puts
	// Re-upload: artifacts already present with matching size -> skipped; only
	// the manifest is re-put.
	if _, err := runUpload(ctx, store, src, "ns/snap", "ns/snap", false); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if delta := store.puts - first; delta != 1 {
		t.Errorf("re-upload should only re-put the manifest (1 put); got %d extra puts", delta)
	}
}

// TestUpload_ReuploadsStaleSameSizeContent reproduces the cluster-walkthrough
// bug: a memory-ranges file is always exactly the guest's RAM size, so a second
// snapshot reusing the same key prefix (a deleted+recreated same-named snapshot)
// with same-size-but-different-content must RE-UPLOAD it — a size-only resume
// check would keep the stale object while the manifest records the new hash, and
// the restore would then fail verification forever.
func TestUpload_ReuploadsStaleSameSizeContent(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	// First snapshot at ns/snap.
	src1, _ := seedSnapshotDir(t)
	if _, err := runUpload(ctx, store, src1, "ns/snap", "ns/snap", false); err != nil {
		t.Fatalf("first upload: %v", err)
	}

	// Second snapshot reusing the SAME key prefix: memory.img is the SAME 4096
	// bytes but DIFFERENT content; config.json + root.raw are unchanged.
	src2 := t.TempDir()
	writeFile(t, src2, "config.json", []byte(`{"cpus":2}`))
	writeFile(t, src2, "memory.img", bytes.Repeat([]byte("X"), 4096))
	writeFile(t, src2, "disks/root.raw", bytes.Repeat([]byte("D"), 8192))
	if _, err := runUpload(ctx, store, src2, "ns/snap", "ns/snap", false); err != nil {
		t.Fatalf("second upload: %v", err)
	}

	// memory.img must now hold the NEW content (re-uploaded), not the stale "M".
	if got := store.objs["ns/snap/memory.img"]; !bytes.Equal(got, bytes.Repeat([]byte("X"), 4096)) {
		t.Fatalf("memory.img kept stale content; size-only skip not fixed (len=%d, first byte=%q)", len(got), got[0])
	}
	// And a fresh download must verify cleanly against the new manifest.
	dst := t.TempDir()
	if _, err := runDownload(ctx, store, dst, "ns/snap"); err != nil {
		t.Fatalf("download after re-upload must verify clean: %v", err)
	}
}

func TestDownload_IdempotentSkipsVerified(t *testing.T) {
	ctx := context.Background()
	src, _ := seedSnapshotDir(t)
	store := newFakeStore()
	_, _ = runUpload(ctx, store, src, "ns/snap", "ns/snap", false)

	dst := t.TempDir()
	if _, err := runDownload(ctx, store, dst, "ns/snap"); err != nil {
		t.Fatalf("first download: %v", err)
	}
	getsAfterFirst := store.gets
	if _, err := runDownload(ctx, store, dst, "ns/snap"); err != nil {
		t.Fatalf("second download: %v", err)
	}
	// Second download re-reads only the manifest; artifacts already verify.
	if delta := store.gets - getsAfterFirst; delta != 1 {
		t.Errorf("re-download should only re-get the manifest (1 get); got %d extra gets", delta)
	}
}

func TestDownload_DetectsCorruption(t *testing.T) {
	ctx := context.Background()
	src, _ := seedSnapshotDir(t)
	store := newFakeStore()
	_, _ = runUpload(ctx, store, src, "ns/snap", "ns/snap", false)

	// Corrupt one artifact object (size preserved, content changed) so only the
	// sha256 check can catch it.
	store.objs["ns/snap/disks/root.raw"] = bytes.Repeat([]byte("X"), 8192)

	dst := t.TempDir()
	_, err := runDownload(ctx, store, dst, "ns/snap")
	if err == nil {
		t.Fatal("download must fail verification on a corrupt artifact")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("verification")) &&
		!bytes.Contains([]byte(err.Error()), []byte("sha256")) {
		t.Errorf("error should mention verification/sha256; got %v", err)
	}
}

// TestRunDelete removes every object under the snapshot prefix and refuses an
// empty prefix (whole-bucket blast-radius guard).
func TestRunDelete(t *testing.T) {
	ctx := context.Background()
	src, _ := seedSnapshotDir(t)
	store := newFakeStore()
	if _, err := runUpload(ctx, store, src, "ns/snap", "ns/snap", false); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// An unrelated object under a different prefix must survive.
	store.objs["other/keep.bin"] = []byte("keep")

	if err := runDelete(ctx, store, "ns/snap"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for k := range store.objs {
		if k != "other/keep.bin" {
			t.Errorf("object %q under the deleted prefix survived", k)
		}
	}
	if _, ok := store.objs["other/keep.bin"]; !ok {
		t.Error("delete must not touch objects outside the snapshot prefix")
	}

	// Empty / whitespace prefix is refused (would target the whole bucket).
	for _, bad := range []string{"", "  ", "/"} {
		if err := runDelete(ctx, store, bad); err == nil {
			t.Errorf("empty-ish prefix %q must be refused", bad)
		}
	}
}

func TestRunArgs_Validate(t *testing.T) {
	for _, ok := range []runArgs{
		{mode: "upload", dir: "/d", bucket: "b", keyPrefix: "p"},
		{mode: "download", dir: "/d", bucket: "b", keyPrefix: "p"},
		{mode: "delete", bucket: "b", keyPrefix: "p"}, // delete needs no dir
	} {
		if err := ok.validate(); err != nil {
			t.Errorf("valid args rejected: %+v: %v", ok, err)
		}
	}
	for _, bad := range []runArgs{
		{mode: "sideways", dir: "/d", bucket: "b", keyPrefix: "p"},
		{mode: "upload", bucket: "b", keyPrefix: "p"},              // no dir
		{mode: "upload", dir: "/d", keyPrefix: "p"},                // no bucket
		{mode: "delete", bucket: "b"},                              // no key-prefix
		{mode: "delete", keyPrefix: "p"},                           // no bucket
		{mode: "download", dir: "/d", bucket: "b", insecure: true}, // insecure + default endpoint
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("invalid args accepted: %+v", bad)
		}
	}
}

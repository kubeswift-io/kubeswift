package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
)

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestRoundTrip_ByteIdentical packs a snapshot dir into an in-memory oras
// target, pulls it back to a fresh dir, and asserts every file returned
// byte-identical — the OCI-middle fidelity the spike proved on a real registry,
// here hermetically (no cluster).
func TestRoundTrip_ByteIdentical(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, src, "config.json", []byte(`{"cpus":2,"memory":2048}`))
	writeFile(t, src, "state.json", []byte(`{"vcpus":[0,1]}`))
	// A non-trivial "memory-ranges" blob.
	mem := make([]byte, 1<<20)
	for i := range mem {
		mem[i] = byte(i * 7)
	}
	writeFile(t, src, "memory-ranges", mem)

	want := map[string]string{}
	for _, n := range []string{"config.json", "state.json", "memory-ranges"} {
		want[n] = sha256File(t, filepath.Join(src, n))
	}

	store := memory.New()
	desc, stats, err := packAndPush(ctx, src, store, "v1", "ns/snap", true)
	if err != nil {
		t.Fatalf("packAndPush: %v", err)
	}
	if stats.TransferredBytes == 0 || stats.ManifestDigest == "" {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if int(stats.TotalBytes) < len(mem) {
		t.Errorf("totalBytes %d < memory size %d", stats.TotalBytes, len(mem))
	}
	if desc.Digest.String() != stats.ManifestDigest {
		t.Errorf("manifest digest mismatch: desc=%s stats=%s", desc.Digest, stats.ManifestDigest)
	}

	dst := filepath.Join(t.TempDir(), "restore")
	pulled, err := pullAndMaterialize(ctx, store, "v1", dst)
	if err != nil {
		t.Fatalf("pullAndMaterialize: %v", err)
	}
	if pulled.Digest != desc.Digest {
		t.Errorf("pulled manifest %s != pushed %s", pulled.Digest, desc.Digest)
	}
	for n, w := range want {
		got := sha256File(t, filepath.Join(dst, n))
		if got != w {
			t.Errorf("%s: round-trip sha mismatch: got %s want %s", n, got, w)
		}
	}
}

// TestPush_IdenticalContentDedups pushes the same dir twice; the second push
// must skip the already-present layers (dedup by digest) rather than re-transfer
// them — the byte-accounting the controller stamps as pushedBytes.
func TestPush_IdenticalContentDedups(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, src, "config.json", []byte(`{"cpus":2}`))
	writeFile(t, src, "memory-ranges", make([]byte, 512<<10))

	store := memory.New()
	if _, _, err := packAndPush(ctx, src, store, "v1", "", false); err != nil {
		t.Fatalf("first push: %v", err)
	}
	_, stats2, err := packAndPush(ctx, src, store, "v2", "", false)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if stats2.SkippedBytes == 0 {
		t.Errorf("second push of identical content should skip (dedup) layers; stats=%+v", stats2)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		a    runArgs
		ok   bool
	}{
		{"upload ok", runArgs{mode: "upload", dir: "/snap", repository: "r/x", tag: "t"}, true},
		{"download ok", runArgs{mode: "download", dir: "/snap", repository: "r/x", tag: "t"}, true},
		{"delete needs no dir", runArgs{mode: "delete", repository: "r/x", digest: "sha256:abc"}, true},
		{"delete needs a digest, not a tag", runArgs{mode: "delete", repository: "r/x", tag: "t"}, false},
		{"upload needs dir", runArgs{mode: "upload", repository: "r/x", tag: "t"}, false},
		{"needs repository", runArgs{mode: "upload", dir: "/snap", tag: "t"}, false},
		{"needs tag", runArgs{mode: "upload", dir: "/snap", repository: "r/x"}, false},
		{"bad mode", runArgs{mode: "wat", repository: "r/x", tag: "t"}, false},
	}
	for _, c := range cases {
		err := c.a.validate()
		if c.ok && err != nil {
			t.Errorf("%s: want ok, got %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: want error, got nil", c.name)
		}
	}
}

// Two snapshots given the same tag push to it in turn. Deleting the older one
// used to resolve the tag, which by then named the newer artifact, and delete
// that (#705). By digest, only the older artifact goes and the tag keeps the
// newer one. Runs against a real (in-process) registry over HTTP.
func TestDeleteArtifact_ByDigestSparesAMovedTag(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	repo, err := remote.NewRepository(strings.TrimPrefix(srv.URL, "http://") + "/vm-snapshots")
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true

	push := func(content string) string {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, dir, "config.json", []byte(content))
		_, stats, err := packAndPush(ctx, dir, repo, "shared", "ns/snap", false)
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		return stats.ManifestDigest
	}
	older := push(`{"capture":1}`)
	newer := push(`{"capture":2}`) // the tag now names this one
	if older == newer {
		t.Fatal("the two pushes produced the same manifest")
	}

	if err := deleteArtifact(ctx, repo, older); err != nil {
		t.Fatalf("delete older: %v", err)
	}
	if _, err := repo.Resolve(ctx, older); !errors.Is(err, errdef.ErrNotFound) {
		t.Errorf("older artifact still resolvable after its delete (err=%v)", err)
	}
	if d, err := repo.Resolve(ctx, "shared"); err != nil || d.Digest.String() != newer {
		t.Errorf("the tag no longer names the newer artifact: digest=%v err=%v", d.Digest, err)
	}
	if _, err := repo.Resolve(ctx, newer); err != nil {
		t.Errorf("newer artifact was deleted along with the older one: %v", err)
	}
	// Idempotent: an artifact already gone is success.
	if err := deleteArtifact(ctx, repo, older); err != nil {
		t.Errorf("second delete of the same digest: %v", err)
	}
}

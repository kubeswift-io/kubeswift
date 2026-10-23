package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// An optional artifact already ready on the node is used without the
// registry while the controller's authorization stands, even past its
// deadline (fetchTimeout 0s, or an init container that started late). It used
// to be skipped as FetchTimeout: the run asked the registry first, with an
// expired context. Past the authorization, or for a required artifact, the
// registry is asked as before.
func TestOptionalArtifact_ReadyOnTheNodeNeedsNoRegistry(t *testing.T) {
	bin := binary(t)
	srv := httptest.NewServer(registry.New())
	host := strings.TrimPrefix(srv.URL, "http://")
	img, _ := random.Image(256, 1)
	ref, _ := name.ParseReference(host + "/art:1")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	dig, _ := img.Digest()
	pinned := host + "/art@" + dig.String()

	cache := filepath.Join(t.TempDir(), "cache")
	t.Cleanup(func() {
		_ = filepath.Walk(cache, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	if code, res, out := materializeRun(t, bin, "--cache-dir", cache, "--optional", "--image", pinned); code != 0 || res.Skipped != nil {
		t.Fatalf("fill the cache: code=%d res=%+v out=%s", code, res, out)
	}
	srv.Close()

	past := time.Now().Add(-time.Minute).Format(time.RFC3339)
	future := time.Now().Add(5 * time.Minute).Format(time.RFC3339)

	code, res, out := materializeRun(t, bin, "--cache-dir", cache, "--optional", "--image", pinned,
		"--deadline", past, "--authorized-until", future)
	if code != 0 || res.Skipped != nil || !res.CacheHit || res.Digest != dig.String() || !strings.HasPrefix(out, "ok:") {
		t.Fatalf("ready on the node, deadline passed: code=%d res=%+v out=%s", code, res, out)
	}

	code, res, out = materializeRun(t, bin, "--cache-dir", cache, "--optional", "--image", pinned,
		"--deadline", past, "--authorized-until", past)
	if code != 0 || res.Skipped == nil || res.Skipped.Reason != "FetchTimeout" || strings.HasPrefix(out, "ok:") {
		t.Fatalf("authorization expired: code=%d res=%+v out=%s", code, res, out)
	}

	// Unverified for the key it names: the registry is needed (cosign needs
	// TLS, so this one is refused at verification), never the cache.
	key := filepath.Join(t.TempDir(), "cosign.pub")
	if err := os.WriteFile(key, []byte("not recorded"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, res, out = materializeRun(t, bin, "--cache-dir", cache, "--optional", "--image", pinned,
		"--verify-key", key, "--authorized-until", future)
	if code != 0 || res.Skipped == nil || strings.HasPrefix(out, "ok:") {
		t.Fatalf("unverified for its key: code=%d res=%+v out=%s", code, res, out)
	}

	// A required artifact still asks the registry.
	code, _, out = materializeRun(t, bin, "--cache-dir", cache, "--image", pinned, "--authorized-until", future, "--timeout", "5s")
	if code == 0 {
		t.Fatalf("required artifact used the cache without the registry: out=%s", out)
	}
}

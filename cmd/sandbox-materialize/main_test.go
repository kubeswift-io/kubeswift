package main

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// binary builds sandbox-materialize once per test run.
func binary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sandbox-materialize")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// materializeRun runs the binary and returns its exit code and result.
func materializeRun(t *testing.T, bin string, args ...string) (int, materialize.Result, string) {
	t.Helper()
	dir := t.TempDir()
	// Entries are sealed read-only; let TempDir's cleanup remove them.
	t.Cleanup(func() {
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	result := filepath.Join(dir, "result.json")
	ok := filepath.Join(dir, "ok")
	cmd := exec.Command(bin, append([]string{"--cache-dir", filepath.Join(dir, "cache"), "--mode", "oci",
		"--insecure", "--result-file", result, "--ok-file", ok}, args...)...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, isExit := err.(*exec.ExitError)
		if !isExit {
			t.Fatalf("run: %v", err)
		}
		code = ee.ExitCode()
	}
	var res materialize.Result
	if b, err := os.ReadFile(result); err == nil {
		_ = json.Unmarshal(b, &res)
	}
	if _, err := os.Stat(ok); err == nil {
		return code, res, "ok:" + string(out)
	}
	return code, res, string(out)
}

func TestOptionalArtifact(t *testing.T) {
	bin := binary(t)
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	img, _ := random.Image(256, 1)
	ref, _ := name.ParseReference(host + "/art:1")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	dig, _ := img.Digest()

	// Present: materialized and marked ok.
	code, res, out := materializeRun(t, bin, "--optional", "--image", host+"/art@"+dig.String())
	if code != 0 || res.Skipped != nil || res.Digest != dig.String() || !strings.HasPrefix(out, "ok:") {
		t.Fatalf("present: code=%d res=%+v out=%s", code, res, out)
	}

	// Missing: skipped as refused, exit 0, no ok file.
	code, res, out = materializeRun(t, bin, "--optional", "--image", host+"/missing:1")
	if code != 0 || res.Skipped == nil || res.Skipped.Reason != "ResolveRefused" || strings.HasPrefix(out, "ok:") {
		t.Fatalf("missing: code=%d res=%+v out=%s", code, res, out)
	}

	// Required and missing: the run still fails, as before.
	code, res, _ = materializeRun(t, bin, "--image", host+"/missing:1")
	if code == 0 || res.Skipped != nil {
		t.Fatalf("required missing: code=%d res=%+v", code, res)
	}

	// A registry that never answers: skipped at the deadline.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold the connection open, answer nothing
		}
	}()
	start := time.Now()
	code, res, out = materializeRun(t, bin, "--optional", "--image", l.Addr().String()+"/art:1",
		"--deadline", time.Now().Add(2*time.Second).Format(time.RFC3339))
	if code != 0 || res.Skipped == nil || res.Skipped.Reason != "FetchTimeout" || strings.HasPrefix(out, "ok:") {
		t.Fatalf("hung registry: code=%d res=%+v out=%s", code, res, out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("deadline not honoured: took %v", d)
	}
}

func TestSkipReason_VerificationIsNeverRetriedWithout(t *testing.T) {
	// A verification failure is reported as such (never folded into a
	// transient reason a caller might retry without verification).
	if r := skipReason(t.Context(), stageVerify, os.ErrInvalid); r != "VerificationFailed" {
		t.Errorf("reason %s", r)
	}
	if r := skipReason(t.Context(), stageMaterialize, materialize.ErrTooLarge); r != "MaterializeFailed" {
		t.Errorf("reason %s", r)
	}
}

// The node cache stays within its limit: a second artifact evicts the first,
// least recently used; held by a launcher, the first stays and the second is
// refused.
func TestCacheLimit(t *testing.T) {
	bin := binary(t)
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	push := func(tag string) string {
		img, _ := random.Image(64*1024, 1)
		ref, _ := name.ParseReference(host + "/art:" + tag)
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		return host + "/art@" + d.String()
	}
	a, b := push("a"), push("b")
	dir := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	cache, use := filepath.Join(dir, "cache"), filepath.Join(dir, "use")
	runOne := func(ref string, limit int64) (int, materialize.Result) {
		result := filepath.Join(dir, "result.json")
		_ = os.Remove(result)
		cmd := exec.Command(bin, "--image", ref, "--cache-dir", cache, "--mode", "oci", "--insecure",
			"--use-dir", use, "--cache-max-bytes", strconv.FormatInt(limit, 10), "--cache-min-age", "0s", "--result-file", result)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}
		var res materialize.Result
		if b, err := os.ReadFile(result); err == nil {
			_ = json.Unmarshal(b, &res)
		}
		return code, res
	}
	code, resA := runOne(a, 1<<30)
	if code != 0 {
		t.Fatalf("first artifact: exit %d", code)
	}
	entryA := filepath.Base(resA.RootfsPath)
	// About one entry fits: the second evicts the first.
	code, resB := runOne(b, 150*1024)
	if code != 0 || resB.Cache == nil || resB.Cache.Evicted != 1 {
		t.Fatalf("second artifact: exit %d cache %+v", code, resB.Cache)
	}
	if _, err := os.Stat(filepath.Join(cache, entryA)); err == nil {
		t.Fatal("least recently used entry still there")
	}

	// A launcher holds b: a needs room, cannot evict b, and is refused.
	f, err := os.Open(materialize.UseLockPath(use, filepath.Base(resB.RootfsPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	code, resA = runOne(a, 150*1024)
	if code == 0 {
		t.Fatalf("a full cache accepted an entry: %+v", resA.Cache)
	}
	if _, err := os.Stat(filepath.Join(cache, filepath.Base(resB.RootfsPath))); err != nil {
		t.Fatal("an entry in use was evicted")
	}
}

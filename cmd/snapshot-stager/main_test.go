package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMockSnapshot creates a directory layout that resembles a CH
// snapshot: config.json with a payload.cmdline and a config.net[]
// containing one virtio-net device, plus opaque state.json /
// memory-ranges placeholders.
func writeMockSnapshot(t *testing.T, dir string, cmdline, mac string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	cfg := map[string]any{
		"config": map[string]any{
			"payload": map[string]any{"cmdline": cmdline},
			"net": []any{
				map[string]any{"id": "_net0", "tap": "tap0", "mac": mac},
			},
			"disks": []any{
				map[string]any{"path": "/var/lib/kubeswift/disks/root/image.raw"},
			},
		},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("opaque-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory-ranges"), []byte("opaque-memory"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readPatchedCmdlineAndMAC(t *testing.T, dir string) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	c := cfg["config"].(map[string]any)
	pl := c["payload"].(map[string]any)
	net := c["net"].([]any)
	dev := net[0].(map[string]any)
	cmdline, _ := pl["cmdline"].(string)
	mac, _ := dev["mac"].(string)
	return cmdline, mac
}

func TestRun_AppliesPatchesAndWritesSentinel(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeMockSnapshot(t, src, "console=ttyS0 root=/dev/vda", "52:54:00:01:01:01")

	if err := run(src, dst, true, []string{"52:54:00:aa:bb:cc"}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// All snapshot files copied.
	for _, f := range []string{"config.json", "state.json", "memory-ranges", ".copy-complete"} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Errorf("expected %s in dst, err=%v", f, err)
		}
	}
	// Patches applied.
	cmdline, mac := readPatchedCmdlineAndMAC(t, dst)
	if !strings.Contains(cmdline, "kubeswift.clone=true") {
		t.Errorf("cmdline missing marker: %q", cmdline)
	}
	if mac != "52:54:00:aa:bb:cc" {
		t.Errorf("mac = %q, want 52:54:00:aa:bb:cc", mac)
	}
	// Source untouched (read-only contract).
	srcCmdline, srcMAC := readPatchedCmdlineAndMAC(t, src)
	if strings.Contains(srcCmdline, "kubeswift.clone=true") {
		t.Errorf("source cmdline was mutated: %q", srcCmdline)
	}
	if srcMAC != "52:54:00:01:01:01" {
		t.Errorf("source mac was mutated: %q", srcMAC)
	}
}

func TestRun_SentinelPresentIsIdempotentNoOp(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeMockSnapshot(t, src, "console=ttyS0", "52:54:00:11:22:33")

	// Pre-seed dst with a stale config.json + sentinel — the second
	// run must NOT recopy or repatch (sentinel short-circuits).
	stalePayload := map[string]any{
		"config": map[string]any{
			"payload": map[string]any{"cmdline": "do-not-touch"},
			"net": []any{
				map[string]any{"id": "_net0", "mac": "52:54:00:de:ad:be"},
			},
		},
	}
	staleData, _ := json.MarshalIndent(stalePayload, "", "  ")
	if err := os.WriteFile(filepath.Join(dst, "config.json"), staleData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, ".copy-complete"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run(src, dst, true, []string{"52:54:00:aa:bb:cc"}); err != nil {
		t.Fatalf("run: %v", err)
	}

	cmdline, mac := readPatchedCmdlineAndMAC(t, dst)
	if cmdline != "do-not-touch" {
		t.Errorf("cmdline mutated despite sentinel: %q", cmdline)
	}
	if mac != "52:54:00:de:ad:be" {
		t.Errorf("mac mutated despite sentinel: %q", mac)
	}
	// state.json should NOT have been copied (sentinel made it a no-op).
	if _, err := os.Stat(filepath.Join(dst, "state.json")); !os.IsNotExist(err) {
		t.Errorf("state.json copied despite sentinel; err=%v", err)
	}
}

func TestRun_PartialPriorRunIsWipedAndRetried(t *testing.T) {
	// Simulate: a prior init-container run started but crashed before
	// the sentinel write. dst has half-written state. Without the
	// wipe step, copies would land on top of the partial state.
	src := t.TempDir()
	dst := t.TempDir()
	writeMockSnapshot(t, src, "console=ttyS0", "52:54:00:11:22:33")

	if err := os.WriteFile(filepath.Join(dst, "config.json"), []byte("{garbled}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "stale-extra"), []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run(src, dst, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Stale extra file from the prior run is gone (wipe).
	if _, err := os.Stat(filepath.Join(dst, "stale-extra")); !os.IsNotExist(err) {
		t.Errorf("stale-extra should have been wiped; err=%v", err)
	}
	// config.json is the freshly copied one (parseable).
	data, err := os.ReadFile(filepath.Join(dst, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Errorf("config.json not parseable after retry: %v", err)
	}
}

func TestRun_NoPatchesRequestedSkipsConfigJSONRewrite(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeMockSnapshot(t, src, "console=ttyS0", "52:54:00:11:22:33")

	if err := run(src, dst, false, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Sentinel still written (run completed).
	if _, err := os.Stat(filepath.Join(dst, ".copy-complete")); err != nil {
		t.Errorf("sentinel missing: %v", err)
	}
	cmdline, mac := readPatchedCmdlineAndMAC(t, dst)
	if cmdline != "console=ttyS0" {
		t.Errorf("cmdline mutated unexpectedly: %q", cmdline)
	}
	if mac != "52:54:00:11:22:33" {
		t.Errorf("mac mutated unexpectedly: %q", mac)
	}
}

func TestRun_MissingSourceFails(t *testing.T) {
	dst := t.TempDir()
	err := run("/this/path/does/not/exist", dst, true, nil)
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestParseMACsCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"52:54:00:aa:bb:01", []string{"52:54:00:aa:bb:01"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{"a,,c", []string{"a", "", "c"}}, // empty position preserves source MAC
		{" a , b ", []string{"a", "b"}},
	}
	for _, c := range cases {
		got := parseMACsCSV(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseMACsCSV(%q) len=%d want %d (got=%v)", c.in, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseMACsCSV(%q)[%d] = %q want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestSentinelWrittenLast(t *testing.T) {
	// The sentinel must be written after the patcher; otherwise a
	// subsequent run sees the sentinel and skips the patch step. We
	// test by checking the sentinel mtime is no earlier than the
	// config.json mtime after a full run.
	src := t.TempDir()
	dst := t.TempDir()
	writeMockSnapshot(t, src, "console=ttyS0", "52:54:00:11:22:33")
	if err := run(src, dst, true, []string{"52:54:00:aa:bb:cc"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	cfgInfo, err := os.Stat(filepath.Join(dst, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	sentInfo, err := os.Stat(filepath.Join(dst, ".copy-complete"))
	if err != nil {
		t.Fatal(err)
	}
	if sentInfo.ModTime().Before(cfgInfo.ModTime()) {
		t.Errorf("sentinel mtime %v is before config.json mtime %v",
			sentInfo.ModTime(), cfgInfo.ModTime())
	}
}

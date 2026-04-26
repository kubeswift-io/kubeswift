package configjson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustMarshal(t *testing.T, cfg map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := map[string]any{
		"config": map[string]any{
			"payload": map[string]any{
				"cmdline": "console=ttyS0 root=/dev/vda1",
				"kernel":  "/path/to/CLOUDHV.fd",
			},
			"memory": map[string]any{"size": 2147483648.0},
		},
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigJSONFilename), mustMarshal(t, original), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	cmdline := got["config"].(map[string]any)["payload"].(map[string]any)["cmdline"]
	if cmdline != "console=ttyS0 root=/dev/vda1" {
		t.Errorf("cmdline = %v, want preserved", cmdline)
	}
}

func TestRead_FileMissing(t *testing.T) {
	if _, err := Read(t.TempDir()); err == nil {
		t.Errorf("expected error for missing config.json")
	}
}

func TestRead_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigJSONFilename), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Read(dir); err == nil {
		t.Errorf("expected parse error")
	}
}

func TestPatch_AppendCloneMarker_FreshCmdline(t *testing.T) {
	cfg := map[string]any{
		"config": map[string]any{
			"payload": map[string]any{
				"cmdline": "console=ttyS0 root=/dev/vda1",
			},
		},
	}
	changes, err := Patch(cfg, PatchOptions{AppendCmdlineMarker: true})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if len(changes) != 1 || !strings.Contains(changes[0], CloneCmdlineMarker) {
		t.Errorf("expected one change citing %q, got %v", CloneCmdlineMarker, changes)
	}
	cmdline := cfg["config"].(map[string]any)["payload"].(map[string]any)["cmdline"]
	want := "console=ttyS0 root=/dev/vda1 " + CloneCmdlineMarker
	if cmdline != want {
		t.Errorf("cmdline = %q, want %q", cmdline, want)
	}
}

func TestPatch_AppendCloneMarker_Idempotent(t *testing.T) {
	cfg := map[string]any{
		"config": map[string]any{
			"payload": map[string]any{
				"cmdline": "console=ttyS0 " + CloneCmdlineMarker + " root=/dev/vda1",
			},
		},
	}
	changes, err := Patch(cfg, PatchOptions{AppendCmdlineMarker: true})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected no changes (idempotent), got %v", changes)
	}
	cmdline := cfg["config"].(map[string]any)["payload"].(map[string]any)["cmdline"]
	want := "console=ttyS0 " + CloneCmdlineMarker + " root=/dev/vda1"
	if cmdline != want {
		t.Errorf("cmdline mutated; got %q want %q", cmdline, want)
	}
}

func TestPatch_AppendCloneMarker_NoCmdlineField(t *testing.T) {
	cfg := map[string]any{
		"config": map[string]any{
			"payload": map[string]any{
				"kernel": "/path/to/bzImage",
			},
		},
	}
	changes, err := Patch(cfg, PatchOptions{AppendCmdlineMarker: true})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if len(changes) != 1 {
		t.Errorf("expected one change, got %v", changes)
	}
	cmdline := cfg["config"].(map[string]any)["payload"].(map[string]any)["cmdline"]
	if cmdline != CloneCmdlineMarker {
		t.Errorf("cmdline = %v, want %q", cmdline, CloneCmdlineMarker)
	}
}

func TestPatch_AppendCloneMarker_SubstringFalsePositive(t *testing.T) {
	// "kubeswift.clone=truefoo" should NOT count as the marker being
	// present. We split on whitespace so this distinction is honored.
	cfg := map[string]any{
		"config": map[string]any{
			"payload": map[string]any{
				"cmdline": "console=ttyS0 kubeswift.clone=truefoo root=/dev/vda1",
			},
		},
	}
	changes, err := Patch(cfg, PatchOptions{AppendCmdlineMarker: true})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if len(changes) != 1 {
		t.Errorf("expected marker append (substring shouldn't match); got %v", changes)
	}
}

func TestPatch_NoOp_ZeroOptions(t *testing.T) {
	cfg := map[string]any{"config": map[string]any{"payload": map[string]any{}}}
	changes, err := Patch(cfg, PatchOptions{})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("zero PatchOptions should be no-op, got %v", changes)
	}
}

func TestPatch_TopLevelConfigMissing_ErrReturned(t *testing.T) {
	cfg := map[string]any{"unrelated": "value"}
	if _, err := Patch(cfg, PatchOptions{AppendCmdlineMarker: true}); err == nil {
		t.Errorf("expected error when top-level 'config' missing")
	}
}

func TestPatch_PreservesUnrelatedFields(t *testing.T) {
	// Sanity: patching the cmdline must not perturb other fields like
	// memory, devices, or top-level keys we don't understand. The
	// snapshot directory is opaque per Phase 0 Constraint #4 — only
	// the cmdline and (commit 13) MACs may change.
	cfg := map[string]any{
		"config": map[string]any{
			"payload": map[string]any{
				"cmdline": "console=ttyS0",
				"kernel":  "/path/to/CLOUDHV.fd",
			},
			"memory":  map[string]any{"size": 2147483648.0},
			"devices": []any{"vfio-pci-1", "virtio-net-2"},
		},
		"vmm": map[string]any{"version": "v51.1"},
	}
	if _, err := Patch(cfg, PatchOptions{AppendCmdlineMarker: true}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	// kernel preserved
	kernel := cfg["config"].(map[string]any)["payload"].(map[string]any)["kernel"]
	if kernel != "/path/to/CLOUDHV.fd" {
		t.Errorf("kernel = %v, want preserved", kernel)
	}
	// memory preserved
	memory := cfg["config"].(map[string]any)["memory"]
	if memory.(map[string]any)["size"].(float64) != 2147483648 {
		t.Errorf("memory size mutated: %v", memory)
	}
	// vmm version preserved
	if cfg["vmm"].(map[string]any)["version"] != "v51.1" {
		t.Errorf("vmm.version mutated")
	}
}

func TestWrite_RoundTripPreservesCmdline(t *testing.T) {
	dir := t.TempDir()
	cfg := map[string]any{
		"config": map[string]any{
			"payload": map[string]any{
				"cmdline": "console=ttyS0 " + CloneCmdlineMarker,
			},
		},
	}
	if err := Write(dir, cfg); err != nil {
		t.Fatalf("write: %v", err)
	}
	roundTripped, err := Read(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	cmdline := roundTripped["config"].(map[string]any)["payload"].(map[string]any)["cmdline"]
	if cmdline != "console=ttyS0 "+CloneCmdlineMarker {
		t.Errorf("round-trip mutated cmdline: %v", cmdline)
	}
}

func TestRewriteMACs_StubReturnsErrInCommit12(t *testing.T) {
	// Commit 13 implements the body. Commit 12 ships the stub that
	// returns a clear error if a caller passes a non-empty MAC map.
	// Locks the API shape so commit 13 doesn't break callers.
	cfg := map[string]any{
		"config": map[string]any{"payload": map[string]any{}},
	}
	_, err := Patch(cfg, PatchOptions{
		RewriteMACs: map[string]string{"eth0": "52:54:00:de:ad:be"},
	})
	if err == nil || !strings.Contains(err.Error(), "commit 13") {
		t.Errorf("expected commit-13 stub error, got: %v", err)
	}
}

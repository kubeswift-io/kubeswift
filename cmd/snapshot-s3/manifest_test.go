package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseManifest_RejectsBadSchema(t *testing.T) {
	if _, err := parseManifest([]byte(`{"schemaVersion":99}`)); err == nil {
		t.Error("must reject unknown schemaVersion")
	}
	if _, err := parseManifest([]byte(`not json`)); err == nil {
		t.Error("must reject non-JSON")
	}
}

func TestVerifyArtifact_Cases(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, size, err := fileSHA256(filepath.Join(dir, "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	// exact match
	if err := verifyArtifact(dir, Artifact{Path: "a.bin", Bytes: size, SHA256: sum}); err != nil {
		t.Errorf("matching artifact should verify; got %v", err)
	}
	// size mismatch
	if err := verifyArtifact(dir, Artifact{Path: "a.bin", Bytes: size + 1, SHA256: sum}); err == nil {
		t.Error("size mismatch must fail")
	}
	// sha mismatch
	if err := verifyArtifact(dir, Artifact{Path: "a.bin", Bytes: size, SHA256: "deadbeef"}); err == nil {
		t.Error("sha256 mismatch must fail")
	}
	// missing file
	if err := verifyArtifact(dir, Artifact{Path: "missing.bin", Bytes: 1, SHA256: "x"}); err == nil {
		t.Error("missing file must fail")
	}
}

func TestBuildManifest_ExcludesManifestObject_Deterministic(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.bin"), []byte("b"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "a.bin"), []byte("a"), 0o644)
	m, err := buildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Artifacts) != 2 {
		t.Fatalf("manifest.json must be excluded; got %d artifacts", len(m.Artifacts))
	}
	if m.Artifacts[0].Path != "a.bin" || m.Artifacts[1].Path != "b.bin" {
		t.Errorf("artifacts must be sorted; got %s, %s", m.Artifacts[0].Path, m.Artifacts[1].Path)
	}
}

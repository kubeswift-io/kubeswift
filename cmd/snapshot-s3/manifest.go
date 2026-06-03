package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// manifestObjectName is the manifest's object key (relative to the snapshot
// prefix). It is uploaded LAST so its presence in S3 means "the export is
// complete", and it is the first object the download path fetches.
const manifestObjectName = "manifest.json"

const manifestSchemaVersion = 1

// Artifact is one file in the snapshot, identified by its path RELATIVE to the
// snapshot directory (forward-slash separated, S3-key-safe).
type Artifact struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Manifest is the source of truth for an s3-exported snapshot: the full set of
// artifacts with per-file size + sha256 so the restore path can detect a
// truncated or corrupt object and fail loudly rather than boot a broken guest
// (Design Principle #6).
type Manifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	SwiftSnapshot string     `json:"swiftSnapshot,omitempty"`
	IncludeMemory bool       `json:"includeMemory"`
	Artifacts     []Artifact `json:"artifacts"`
	TotalBytes    int64      `json:"totalBytes"`
}

// buildManifest walks dir recursively and records every regular file's relative
// path, size, and sha256. The manifest object itself is excluded (it is not an
// artifact of the snapshot). The artifact list is sorted for determinism.
func buildManifest(dir string) (*Manifest, error) {
	m := &Manifest{SchemaVersion: manifestSchemaVersion}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == manifestObjectName {
			return nil
		}
		sum, size, err := fileSHA256(p)
		if err != nil {
			return err
		}
		m.Artifacts = append(m.Artifacts, Artifact{Path: rel, Bytes: size, SHA256: sum})
		m.TotalBytes += size
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].Path < m.Artifacts[j].Path })
	return m, nil
}

// fileSHA256 streams the file through sha256, returning the hex digest and size.
func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// verifyArtifact checks a file under dir against its manifest entry (size +
// sha256). Returns a descriptive error on any mismatch or read failure.
func verifyArtifact(dir string, a Artifact) error {
	path := filepath.Join(dir, filepath.FromSlash(a.Path))
	sum, size, err := fileSHA256(path)
	if err != nil {
		return fmt.Errorf("verify %s: %w", a.Path, err)
	}
	if size != a.Bytes {
		return fmt.Errorf("verify %s: size %d != manifest %d", a.Path, size, a.Bytes)
	}
	if sum != a.SHA256 {
		return fmt.Errorf("verify %s: sha256 mismatch (got %s, want %s)", a.Path, sum, a.SHA256)
	}
	return nil
}

func (m *Manifest) marshal() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

func parseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.SchemaVersion != manifestSchemaVersion {
		return nil, fmt.Errorf("unsupported manifest schemaVersion %d (want %d)", m.SchemaVersion, manifestSchemaVersion)
	}
	return &m, nil
}

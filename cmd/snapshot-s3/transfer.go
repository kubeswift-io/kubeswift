package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// transferStats reports how many artifact bytes a transfer moved vs skipped
// (already present + verified). transferred + skipped == total. The controller
// reads this back via the container termination message (no extra RBAC) to
// stamp status bytes + the Phase 5 byte counters. The manifest object itself
// (tiny) is not counted, so the invariant holds.
type transferStats struct {
	TransferredBytes int64 `json:"transferredBytes"`
	SkippedBytes     int64 `json:"skippedBytes"`
	TotalBytes       int64 `json:"totalBytes"`
}

// runUpload builds the manifest from srcDir, uploads every artifact under
// keyPrefix, and uploads manifest.json LAST (its presence marks the export
// complete). Idempotent: an artifact already present with the matching size is
// skipped, so a re-scheduled Job resumes rather than restarts.
func runUpload(ctx context.Context, store objectStore, srcDir, keyPrefix, snapName string, includeMemory bool) (transferStats, error) {
	m, err := buildManifest(srcDir)
	if err != nil {
		return transferStats{}, fmt.Errorf("build manifest from %q: %w", srcDir, err)
	}
	m.SwiftSnapshot = snapName
	m.IncludeMemory = includeMemory
	log.Printf("snapshot-s3 upload: %d artifact(s), %d bytes total", len(m.Artifacts), m.TotalBytes)

	stats := transferStats{TotalBytes: m.TotalBytes}
	for _, a := range m.Artifacts {
		key := path.Join(keyPrefix, a.Path)
		// Resume only when the existing object matches BOTH size and content
		// hash. Size alone is unsafe: a memory-ranges file is always exactly the
		// guest's RAM size, so a stale object left at this key by a prior
		// same-named snapshot would be silently kept while the manifest records
		// the new hash — a permanent mismatch the restore then fails on.
		if size, sha, ok, serr := store.stat(ctx, key); serr == nil && ok && size == a.Bytes && sha == a.SHA256 {
			log.Printf("  skip %s (already uploaded, %d bytes, sha matches)", a.Path, size)
			stats.SkippedBytes += a.Bytes
			continue
		}
		f, err := os.Open(filepath.Join(srcDir, filepath.FromSlash(a.Path)))
		if err != nil {
			return transferStats{}, fmt.Errorf("open artifact %q: %w", a.Path, err)
		}
		log.Printf("  put %s (%d bytes)", a.Path, a.Bytes)
		err = store.put(ctx, key, f, a.Bytes, a.SHA256)
		f.Close()
		if err != nil {
			return transferStats{}, fmt.Errorf("upload artifact %q: %w", a.Path, err)
		}
		stats.TransferredBytes += a.Bytes
	}

	data, err := m.marshal()
	if err != nil {
		return transferStats{}, err
	}
	if err := store.put(ctx, path.Join(keyPrefix, manifestObjectName), bytes.NewReader(data), int64(len(data)), sha256Hex(data)); err != nil {
		return transferStats{}, fmt.Errorf("upload manifest: %w", err)
	}
	log.Printf("snapshot-s3 upload complete: %s/%s (%d transferred, %d skipped)", keyPrefix, manifestObjectName, stats.TransferredBytes, stats.SkippedBytes)
	return stats, nil
}

// runDownload fetches the manifest, then downloads every artifact under
// keyPrefix into dstDir, verifying size + sha256. A truncated/corrupt object
// fails loudly. Idempotent: an artifact already present and verifying is
// skipped.
func runDownload(ctx context.Context, store objectStore, dstDir, keyPrefix string) (transferStats, error) {
	mrc, err := store.get(ctx, path.Join(keyPrefix, manifestObjectName))
	if err != nil {
		return transferStats{}, fmt.Errorf("get manifest (export incomplete or wrong prefix?): %w", err)
	}
	mdata, err := io.ReadAll(mrc)
	mrc.Close()
	if err != nil {
		return transferStats{}, fmt.Errorf("read manifest: %w", err)
	}
	m, err := parseManifest(mdata)
	if err != nil {
		return transferStats{}, err
	}
	log.Printf("snapshot-s3 download: %d artifact(s), %d bytes total", len(m.Artifacts), m.TotalBytes)

	stats := transferStats{TotalBytes: m.TotalBytes}
	for _, a := range m.Artifacts {
		dst := filepath.Join(dstDir, filepath.FromSlash(a.Path))
		if verifyArtifact(dstDir, a) == nil {
			log.Printf("  skip %s (already present, verified)", a.Path)
			stats.SkippedBytes += a.Bytes
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return transferStats{}, fmt.Errorf("mkdir for %q: %w", a.Path, err)
		}
		if err := downloadOne(ctx, store, path.Join(keyPrefix, a.Path), dst); err != nil {
			return transferStats{}, err
		}
		if err := verifyArtifact(dstDir, a); err != nil {
			return transferStats{}, fmt.Errorf("downloaded artifact failed verification: %w", err)
		}
		log.Printf("  got %s (%d bytes, verified)", a.Path, a.Bytes)
		stats.TransferredBytes += a.Bytes
	}
	log.Printf("snapshot-s3 download complete into %s (%d transferred, %d skipped)", dstDir, stats.TransferredBytes, stats.SkippedBytes)
	return stats, nil
}

// runDelete removes every object under keyPrefix (prefix-scoped list-and-remove,
// per the Phase 5 design OQ1). The prefix is per-<namespace>/<snapshot>, so the
// blast radius is a single snapshot — and a prefix-list also reaps orphans a
// manifest-driven delete would miss (a partial upload that never reached
// manifest.json, stale same-size objects). Refuses an empty prefix (which would
// target the whole bucket).
func runDelete(ctx context.Context, store objectStore, keyPrefix string) error {
	if strings.Trim(keyPrefix, "/ ") == "" {
		return fmt.Errorf("refusing to delete: empty key prefix would target the whole bucket")
	}
	keys, err := store.list(ctx, keyPrefix)
	if err != nil {
		return fmt.Errorf("list %q: %w", keyPrefix, err)
	}
	log.Printf("snapshot-s3 delete: %d object(s) under %s", len(keys), keyPrefix)
	for _, k := range keys {
		if err := store.remove(ctx, k); err != nil {
			return fmt.Errorf("remove %q: %w", k, err)
		}
		log.Printf("  rm %s", k)
	}
	log.Printf("snapshot-s3 delete complete: %s", keyPrefix)
	return nil
}

func downloadOne(ctx context.Context, store objectStore, key, dst string) error {
	rc, err := store.get(ctx, key)
	if err != nil {
		return fmt.Errorf("get %q: %w", key, err)
	}
	defer rc.Close()
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %q: %w", dst, err)
	}
	_, err = io.Copy(f, rc)
	cerr := f.Close()
	if err != nil {
		return fmt.Errorf("write %q: %w", dst, err)
	}
	return cerr
}

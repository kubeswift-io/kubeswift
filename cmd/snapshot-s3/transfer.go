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

	"github.com/klauspost/compress/zstd"
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
	for i := range m.Artifacts {
		// Mark every artifact zstd-compressed in the manifest we upload, so the
		// download path knows to fetch the codec-suffixed object and decompress.
		// (The memory image is mostly zeros — sparse holes read back as zeros —
		// which zstd collapses; small JSON artifacts compress harmlessly.)
		m.Artifacts[i].Compression = compressionZstd
		a := m.Artifacts[i]
		key := path.Join(keyPrefix, objectKeyFor(a))
		// Resume on the ORIGINAL content hash (recorded as object metadata at
		// upload time). sha256 is collision-safe, so hash-match alone is a safe
		// skip — and unlike PR #118's size+sha check, it composes with
		// compression: the stored object's size is the COMPRESSED size, not the
		// artifact's original Bytes, so a size comparison can no longer be used.
		if _, sha, ok, serr := store.stat(ctx, key); serr == nil && ok && sha == a.SHA256 {
			log.Printf("  skip %s (already uploaded, sha matches)", a.Path)
			stats.SkippedBytes += a.Bytes
			continue
		}
		f, err := os.Open(filepath.Join(srcDir, filepath.FromSlash(a.Path)))
		if err != nil {
			return transferStats{}, fmt.Errorf("open artifact %q: %w", a.Path, err)
		}
		log.Printf("  put %s (%d bytes -> zstd)", a.Path, a.Bytes)
		// size=-1: the compressed length is unknown up front; minio-go streams
		// and multiparts as needed. The sha256 metadata is the ORIGINAL hash
		// (for resume + download verification), NOT the compressed object's.
		cr := compressingReader(f)
		err = store.put(ctx, key, cr, -1, a.SHA256)
		cr.Close() // unblock the compress goroutine if put errored mid-stream
		f.Close()
		if err != nil {
			return transferStats{}, fmt.Errorf("upload artifact %q: %w", a.Path, err)
		}
		// TransferredBytes/SkippedBytes track LOGICAL artifact bytes (preserving
		// the transferred+skipped==total invariant the controller relies on);
		// the compressed wire/storage saving is not surfaced in these counters.
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
		if err := downloadOne(ctx, store, path.Join(keyPrefix, objectKeyFor(a)), dst, a.Compression); err != nil {
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

// downloadOne fetches key into dst, decompressing on the fly when the artifact
// was stored with a codec. dst always receives the ORIGINAL (decompressed)
// bytes, which the caller then verifies against the manifest's size + sha256.
func downloadOne(ctx context.Context, store objectStore, key, dst, compression string) error {
	rc, err := store.get(ctx, key)
	if err != nil {
		return fmt.Errorf("get %q: %w", key, err)
	}
	defer rc.Close()
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %q: %w", dst, err)
	}
	var src io.Reader = rc
	var dec *zstd.Decoder
	if compression == compressionZstd {
		dec, err = zstd.NewReader(rc)
		if err != nil {
			f.Close()
			return fmt.Errorf("zstd reader for %q: %w", key, err)
		}
		src = dec
	}
	_, err = io.Copy(f, src)
	if dec != nil {
		dec.Close()
	}
	cerr := f.Close()
	if err != nil {
		return fmt.Errorf("write %q: %w", dst, err)
	}
	return cerr
}

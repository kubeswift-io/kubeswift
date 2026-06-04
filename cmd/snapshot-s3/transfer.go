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
)

// runUpload builds the manifest from srcDir, uploads every artifact under
// keyPrefix, and uploads manifest.json LAST (its presence marks the export
// complete). Idempotent: an artifact already present with the matching size is
// skipped, so a re-scheduled Job resumes rather than restarts.
func runUpload(ctx context.Context, store objectStore, srcDir, keyPrefix, snapName string, includeMemory bool) error {
	m, err := buildManifest(srcDir)
	if err != nil {
		return fmt.Errorf("build manifest from %q: %w", srcDir, err)
	}
	m.SwiftSnapshot = snapName
	m.IncludeMemory = includeMemory
	log.Printf("snapshot-s3 upload: %d artifact(s), %d bytes total", len(m.Artifacts), m.TotalBytes)

	for _, a := range m.Artifacts {
		key := path.Join(keyPrefix, a.Path)
		// Resume only when the existing object matches BOTH size and content
		// hash. Size alone is unsafe: a memory-ranges file is always exactly the
		// guest's RAM size, so a stale object left at this key by a prior
		// same-named snapshot would be silently kept while the manifest records
		// the new hash — a permanent mismatch the restore then fails on.
		if size, sha, ok, serr := store.stat(ctx, key); serr == nil && ok && size == a.Bytes && sha == a.SHA256 {
			log.Printf("  skip %s (already uploaded, %d bytes, sha matches)", a.Path, size)
			continue
		}
		f, err := os.Open(filepath.Join(srcDir, filepath.FromSlash(a.Path)))
		if err != nil {
			return fmt.Errorf("open artifact %q: %w", a.Path, err)
		}
		log.Printf("  put %s (%d bytes)", a.Path, a.Bytes)
		err = store.put(ctx, key, f, a.Bytes, a.SHA256)
		f.Close()
		if err != nil {
			return fmt.Errorf("upload artifact %q: %w", a.Path, err)
		}
	}

	data, err := m.marshal()
	if err != nil {
		return err
	}
	if err := store.put(ctx, path.Join(keyPrefix, manifestObjectName), bytes.NewReader(data), int64(len(data)), sha256Hex(data)); err != nil {
		return fmt.Errorf("upload manifest: %w", err)
	}
	log.Printf("snapshot-s3 upload complete: %s/%s", keyPrefix, manifestObjectName)
	return nil
}

// runDownload fetches the manifest, then downloads every artifact under
// keyPrefix into dstDir, verifying size + sha256. A truncated/corrupt object
// fails loudly. Idempotent: an artifact already present and verifying is
// skipped.
func runDownload(ctx context.Context, store objectStore, dstDir, keyPrefix string) error {
	mrc, err := store.get(ctx, path.Join(keyPrefix, manifestObjectName))
	if err != nil {
		return fmt.Errorf("get manifest (export incomplete or wrong prefix?): %w", err)
	}
	mdata, err := io.ReadAll(mrc)
	mrc.Close()
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	m, err := parseManifest(mdata)
	if err != nil {
		return err
	}
	log.Printf("snapshot-s3 download: %d artifact(s), %d bytes total", len(m.Artifacts), m.TotalBytes)

	for _, a := range m.Artifacts {
		dst := filepath.Join(dstDir, filepath.FromSlash(a.Path))
		if verifyArtifact(dstDir, a) == nil {
			log.Printf("  skip %s (already present, verified)", a.Path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir for %q: %w", a.Path, err)
		}
		if err := downloadOne(ctx, store, path.Join(keyPrefix, a.Path), dst); err != nil {
			return err
		}
		if err := verifyArtifact(dstDir, a); err != nil {
			return fmt.Errorf("downloaded artifact failed verification: %w", err)
		}
		log.Printf("  got %s (%d bytes, verified)", a.Path, a.Bytes)
	}
	log.Printf("snapshot-s3 download complete into %s", dstDir)
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

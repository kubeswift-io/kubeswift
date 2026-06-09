package main

import (
	"io"

	"github.com/klauspost/compress/zstd"
)

// Tier C snapshot artifacts are stream-compressed with zstd on upload and
// decompressed on download (design snapshot-ch-v52-efficiency.md §3). CH v52's
// sparse memory snapshot shrinks LOCAL disk (holes), but a normal read of the
// memory file returns zeros for those holes — so the S3 object would otherwise
// carry the full guest-RAM size of mostly-zeros. zstd collapses the zero runs
// to almost nothing: the upload win is compression, not on-disk sparseness.
//
// The manifest keeps each artifact's ORIGINAL bytes + sha256 (what the restore
// consumes and verifies); the object stores the COMPRESSED bytes at a
// codec-suffixed key, and the Artifact.Compression field is the authoritative
// signal for the download path. Empty Compression (older manifests) means the
// object is stored uncompressed at the bare path — backward compatible.
const (
	compressionZstd = "zstd"
	zstdSuffix      = ".zst"
)

// objectKeyFor returns the S3 object key for an artifact relative to the
// snapshot prefix: the artifact path, plus a codec suffix when compressed.
// The suffix keeps the bucket self-describing while the manifest's Compression
// field stays authoritative.
func objectKeyFor(a Artifact) string {
	if a.Compression == compressionZstd {
		return a.Path + zstdSuffix
	}
	return a.Path
}

// compressingReader returns an io.ReadCloser that yields the zstd-compressed
// bytes of r. Compression runs in a goroutine writing into an io.Pipe so the
// uploader streams compressed bytes without buffering the whole (multi-GiB)
// artifact in memory. A compression/read error is surfaced to the consumer via
// the pipe so the upload fails loudly rather than storing a truncated object.
func compressingReader(r io.Reader) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		enc, err := zstd.NewWriter(pw)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		_, copyErr := io.Copy(enc, r)
		// enc.Close() flushes the zstd footer; the object is only valid if
		// both the copy and the flush succeed.
		closeErr := enc.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		pw.CloseWithError(copyErr)
	}()
	return pr
}

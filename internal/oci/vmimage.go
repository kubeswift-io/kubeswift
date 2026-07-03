package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

// Golden-image (P3) artifact types. A golden VM disk is stored CHUNKED so
// identical chunks dedup by content address: zero windows are never stored, and
// unchanged blocks across versions (v1 -> v1.1) are shared.
const (
	VMImageArtifactType = "application/vnd.kubeswift.vmimage.v1"
	VMImageConfigType   = "application/vnd.kubeswift.vmimage.config.v1+json"
	VMDiskChunkType     = "application/vnd.kubeswift.vmdisk.chunk.v1"
	// ChunkOffsetAnnotation records a chunk layer's byte offset in the disk;
	// the offset is authoritative (not layer order), so an identical chunk at the
	// same offset across versions yields a byte-identical descriptor -> dedup.
	ChunkOffsetAnnotation = "kubeswift.io/chunk-offset"
)

// Config is the golden-image artifact's config blob: enough to reassemble the
// sparse disk (TotalSize is the truncate target; ChunkSize is informational).
type Config struct {
	TotalSize int64  `json:"totalSize"`
	ChunkSize int64  `json:"chunkSize"`
	Format    string `json:"format"`
	OSType    string `json:"osType,omitempty"`
}

// PushResult reports the byte accounting of a ChunkAndPush. TransferredBytes are
// the bytes actually pushed; SkippedBytes are chunks the registry already had
// (deduped); TotalBytes is the logical disk size. TransferredBytes/TotalBytes is
// the dedup figure.
type PushResult struct {
	ManifestDigest   string
	TransferredBytes int64
	SkippedBytes     int64
	TotalBytes       int64
}

// ChunkAndPush streams filePath in chunkSize windows, SKIPS all-zero windows
// (never stored — a raw disk is sparse), and pushes each non-zero chunk as one
// digest-addressed layer. A chunk already present in dst (a re-push of an
// unchanged v1.1 block, or a repeated non-zero pattern) is counted as skipped
// (deduped), not re-uploaded — so TransferredBytes/TotalBytes is the dedup
// figure. Streams window-by-window; the whole disk is never held in memory.
func ChunkAndPush(ctx context.Context, filePath string, dst oras.Target, tag string, chunkSize int64, format, osType string) (ocispec.Descriptor, PushResult, error) {
	var zero ocispec.Descriptor
	f, err := os.Open(filePath)
	if err != nil {
		return zero, PushResult{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return zero, PushResult{}, err
	}
	totalSize := fi.Size()
	if fi.Mode()&os.ModeDevice != 0 {
		// A block device (a raw Block-mode PVC — the W9 root path or a v1.1
		// data disk) reports Size()==0 from Stat(); its real size comes from
		// seeking to the end. The read loop below already reads the whole device
		// to EOF, so the chunks are correct either way — this only fixes the
		// recorded TotalSize so the download side can size a Filesystem target.
		if end, serr := f.Seek(0, io.SeekEnd); serr == nil {
			totalSize = end
			if _, serr := f.Seek(0, io.SeekStart); serr != nil {
				return zero, PushResult{}, fmt.Errorf("seek to start after sizing %s: %w", filePath, serr)
			}
		}
	}

	buf := make([]byte, chunkSize)
	zeros := make([]byte, chunkSize)
	var layers []ocispec.Descriptor
	var transferred, skipped int64
	offset := int64(0)
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			chunk := buf[:n]
			if !bytes.Equal(chunk, zeros[:n]) {
				desc := ocispec.Descriptor{
					MediaType:   VMDiskChunkType,
					Digest:      godigest.FromBytes(chunk),
					Size:        int64(n),
					Annotations: map[string]string{ChunkOffsetAnnotation: strconv.FormatInt(offset, 10)},
				}
				exists, eerr := dst.Exists(ctx, desc)
				if eerr != nil {
					return zero, PushResult{}, fmt.Errorf("exists check at offset %d: %w", offset, eerr)
				}
				if exists {
					skipped += int64(n)
				} else {
					// Push reads the reader fully before returning, so buf is safe to reuse.
					if perr := dst.Push(ctx, desc, bytes.NewReader(chunk)); perr != nil {
						return zero, PushResult{}, fmt.Errorf("push chunk at offset %d: %w", offset, perr)
					}
					transferred += int64(n)
				}
				layers = append(layers, desc)
			}
			offset += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return zero, PushResult{}, fmt.Errorf("read %s: %w", filePath, rerr)
		}
	}

	// Config blob (must be pushed before the manifest that references it).
	cfgBytes, err := json.Marshal(Config{TotalSize: totalSize, ChunkSize: chunkSize, Format: format, OSType: osType})
	if err != nil {
		return zero, PushResult{}, err
	}
	cfgDesc := ocispec.Descriptor{MediaType: VMImageConfigType, Digest: godigest.FromBytes(cfgBytes), Size: int64(len(cfgBytes))}
	if exists, _ := dst.Exists(ctx, cfgDesc); !exists {
		if err := dst.Push(ctx, cfgDesc, bytes.NewReader(cfgBytes)); err != nil {
			return zero, PushResult{}, fmt.Errorf("push config: %w", err)
		}
	}

	manifestDesc, err := oras.PackManifest(ctx, dst, oras.PackManifestVersion1_1, VMImageArtifactType,
		oras.PackManifestOptions{Layers: layers, ConfigDescriptor: &cfgDesc})
	if err != nil {
		return zero, PushResult{}, fmt.Errorf("pack manifest: %w", err)
	}
	if err := dst.Tag(ctx, manifestDesc, tag); err != nil {
		return zero, PushResult{}, fmt.Errorf("tag: %w", err)
	}
	return manifestDesc, PushResult{
		ManifestDigest:   manifestDesc.Digest.String(),
		TransferredBytes: transferred,
		SkippedBytes:     skipped,
		TotalBytes:       totalSize,
	}, nil
}

// PullAndReassemble resolves ref (tag or digest), reads the config, truncates
// filePath to totalSize (a sparse file — omitted zero windows cost nothing), then
// fetches each chunk layer (digest-verified, so a corrupt chunk fails loudly)
// and writes it at its recorded offset.
func PullAndReassemble(ctx context.Context, src oras.ReadOnlyTarget, ref, filePath string) (ocispec.Descriptor, error) {
	var zero ocispec.Descriptor
	manifestDesc, err := src.Resolve(ctx, ref)
	if err != nil {
		return zero, fmt.Errorf("resolve %s: %w", ref, err)
	}
	manifestBytes, err := content.FetchAll(ctx, src, manifestDesc)
	if err != nil {
		return zero, fmt.Errorf("fetch manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return zero, fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.ArtifactType != VMImageArtifactType && manifest.Config.MediaType != VMImageConfigType {
		return zero, fmt.Errorf("not a kubeswift golden image (artifactType %q)", manifest.ArtifactType)
	}
	cfgBytes, err := content.FetchAll(ctx, src, manifest.Config)
	if err != nil {
		return zero, fmt.Errorf("fetch config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return zero, fmt.Errorf("parse config: %w", err)
	}

	// Zero windows are never stored (ChunkAndPush skips them), so the destination
	// must read as zero wherever no chunk lands. Two destination kinds:
	//   - regular file (Filesystem image.raw): os.Create's O_TRUNC zeroes it, then
	//     Truncate sizes the sparse file so the tail beyond the last chunk is zero.
	//   - block device (a raw Block-mode PVC, W9 / v1.1 data disk): Truncate()
	//     returns EINVAL and O_TRUNC is a no-op, so we CANNOT re-zero it here — we
	//     rely on the destination PVC being freshly provisioned (Longhorn zeroes
	//     new volumes), so a skipped zero window correctly reads back as zero.
	// (A device is detected by Stat; the node exists whether or not it's a device.)
	isBlockDev := false
	if info, serr := os.Stat(filePath); serr == nil && info.Mode()&os.ModeDevice != 0 {
		isBlockDev = true
	}
	var f *os.File
	if isBlockDev {
		f, err = os.OpenFile(filePath, os.O_RDWR, 0o644)
	} else {
		f, err = os.Create(filePath) // O_RDWR|O_CREATE|O_TRUNC — zeroes a stale file
	}
	if err != nil {
		return zero, err
	}
	defer f.Close()
	if !isBlockDev {
		if err := f.Truncate(cfg.TotalSize); err != nil {
			return zero, fmt.Errorf("truncate to %d: %w", cfg.TotalSize, err)
		}
	}
	for _, layer := range manifest.Layers {
		// Skip non-chunk layers — oras.PackManifest injects an OCI "empty"
		// placeholder layer when there are no real layers (an all-zero disk).
		if layer.MediaType != VMDiskChunkType {
			continue
		}
		off, err := strconv.ParseInt(layer.Annotations[ChunkOffsetAnnotation], 10, 64)
		if err != nil {
			return zero, fmt.Errorf("chunk %s missing/bad %s: %w", layer.Digest, ChunkOffsetAnnotation, err)
		}
		if err := fetchChunkAt(ctx, src, layer, f, off); err != nil {
			return zero, fmt.Errorf("chunk at offset %d: %w", off, err)
		}
	}
	return manifestDesc, nil
}

// fetchChunkAt streams one chunk layer into f at off, verifying digest + size.
// It streams (VerifyReader), NOT content.FetchAll, because FetchAll refuses any
// blob larger than oras's 32 MiB in-memory cap (maxDescriptorSize) — chunks are
// commonly 64 MiB — and streaming also avoids buffering the whole chunk.
func fetchChunkAt(ctx context.Context, src oras.ReadOnlyTarget, layer ocispec.Descriptor, f *os.File, off int64) error {
	rc, err := src.Fetch(ctx, layer)
	if err != nil {
		return err
	}
	defer rc.Close()
	vr := content.NewVerifyReader(rc, layer)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(f, vr); err != nil {
		return err
	}
	return vr.Verify() // digest + size mismatch fails loudly
}

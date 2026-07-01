package main

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
// unchanged blocks across versions (v1 -> v1.1) are shared. See
// docs/design/oras-golden-image.md.
const (
	vmImageArtifactType = "application/vnd.kubeswift.vmimage.v1"
	vmImageConfigType   = "application/vnd.kubeswift.vmimage.config.v1+json"
	vmDiskChunkType     = "application/vnd.kubeswift.vmdisk.chunk.v1"
	// chunkOffsetAnnotation records a chunk layer's byte offset in the disk;
	// the offset is authoritative (not layer order), so an identical chunk at the
	// same offset across versions yields a byte-identical descriptor -> dedup.
	chunkOffsetAnnotation = "kubeswift.io/chunk-offset"
)

// vmImageConfig is the artifact's config blob: enough to reassemble the sparse
// disk (totalSize is the truncate target; chunkSize is informational).
type vmImageConfig struct {
	TotalSize int64  `json:"totalSize"`
	ChunkSize int64  `json:"chunkSize"`
	Format    string `json:"format"`
	OSType    string `json:"osType,omitempty"`
}

// chunkAndPush streams filePath in chunkSize windows, SKIPS all-zero windows
// (never stored — a raw disk is sparse), and pushes each non-zero chunk as one
// digest-addressed layer. A chunk already present in dst (a re-push of an
// unchanged v1.1 block, or a repeated non-zero pattern) is counted as skipped
// (deduped), not re-uploaded — so transferredBytes/totalBytes is the dedup
// figure. Streams window-by-window; the whole disk is never held in memory.
func chunkAndPush(ctx context.Context, filePath string, dst oras.Target, tag string, chunkSize int64, format, osType string) (ocispec.Descriptor, transferStats, error) {
	var zero ocispec.Descriptor
	f, err := os.Open(filePath)
	if err != nil {
		return zero, transferStats{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return zero, transferStats{}, err
	}
	totalSize := fi.Size()

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
					MediaType:   vmDiskChunkType,
					Digest:      godigest.FromBytes(chunk),
					Size:        int64(n),
					Annotations: map[string]string{chunkOffsetAnnotation: strconv.FormatInt(offset, 10)},
				}
				exists, eerr := dst.Exists(ctx, desc)
				if eerr != nil {
					return zero, transferStats{}, fmt.Errorf("exists check at offset %d: %w", offset, eerr)
				}
				if exists {
					skipped += int64(n)
				} else {
					// Push reads the reader fully before returning, so buf is safe to reuse.
					if perr := dst.Push(ctx, desc, bytes.NewReader(chunk)); perr != nil {
						return zero, transferStats{}, fmt.Errorf("push chunk at offset %d: %w", offset, perr)
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
			return zero, transferStats{}, fmt.Errorf("read %s: %w", filePath, rerr)
		}
	}

	// Config blob (must be pushed before the manifest that references it).
	cfgBytes, err := json.Marshal(vmImageConfig{TotalSize: totalSize, ChunkSize: chunkSize, Format: format, OSType: osType})
	if err != nil {
		return zero, transferStats{}, err
	}
	cfgDesc := ocispec.Descriptor{MediaType: vmImageConfigType, Digest: godigest.FromBytes(cfgBytes), Size: int64(len(cfgBytes))}
	if exists, _ := dst.Exists(ctx, cfgDesc); !exists {
		if err := dst.Push(ctx, cfgDesc, bytes.NewReader(cfgBytes)); err != nil {
			return zero, transferStats{}, fmt.Errorf("push config: %w", err)
		}
	}

	manifestDesc, err := oras.PackManifest(ctx, dst, oras.PackManifestVersion1_1, vmImageArtifactType,
		oras.PackManifestOptions{Layers: layers, ConfigDescriptor: &cfgDesc})
	if err != nil {
		return zero, transferStats{}, fmt.Errorf("pack manifest: %w", err)
	}
	if err := dst.Tag(ctx, manifestDesc, tag); err != nil {
		return zero, transferStats{}, fmt.Errorf("tag: %w", err)
	}
	return manifestDesc, transferStats{
		TransferredBytes: transferred,
		SkippedBytes:     skipped,
		TotalBytes:       totalSize,
		ManifestDigest:   manifestDesc.Digest.String(),
	}, nil
}

// pullAndReassemble resolves ref (tag or digest), reads the config, truncates
// filePath to totalSize (a sparse file — omitted zero windows cost nothing), then
// fetches each chunk layer (content.FetchAll verifies the digest, so a corrupt
// chunk fails loudly) and writes it at its recorded offset.
func pullAndReassemble(ctx context.Context, src oras.ReadOnlyTarget, ref, filePath string) (ocispec.Descriptor, error) {
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
	if manifest.ArtifactType != vmImageArtifactType && manifest.Config.MediaType != vmImageConfigType {
		return zero, fmt.Errorf("not a kubeswift golden image (artifactType %q)", manifest.ArtifactType)
	}
	cfgBytes, err := content.FetchAll(ctx, src, manifest.Config)
	if err != nil {
		return zero, fmt.Errorf("fetch config: %w", err)
	}
	var cfg vmImageConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return zero, fmt.Errorf("parse config: %w", err)
	}

	f, err := os.Create(filePath)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	if err := f.Truncate(cfg.TotalSize); err != nil {
		return zero, fmt.Errorf("truncate to %d: %w", cfg.TotalSize, err)
	}
	for _, layer := range manifest.Layers {
		// Skip non-chunk layers — oras.PackManifest injects an OCI "empty"
		// placeholder layer when there are no real layers (an all-zero disk).
		if layer.MediaType != vmDiskChunkType {
			continue
		}
		off, err := strconv.ParseInt(layer.Annotations[chunkOffsetAnnotation], 10, 64)
		if err != nil {
			return zero, fmt.Errorf("chunk %s missing/bad %s: %w", layer.Digest, chunkOffsetAnnotation, err)
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

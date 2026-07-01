package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// artifactType tags the snapshot as a KubeSwift VM snapshot so a registry
// referrers query can distinguish it from an image.
const artifactType = "application/vnd.kubeswift.vmsnapshot.v1"

// transferStats is written to the container termination message. The controller
// reads transferredBytes/skippedBytes/totalBytes via clonecommon.TransferReport
// (the shared s3/oci report shape) and the oci-only reference/manifestDigest to
// stamp status.oci — a superset, so the 3-field reader ignores the extras.
type transferStats struct {
	TransferredBytes int64  `json:"transferredBytes"`
	SkippedBytes     int64  `json:"skippedBytes"`
	TotalBytes       int64  `json:"totalBytes"`
	Reference        string `json:"reference,omitempty"`
	ManifestDigest   string `json:"manifestDigest,omitempty"`
}

// mediaTypeFor gives each snapshot artifact a descriptive OCI layer media type.
// The bytes are opaque to the registry; the type is for human/tooling clarity.
func mediaTypeFor(name string) string {
	switch {
	case name == "config.json":
		return "application/vnd.kubeswift.vmsnapshot.config.v1+json"
	case name == "state.json":
		return "application/vnd.kubeswift.vmsnapshot.state.v1+json"
	case strings.HasPrefix(name, "memory"):
		return "application/vnd.kubeswift.vmsnapshot.memory.v1"
	default:
		return "application/vnd.kubeswift.vmsnapshot.file.v1"
	}
}

// packAndPush packages every regular file in dir as one OCI layer (title-
// annotated so a pull restores it by name), builds an artifact manifest, and
// copies it to dst under tag. The byte hooks separate bytes actually
// transferred from bytes the registry already had (skipped = deduped) — the
// golden-base thin-overlay dedup that motivates this backend over s3.
func packAndPush(ctx context.Context, dir string, dst oras.Target, tag, snapName string, includeMemory bool) (ocispec.Descriptor, transferStats, error) {
	var zero ocispec.Descriptor
	store, err := file.New(dir)
	if err != nil {
		return zero, transferStats{}, err
	}
	defer store.Close()

	names, err := regularFiles(dir)
	if err != nil {
		return zero, transferStats{}, err
	}
	if len(names) == 0 {
		return zero, transferStats{}, fmt.Errorf("no files to pack in %s", dir)
	}
	layers := make([]ocispec.Descriptor, 0, len(names))
	for _, n := range names {
		desc, err := store.Add(ctx, n, mediaTypeFor(n), n)
		if err != nil {
			return zero, transferStats{}, fmt.Errorf("add %s: %w", n, err)
		}
		layers = append(layers, desc)
	}
	annotations := map[string]string{}
	if snapName != "" {
		annotations["io.kubeswift.snapshot"] = snapName
	}
	if includeMemory {
		annotations["io.kubeswift.includeMemory"] = "true"
	}
	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, artifactType,
		oras.PackManifestOptions{Layers: layers, ManifestAnnotations: annotations})
	if err != nil {
		return zero, transferStats{}, fmt.Errorf("pack manifest: %w", err)
	}
	if err := store.Tag(ctx, manifestDesc, tag); err != nil {
		return zero, transferStats{}, fmt.Errorf("tag: %w", err)
	}

	var transferred, skipped int64
	opts := oras.DefaultCopyOptions
	opts.PostCopy = func(_ context.Context, d ocispec.Descriptor) error {
		atomic.AddInt64(&transferred, d.Size)
		return nil
	}
	opts.OnCopySkipped = func(_ context.Context, d ocispec.Descriptor) error {
		atomic.AddInt64(&skipped, d.Size)
		return nil
	}
	if _, err := oras.Copy(ctx, store, tag, dst, tag, opts); err != nil {
		return zero, transferStats{}, fmt.Errorf("push: %w", err)
	}
	return manifestDesc, transferStats{
		TransferredBytes: transferred,
		SkippedBytes:     skipped,
		TotalBytes:       transferred + skipped,
		ManifestDigest:   manifestDesc.Digest.String(),
	}, nil
}

// pullAndMaterialize pulls ref (a tag or a sha256 digest) from src and writes
// each layer to dir by its title annotation. ORAS verifies every blob's digest
// against the manifest, so a corrupt artifact fails loudly rather than
// materializing bad bytes onto the restore node.
func pullAndMaterialize(ctx context.Context, src oras.ReadOnlyTarget, ref, dir string) (ocispec.Descriptor, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ocispec.Descriptor{}, err
	}
	store, err := file.New(dir)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer store.Close()
	desc, err := oras.Copy(ctx, src, ref, store, ref, oras.DefaultCopyOptions)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pull: %w", err)
	}
	return desc, nil
}

// deleteArtifact removes the tag's manifest from the registry (idempotent — a
// missing manifest is success). Blob reclamation is the registry's GC, not
// ours; KubeSwift is a registry client.
func deleteArtifact(ctx context.Context, repo *remote.Repository, tag string) error {
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("resolve %s: %w", tag, err)
	}
	if err := repo.Delete(ctx, desc); err != nil {
		return fmt.Errorf("delete %s: %w", tag, err)
	}
	return nil
}

func regularFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// newRepository builds an authenticated ORAS remote for repoRef. Credentials
// come from the Docker config (DOCKER_CONFIG / ~/.docker/config.json — the
// dockerconfigjson pull-secret the controller mounts); anonymous when absent.
// insecure switches to plaintext HTTP for an in-cluster / test registry.
func newRepository(repoRef string, insecure bool) (*remote.Repository, error) {
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", repoRef, err)
	}
	repo.PlainHTTP = insecure
	if credStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		repo.Client = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: credentials.Credential(credStore),
		}
	}
	return repo, nil
}

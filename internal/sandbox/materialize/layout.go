package materialize

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ModeLayout materializes an OCI artifact as an OCI image layout directory
// (oci-layout, index.json, blobs/sha256/...): the referenced manifest, image
// or index or any artifact, with every blob it names, exactly as pushed. A
// workload reads it offline with any OCI-aware tool. Used by SwiftSandbox
// spec.artifacts (layout: oci).
const ModeLayout Mode = "oci"

// ResolveDescriptor returns the reference's repository and the digest of the
// manifest it names, as pushed: for an index, the index's own digest, not a
// platform image's. It is what spec.artifacts records and verifies.
func ResolveDescriptor(opts Options) (repository, digest string, err error) {
	ref, err := parseRef(opts)
	if err != nil {
		return "", "", err
	}
	desc, err := remote.Get(ref, opts.remoteOptions()...)
	if err != nil {
		return "", "", fmt.Errorf("resolve %q: %w", opts.ImageRef, err)
	}
	return ref.Context().Name(), desc.Digest.String(), nil
}

// MaterializeLayout writes opts.ImageRef as an OCI image layout under
// opts.CacheDir, keyed by the manifest digest, with the same per-digest lock
// and atomic rename as Materialize. A cache hit writes nothing.
func MaterializeLayout(opts Options) (*Result, error) {
	ref, err := parseRef(opts)
	if err != nil {
		return nil, err
	}
	desc, err := remote.Get(ref, opts.remoteOptions()...)
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", opts.ImageRef, err)
	}
	digest := desc.Digest.String()
	res := &Result{ImageRef: opts.ImageRef, Digest: digest, Mode: ModeLayout,
		RootfsPath: CachePathFor(opts.CacheDir, digest, ModeLayout)}
	if err := os.MkdirAll(opts.CacheDir, 0o755); err != nil {
		return res, fmt.Errorf("create cache dir: %w", err)
	}
	if _, err := os.Stat(res.RootfsPath); err == nil && !needsReadOnlyRepair(res.RootfsPath) {
		res.CacheHit = true
		return res, nil
	}
	if unlock, lerr := lockDigest(opts.CacheDir, digest); lerr == nil {
		defer unlock()
		if _, err := os.Stat(res.RootfsPath); err == nil {
			if err := repairReadOnly(res.RootfsPath, false); err != nil {
				return res, err
			}
			res.CacheHit = true
			return res, nil
		}
	}

	if opts.MaxBytes > 0 {
		size, err := layoutSize(desc)
		if err != nil {
			return res, fmt.Errorf("size of %s: %w", digest, err)
		}
		if size > opts.MaxBytes {
			return res, fmt.Errorf("%w: %s is %d bytes, the limit is %d", ErrTooLarge, digest, size, opts.MaxBytes)
		}
	}
	tmp, err := os.MkdirTemp(opts.CacheDir, ".materialize-*")
	if err != nil {
		return res, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "layout")
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		return res, fmt.Errorf("start layout: %w", err)
	}
	// The reference's name, so a tool reading the layout can find the manifest
	// by it (the OCI image-spec annotation for that purpose).
	annotations := layout.WithAnnotations(map[string]string{"org.opencontainers.image.ref.name": ref.String()})
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return res, fmt.Errorf("read index %s: %w", digest, err)
		}
		if err := lp.AppendIndex(idx, annotations); err != nil {
			return res, fmt.Errorf("write index %s: %w", digest, err)
		}
	} else {
		img, err := desc.Image()
		if err != nil {
			return res, fmt.Errorf("read manifest %s: %w", digest, err)
		}
		if err := lp.AppendImage(img, annotations); err != nil {
			return res, fmt.Errorf("write manifest %s: %w", digest, err)
		}
	}
	if n, err := dirSize(dir); err == nil {
		res.SizeBytes = n
	}
	// Readable by an unprivileged guest workload, writable by no one (perms.go).
	if _, err := normalizeReadOnly(dir, false); err != nil {
		return res, fmt.Errorf("normalize modes: %w", err)
	}
	if err := os.Rename(dir, res.RootfsPath); err != nil {
		if _, serr := os.Stat(res.RootfsPath); serr == nil {
			res.CacheHit = true // another writer won the race
			return res, nil
		}
		return res, fmt.Errorf("publish layout: %w", err)
	}
	if err := sealReadOnly(res.RootfsPath); err != nil {
		return res, err
	}
	return res, nil
}

func parseRef(opts Options) (name.Reference, error) {
	var nameOpts []name.Option
	if opts.Insecure {
		nameOpts = append(nameOpts, name.Insecure)
	}
	ref, err := name.ParseReference(opts.ImageRef, nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("parse ref %q: %w", opts.ImageRef, err)
	}
	if opts.PullSecret != "" {
		os.Setenv("DOCKER_CONFIG", filepath.Dir(opts.PullSecret))
	}
	return ref, nil
}

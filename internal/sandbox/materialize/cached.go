package materialize

import (
	"os"

	"github.com/google/go-containerregistry/pkg/name"
)

// CachedArtifact returns the node cache's entry for an artifact pinned by
// digest when it is ready to mount without asking the registry, as a warm
// checkout mounts it (swiftletd's warm_artifacts.rs): sealed, its content read
// from this repository, verified with keyFingerprint when one is given, and
// meeting every platform requirement. ok=false means the registry is needed.
//
// Nothing here authorizes the reader: the caller may use it only for a digest
// the controller has just resolved with the sandbox's own credentials.
func CachedArtifact(opts Options, keyFingerprint string, reqs []Requirement) (*Result, bool, error) {
	ref, err := parseRef(opts)
	if err != nil {
		return nil, false, err
	}
	d, ok := ref.(name.Digest)
	if !ok {
		return nil, false, nil
	}
	digest, repo := d.DigestStr(), ref.Context().Name()
	path := CachePathFor(opts.CacheDir, digest, opts.Mode)
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() ||
		!exists(SealedMarkerPath(path)) ||
		!exists(OriginMarkerPath(opts.CacheDir, digest, repo)) {
		return nil, false, nil
	}
	if keyFingerprint != "" && !exists(VerifiedMarkerPath(opts.CacheDir, digest, keyFingerprint)) {
		return nil, false, nil
	}
	if len(reqs) > 0 {
		canon, err := CanonicalRepository(opts.ImageRef)
		if err != nil {
			return nil, false, err
		}
		for _, req := range reqs {
			fp, err := RecordedRequirement(opts.CacheDir, canon, digest, req)
			if err != nil {
				return nil, false, err
			}
			if fp == "" {
				return nil, false, nil
			}
		}
	}
	return &Result{
		ImageRef:   opts.ImageRef,
		Digest:     digest,
		Mode:       opts.Mode,
		RootfsPath: path,
		CacheHit:   true,
		SizeBytes:  artifactSize(path, fi),
	}, true, nil
}

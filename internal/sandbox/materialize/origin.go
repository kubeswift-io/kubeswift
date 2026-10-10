package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Origin markers: which repositories a cached artifact may be used for.
//
// A registry authorizes per repository, and a manifest names its blobs by
// digest only. Resolving a reference with a sandbox's credentials proves that
// the registry serves that manifest to them, not that it serves the blobs:
// anyone can host a copy of a manifest (it is small and often public, in CI
// logs or SBOMs) in a repository they control, and resolve it to the same
// digest. On a cache hit no blob is fetched, so the shared node cache would
// hand them content they were never allowed to read.
//
// So an entry is used for repository R only when .origin/<digest>/<sha256(R)>
// exists. It is written after the entry is pulled from R, or, when the entry
// is already cached from elsewhere, after every blob has been read from R
// with its digest checked. A conforming registry does not serve a manifest
// whose blobs are not in the same repository, so a re-hosted manifest without
// its blobs fails there.
//
// .sealed/<entry> marks an entry complete: written last, after the entry's
// own modes. Both live beside the entries, never inside one, so an entry's
// content is exactly the artifact.

const (
	originDir = ".origin"
	sealedDir = ".sealed"
)

// RepositoryFingerprint identifies a repository (registry/name, as
// go-containerregistry names it): sha256 of the name, hex.
func RepositoryFingerprint(repository string) string {
	sum := sha256.Sum256([]byte(repository))
	return hex.EncodeToString(sum[:])
}

// OriginMarkerPath records that digest's content was read from repository.
func OriginMarkerPath(cacheDir, digest, repository string) string {
	return filepath.Join(cacheDir, originDir, strings.ReplaceAll(digest, ":", "-"), RepositoryFingerprint(repository))
}

// SealedMarkerPath marks the entry at entryPath complete.
func SealedMarkerPath(entryPath string) string {
	return filepath.Join(filepath.Dir(entryPath), sealedDir, filepath.Base(entryPath))
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// writeMarker creates an empty 0444 file at path atomically. Idempotent.
func writeMarker(path string) error {
	if exists(path) {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".marker-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	tmp.Close()
	if err := os.Chmod(name, 0o444); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func recordOrigin(cacheDir, digest, repository string) error {
	if err := writeMarker(OriginMarkerPath(cacheDir, digest, repository)); err != nil {
		return fmt.Errorf("record origin: %w", err)
	}
	return nil
}

// readBlob reads one blob to the end. go-containerregistry checks a remote
// blob's digest and size as it is read, so a registry that does not hold it,
// or serves other bytes, fails here.
func readBlob(rc io.ReadCloser, err error) error {
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

// checkImageBlobs reads every blob of img (config and layers) from its
// registry.
func checkImageBlobs(img v1.Image) error {
	if _, err := img.RawConfigFile(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	for _, l := range layers {
		d, _ := l.Digest()
		if err := readBlob(l.Compressed()); err != nil {
			return fmt.Errorf("blob %s: %w", d, err)
		}
	}
	return nil
}

func checkIndexBlobs(idx v1.ImageIndex, depth int) error {
	if depth > 4 {
		return fmt.Errorf("index nested more than 4 deep")
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return err
	}
	for _, d := range m.Manifests {
		switch {
		case d.MediaType.IsIndex():
			child, err := idx.ImageIndex(d.Digest)
			if err != nil {
				return err
			}
			if err := checkIndexBlobs(child, depth+1); err != nil {
				return err
			}
		case d.MediaType.IsImage():
			child, err := idx.Image(d.Digest)
			if err != nil {
				return err
			}
			if err := checkImageBlobs(child); err != nil {
				return err
			}
		default:
			return fmt.Errorf("cannot check a %s entry of an index", d.MediaType)
		}
	}
	return nil
}

// checkLayoutBlobs reads every blob a layout of desc holds from its
// repository.
func checkLayoutBlobs(desc *remote.Descriptor) error {
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return err
		}
		return checkIndexBlobs(idx, 0)
	}
	img, err := desc.Image()
	if err != nil {
		return err
	}
	return checkImageBlobs(img)
}

// prepareArtifactCache creates the artifact cache root, readable by root
// only: entries are readable by every guest user (perms.go), and the guest
// reaches them through virtiofsd, which runs as root. A local user on the
// node must not.
func prepareArtifactCache(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("cache dir mode: %w", err)
	}
	return nil
}

// useFromRepository lets a cached digest be used for repository: at once when
// its origin marker exists, otherwise after check reads every blob from it.
func useFromRepository(cacheDir, digest, repository string, check func() error) error {
	if exists(OriginMarkerPath(cacheDir, digest, repository)) {
		return nil
	}
	if err := check(); err != nil {
		return fmt.Errorf("%s is cached on this node, but %s did not serve its content: %w", digest, repository, err)
	}
	return recordOrigin(cacheDir, digest, repository)
}

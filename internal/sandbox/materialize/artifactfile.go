package materialize

import (
	"encoding/json"
	"fmt"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ModuleABIFile is the file a gpu-sandbox kernel artifact carries next to
// bzImage: the kernel release its loadable modules must be built for
// (`uname -r` in the guest, the prefix of a module's vermagic), for example
// "6.6.158-ksabi1". Kernel artifacts published before the guard have none.
const ModuleABIFile = "module-abi"

// ModuleABILabel is the image config label a GPU sandbox image sets to the
// module-abi it was built for. The controller refuses to boot a GPU sandbox
// whose image declares a different one (KernelModuleABIMismatch).
const ModuleABILabel = "io.kubeswift.gpu-sandbox.module-abi"

// maxArtifactFile bounds ArtifactFile: it reads small metadata files only.
const maxArtifactFile = 4 << 10

// ArtifactFile returns the content of the layer titled title
// (org.opencontainers.image.title, as `oras push` sets it) in the OCI artifact
// opts.ImageRef. found is false when the artifact has no such layer. Only the
// manifest and that one blob are fetched, and the blob is capped at 4 KiB.
func ArtifactFile(opts Options, title string) (content []byte, found bool, err error) {
	ref, err := parseRef(opts)
	if err != nil {
		return nil, false, err
	}
	desc, err := remote.Get(ref, opts.remoteOptions()...)
	if err != nil {
		return nil, false, fmt.Errorf("resolve %q: %w", opts.ImageRef, err)
	}
	var m v1.Manifest
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return nil, false, fmt.Errorf("read manifest of %q: %w", opts.ImageRef, err)
	}
	for _, l := range m.Layers {
		if l.Annotations["org.opencontainers.image.title"] != title {
			continue
		}
		if l.Size > maxArtifactFile {
			return nil, false, fmt.Errorf("%s in %q is %d bytes, more than %d", title, opts.ImageRef, l.Size, maxArtifactFile)
		}
		layer, err := remote.Layer(ref.Context().Digest(l.Digest.String()), opts.remoteOptions()...)
		if err != nil {
			return nil, false, fmt.Errorf("fetch %s from %q: %w", title, opts.ImageRef, err)
		}
		rc, err := layer.Compressed() // the blob as pushed
		if err != nil {
			return nil, false, fmt.Errorf("fetch %s from %q: %w", title, opts.ImageRef, err)
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxArtifactFile+1))
		if err != nil {
			return nil, false, fmt.Errorf("read %s from %q: %w", title, opts.ImageRef, err)
		}
		if len(b) > maxArtifactFile {
			return nil, false, fmt.Errorf("%s in %q is larger than %d bytes", title, opts.ImageRef, maxArtifactFile)
		}
		return b, true, nil
	}
	return nil, false, nil
}

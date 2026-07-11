package swiftsandbox

import (
	"os"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

const (
	// SandboxMaterializeImageEnv overrides the sandbox-materialize init image.
	SandboxMaterializeImageEnv = "KUBESWIFT_SANDBOX_MATERIALIZE_IMAGE"
	// SandboxMaterializeImageDefault is the fallback when the env is unset.
	SandboxMaterializeImageDefault = "ghcr.io/kubeswift-io/kubeswift/sandbox-materialize:latest"
	// rootfsCacheDir is the node-local RO-rootfs cache (hostPath), shared across
	// sandboxes and keyed by image digest.
	rootfsCacheDir = "/var/lib/kubeswift/sandbox-rootfs"
)

// SandboxMaterializeImage resolves the materialize init-container image (env
// override, else the pinned default). Mirrors swiftsnapshot.SnapshotS3Image.
func SandboxMaterializeImage() string {
	if v := os.Getenv(SandboxMaterializeImageEnv); v != "" {
		return v
	}
	return SandboxMaterializeImageDefault
}

// resolvedImage is the controller's upfront resolve of spec.Image.
type resolvedImage struct {
	Digest     string
	RootfsPath string // <cache>/<digest>.ext4 — deterministic, matches what the init produces
	Entrypoint string // resolved single entrypoint path (v1: [0] only; see note below)
}

// resolveImage does a cheap registry resolve (manifest + config, no layers) so
// the controller learns the digest -> deterministic cache path and the image
// entrypoint, and can build the launch intent before the materialize init runs.
// The init container independently resolves the same digest, so they agree.
func resolveImage(sb *sandboxv1alpha1.SwiftSandbox) (resolvedImage, error) {
	opts := materialize.Options{ImageRef: sb.Spec.Image, CacheDir: rootfsCacheDir, Mode: materialize.ModeBlock}
	img, digest, err := materialize.RemotePull(opts)
	if err != nil {
		return resolvedImage{}, err
	}
	cfg, err := materialize.ConfigFromImage(img)
	if err != nil {
		return resolvedImage{}, err
	}
	return resolvedImage{
		Digest:     digest,
		RootfsPath: materialize.CachePathFor(rootfsCacheDir, digest, materialize.ModeBlock),
		Entrypoint: resolveEntrypoint(sb, cfg),
	}, nil
}

// resolveEntrypoint picks the entrypoint path the bridge-initramfs execs.
//
// v1 limitation: the bridge execs a single path (switch_root <root> <entrypoint>),
// so only the first token is passed; args beyond it (e.g. an image CMD after its
// ENTRYPOINT) are not yet threaded through the cmdline. spec.command[0] wins;
// else the image ENTRYPOINT[0]/CMD[0]; else empty (the bridge falls back to
// /sbin/init then /bin/sh). Passing a full command line is a follow-up.
func resolveEntrypoint(sb *sandboxv1alpha1.SwiftSandbox, cfg materialize.ImageConfig) string {
	switch {
	case len(sb.Spec.Command) > 0:
		return sb.Spec.Command[0]
	case len(cfg.Entrypoint) > 0:
		return cfg.Entrypoint[0]
	case len(cfg.Cmd) > 0:
		return cfg.Cmd[0]
	}
	return ""
}

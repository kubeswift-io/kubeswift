package swiftsandbox

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	// modelCacheDir is the node-local RO-model cache (hostPath), shared across
	// sandboxes and keyed by model-image digest. A model is always materialized as
	// a tree (virtio-fs source), never an ext4 — the guest mounts it read-only.
	modelCacheDir = "/var/lib/kubeswift/sandbox-models"
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
	RootfsPath string // <cache>/<digest>{.ext4|/} — deterministic, matches what the init produces
	Virtiofs   bool   // rootfs delivered over virtio-fs (tree) instead of a block ext4
	Exec       execSpec
}

// execSpec is the full workload exec (argv + env + cwd), delivered to the guest
// via the per-sandbox config disk (NOT the kernel cmdline — that would leak env to
// /proc/cmdline + the host's ps/logs and cap at ~2-4KB).
type execSpec struct {
	Argv []string // full argv (command/args merged over the image entrypoint/cmd)
	Env  []string // "KEY=VAL", image env overlaid by spec.env
	Cwd  string   // working dir; honored on the cold path (bridge -> guest-agent chroot+chdir) and checkout path
}

// nonTrivial reports whether the exec spec carries anything worth a config disk.
// A bare scratch image with no overrides yields an empty spec — the bridge then
// falls back to /sbin/init -> /bin/sh with no config disk.
func (e execSpec) nonTrivial() bool {
	return len(e.Argv) > 0 || len(e.Env) > 0 || e.Cwd != ""
}

// pullSecretAuth builds an in-memory registry authenticator from a
// docker-registry Secret (spec.imagePullSecret) so the controller can resolve a
// PRIVATE image upfront — the controller pod has no mounted config.json, and
// setting the process-global DOCKER_CONFIG env would race concurrent reconciles.
// The Secret is read with an UNCACHED reader (get-only; no cluster-wide secrets
// informer). Returns nil (anonymous) when no pull secret is set.
func pullSecretAuth(ctx context.Context, reader client.Reader, namespace, secretName, imageRef string) (authn.Authenticator, error) {
	if secretName == "" {
		return nil, nil
	}
	var sec corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, &sec); err != nil {
		return nil, fmt.Errorf("read imagePullSecret %q: %w", secretName, err)
	}
	dcj := sec.Data[corev1.DockerConfigJsonKey] // ".dockerconfigjson"
	if len(dcj) == 0 {
		return nil, fmt.Errorf("imagePullSecret %q has no %s (is it a kubernetes.io/dockerconfigjson Secret?)", secretName, corev1.DockerConfigJsonKey)
	}
	return materialize.AuthFromDockerConfigJSON(dcj, imageRef)
}

// resolveImage does a cheap registry resolve (manifest + config, no layers) so
// the controller learns the digest -> deterministic cache path and the image
// entrypoint, and can build the launch intent before the materialize init runs.
// The init container independently resolves the same digest, so they agree.
// auth (from pullSecretAuth) authenticates a private image; nil = anonymous.
func resolveImage(sb *sandboxv1alpha1.SwiftSandbox, auth authn.Authenticator) (resolvedImage, error) {
	virtiofs := sb.Spec.RootfsMode == sandboxv1alpha1.SandboxRootfsVirtiofs
	mode := materialize.ModeBlock
	if virtiofs {
		mode = materialize.ModeTree
	}
	opts := materialize.Options{ImageRef: sb.Spec.Image, CacheDir: rootfsCacheDir, Mode: mode, Auth: auth}
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
		RootfsPath: materialize.CachePathFor(rootfsCacheDir, digest, mode),
		Virtiofs:   virtiofs,
		Exec:       resolveExec(sb, cfg),
	}, nil
}

// resolvedModel is the controller's upfront resolve of spec.model.
type resolvedModel struct {
	Digest   string
	TreePath string // <modelCache>/<digest>/ — the virtio-fs source, deterministic
}

// resolveModel does a cheap registry resolve (no layers) of the model image so
// the controller learns the digest -> deterministic node-cache tree path and can
// wire the virtio-fs share into the launch intent before the model-materialize
// init runs (which independently resolves the same digest, so they agree). A
// model is always a tree (virtio-fs). auth (from pullSecretAuth) authenticates a
// private model image; nil = anonymous.
func resolveModel(model *sandboxv1alpha1.SandboxModel, auth authn.Authenticator) (resolvedModel, error) {
	opts := materialize.Options{ImageRef: model.ImageRef, CacheDir: modelCacheDir, Mode: materialize.ModeTree, Auth: auth}
	_, digest, err := materialize.RemotePull(opts)
	if err != nil {
		return resolvedModel{}, err
	}
	return resolvedModel{
		Digest:   digest,
		TreePath: materialize.CachePathFor(modelCacheDir, digest, materialize.ModeTree),
	}, nil
}

// sandboxRootfsMode returns the materialize --mode string for the sandbox's
// rootfs mode ("tree" for virtio-fs, "block" for the default ext4).
func sandboxRootfsMode(sb *sandboxv1alpha1.SwiftSandbox) string {
	if sb.Spec.RootfsMode == sandboxv1alpha1.SandboxRootfsVirtiofs {
		return "tree"
	}
	return "block"
}

// resolveExec computes the full workload exec spec using k8s/OCI semantics, the
// SwiftSandbox spec merged over the image config:
//
//   - argv: spec.command overrides the image ENTRYPOINT; spec.args overrides the
//     image CMD. Per the k8s rule, setting command suppresses the image CMD unless
//     args are also given.
//   - env: the image env, then spec.env overrides by key (v1: literal Value only —
//     ValueFrom needs a downward-API/secret path not available in a microVM).
//   - cwd: spec.workingDir, else the image WorkingDir.
func resolveExec(sb *sandboxv1alpha1.SwiftSandbox, cfg materialize.ImageConfig) execSpec {
	var argv []string
	if len(sb.Spec.Command) > 0 {
		argv = append(argv, sb.Spec.Command...)
		argv = append(argv, sb.Spec.Args...)
	} else {
		argv = append(argv, cfg.Entrypoint...)
		if len(sb.Spec.Args) > 0 {
			argv = append(argv, sb.Spec.Args...)
		} else {
			argv = append(argv, cfg.Cmd...)
		}
	}
	cwd := sb.Spec.WorkingDir
	if cwd == "" {
		cwd = cfg.WorkingDir
	}
	return execSpec{Argv: argv, Env: mergeEnv(cfg.Env, sb.Spec.Env), Cwd: cwd}
}

// mergeEnv overlays spec.env (literal values) onto the image env, preserving the
// image order and appending new keys. Both are "KEY=VAL" on output.
func mergeEnv(imageEnv []string, specEnv []corev1.EnvVar) []string {
	val := map[string]string{}
	var order []string
	seen := func(k string) bool { _, ok := val[k]; return ok }
	for _, e := range imageEnv {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if !seen(k) {
			order = append(order, k)
		}
		val[k] = v
	}
	for _, e := range specEnv {
		if !seen(e.Name) {
			order = append(order, e.Name)
		}
		val[e.Name] = e.Value
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+val[k])
	}
	return out
}

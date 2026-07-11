package swiftsandbox

import (
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"

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
	Exec       execSpec
}

// execSpec is the full workload exec (argv + env + cwd), delivered to the guest
// via the per-sandbox config disk (NOT the kernel cmdline — that would leak env to
// /proc/cmdline + the host's ps/logs and cap at ~2-4KB).
type execSpec struct {
	Argv []string // full argv (command/args merged over the image entrypoint/cmd)
	Env  []string // "KEY=VAL", image env overlaid by spec.env
	Cwd  string   // working dir (v1: best-effort; the bridge defaults to /)
}

// nonTrivial reports whether the exec spec carries anything worth a config disk.
// A bare scratch image with no overrides yields an empty spec — the bridge then
// falls back to /sbin/init -> /bin/sh with no config disk.
func (e execSpec) nonTrivial() bool {
	return len(e.Argv) > 0 || len(e.Env) > 0 || e.Cwd != ""
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
		Exec:       resolveExec(sb, cfg),
	}, nil
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

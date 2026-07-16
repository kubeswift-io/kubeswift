// Package materialize turns an OCI image into a node-local VM root filesystem for
// a SwiftSandbox (mode-3 boot): it pulls the image by digest, flattens its layers
// (whiteouts applied), and produces either a read-only ext4 (block, the default)
// or an unpacked tree (virtio-fs). Results are keyed by digest under a node-local
// cache so co-located sandboxes of the same image share one copy (dedup) and a
// warm cache is a sub-second no-op.
//
// It runs as an init container in the launcher pod (the SwiftSandbox controller
// wires it — P4). Extraction preserves ownership/mode/device-nodes, so the
// container runs as root (mirrors the snapshot-s3 root lesson).
package materialize

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Mode selects the rootfs form.
type Mode string

const (
	// ModeBlock materializes a read-only ext4 image: <cacheDir>/<digest>.ext4.
	// The default; the bridge-initramfs mounts it as /dev/vda under a tmpfs overlay.
	ModeBlock Mode = "block"
	// ModeTree materializes an unpacked tree: <cacheDir>/<digest>/. For the
	// virtio-fs sandbox rootfs (one host tree shared across sandboxes).
	ModeTree Mode = "tree"
)

// ext4 sizing: the RO base does not grow, so headroom is only fs metadata +
// inodes. 1.5x the tree + a 64 MiB floor, rounded up to a whole MiB.
const (
	ext4FloorBytes = 64 << 20
	mib            = 1 << 20
)

// ImageConfig is the subset of the OCI image config the sandbox launch needs.
type ImageConfig struct {
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	Env        []string `json:"env,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"`
	User       string   `json:"user,omitempty"`
}

// Result is what the materialize step reports. The binary writes it to
// --result-file (default the container termination-log), so the controller reads
// the resolved digest + config from pod.status…terminated.message.
type Result struct {
	ImageRef   string      `json:"imageRef"`
	Digest     string      `json:"digest"` // sha256:...
	Mode       Mode        `json:"mode"`
	RootfsPath string      `json:"rootfsPath"`
	CacheHit   bool        `json:"cacheHit"`
	SizeBytes  int64       `json:"sizeBytes"`
	Config     ImageConfig `json:"config"`
}

// Options configure a materialize run.
type Options struct {
	ImageRef   string // an OCI reference; a digest ref is strongly preferred
	CacheDir   string // node-local cache root (hostPath)
	Mode       Mode   // ModeBlock (default) or ModeTree
	PullSecret string // path to a docker config.json for private registries; "" = anonymous
	// Auth is an in-memory registry authenticator. When set it takes precedence
	// over PullSecret/the default keychain — used by the controller, which reads
	// the pull Secret from the apiserver (it has no mounted config.json) and can't
	// set the process-global DOCKER_CONFIG env safely under concurrent reconciles.
	Auth     authn.Authenticator
	Insecure bool // allow a plain-HTTP registry (trusted in-cluster stores only)
}

// authOption resolves the go-containerregistry auth for a pull: an explicit
// in-memory authenticator (opts.Auth) wins; otherwise the default keychain reads
// $DOCKER_CONFIG (the mounted pull-secret path, set by RemotePull/Resolve).
func (opts Options) authOption() remote.Option {
	if opts.Auth != nil {
		return remote.WithAuth(opts.Auth)
	}
	return remote.WithAuthFromKeychain(authn.DefaultKeychain)
}

// AuthFromDockerConfigJSON builds a registry authenticator from a
// kubernetes.io/dockerconfigjson Secret's bytes for the registry that hosts
// imageRef. Returns authn.Anonymous when the config has no entry for that
// registry (so a pull secret scoped to a different registry degrades to
// anonymous rather than erroring). The controller uses this to resolve a private
// image upfront without mounting a config.json.
func AuthFromDockerConfigJSON(dockerConfigJSON []byte, imageRef string) (authn.Authenticator, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(dockerConfigJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse .dockerconfigjson: %w", err)
	}
	reg := ref.Context().RegistryStr()
	for host, entry := range cfg.Auths {
		if !registryHostMatches(host, reg) {
			continue
		}
		user, pass := entry.Username, entry.Password
		if entry.Auth != "" {
			decoded, derr := base64.StdEncoding.DecodeString(entry.Auth)
			if derr == nil {
				if u, p, ok := strings.Cut(string(decoded), ":"); ok {
					user, pass = u, p
				}
			}
		}
		if user == "" && pass == "" {
			return authn.Anonymous, nil
		}
		return authn.FromConfig(authn.AuthConfig{Username: user, Password: pass}), nil
	}
	return authn.Anonymous, nil
}

// registryHostMatches compares a docker-config auth key (which may carry a
// scheme, a path, or the docker.io "https://index.docker.io/v1/" alias) against a
// go-containerregistry registry host.
func registryHostMatches(configKey, registry string) bool {
	k := configKey
	k = strings.TrimPrefix(k, "https://")
	k = strings.TrimPrefix(k, "http://")
	if i := strings.IndexAny(k, "/"); i >= 0 {
		k = k[:i]
	}
	if k == registry {
		return true
	}
	// Docker Hub aliases.
	dockerAliases := registry == "index.docker.io" || registry == "docker.io" || registry == "registry-1.docker.io"
	keyIsDockerHub := configKey == "https://index.docker.io/v1/" || k == "index.docker.io" || k == "docker.io" || k == "registry-1.docker.io"
	return dockerAliases && keyIsDockerHub
}

// Puller resolves an Options to a pulled image and its digest ("sha256:..."). The
// default is RemotePull; tests inject a stub so the cache/config logic is
// exercised without a registry.
type Puller func(opts Options) (v1.Image, string, error)

// CachePathFor returns the node-local cache path for a digest. The ":" in
// "sha256:abc" is not filename-safe, so it is rendered "sha256-abc".
func CachePathFor(cacheDir, digest string, mode Mode) string {
	name := strings.ReplaceAll(digest, ":", "-")
	if mode == ModeTree {
		return filepath.Join(cacheDir, name)
	}
	return filepath.Join(cacheDir, name+".ext4")
}

// lockDigest takes a node-local exclusive advisory lock (flock) keyed by digest,
// serializing concurrent materializes of the same image on the same node. The
// lock file sits in the shared hostPath cache dir so the flock is visible across
// init containers. Returns an unlock func; the lock file is intentionally left in
// place (a 0-byte marker, reused next time — never the materialized artifact).
func lockDigest(cacheDir, digest string) (func(), error) {
	name := ".lock-" + strings.ReplaceAll(digest, ":", "-")
	f, err := os.OpenFile(filepath.Join(cacheDir, name), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open digest lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock digest lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// ext4SizeBytes sizes the RO ext4: 1.5x the tree + a 64 MiB floor, rounded up to
// a whole MiB (mkfs.ext4 -d fails if the size cannot hold the tree + metadata).
func ext4SizeBytes(treeBytes int64) int64 {
	sz := treeBytes + treeBytes/2 + ext4FloorBytes
	if sz < ext4FloorBytes {
		sz = ext4FloorBytes
	}
	// round up to MiB
	return ((sz + mib - 1) / mib) * mib
}

// ConfigFromImage extracts the launch-relevant config from an image. Exported so
// the SwiftSandbox controller can resolve the entrypoint upfront (a cheap
// manifest+config read, no layers) alongside RemotePull's digest.
func ConfigFromImage(img v1.Image) (ImageConfig, error) { return configFromImage(img) }

// configFromImage extracts the launch-relevant config from an image.
func configFromImage(img v1.Image) (ImageConfig, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return ImageConfig{}, fmt.Errorf("read image config: %w", err)
	}
	c := cf.Config
	return ImageConfig{
		Entrypoint: c.Entrypoint,
		Cmd:        c.Cmd,
		Env:        c.Env,
		WorkingDir: c.WorkingDir,
		User:       c.User,
	}, nil
}

// RemotePull pulls the image from its registry (the default Puller).
func RemotePull(opts Options) (v1.Image, string, error) {
	var nameOpts []name.Option
	if opts.Insecure {
		nameOpts = append(nameOpts, name.Insecure)
	}
	ref, err := name.ParseReference(opts.ImageRef, nameOpts...)
	if err != nil {
		return nil, "", fmt.Errorf("parse image ref %q: %w", opts.ImageRef, err)
	}
	// A mounted pull secret (config.json) is read via $DOCKER_CONFIG by the default
	// keychain; an in-memory opts.Auth (controller path) takes precedence.
	if opts.PullSecret != "" {
		os.Setenv("DOCKER_CONFIG", filepath.Dir(opts.PullSecret))
	}
	img, err := remote.Image(ref, opts.authOption())
	if err != nil {
		return nil, "", fmt.Errorf("pull %q: %w", opts.ImageRef, err)
	}
	dig, err := img.Digest()
	if err != nil {
		return nil, "", fmt.Errorf("resolve digest: %w", err)
	}
	return img, dig.String(), nil
}

// Resolve returns the image's canonical repository (registry/repo, no tag or
// digest) and its digest ("sha256:...") WITHOUT materializing it. It is the
// verify-before-boot gate's input: the caller cosign-verifies Repository@Digest
// before Materialize extracts any layer. A manifest fetch only — cheap.
func Resolve(opts Options) (repository, digest string, err error) {
	var nameOpts []name.Option
	if opts.Insecure {
		nameOpts = append(nameOpts, name.Insecure)
	}
	ref, err := name.ParseReference(opts.ImageRef, nameOpts...)
	if err != nil {
		return "", "", fmt.Errorf("parse image ref %q: %w", opts.ImageRef, err)
	}
	if opts.PullSecret != "" {
		os.Setenv("DOCKER_CONFIG", filepath.Dir(opts.PullSecret))
	}
	img, err := remote.Image(ref, opts.authOption())
	if err != nil {
		return "", "", fmt.Errorf("resolve %q: %w", opts.ImageRef, err)
	}
	dig, err := img.Digest()
	if err != nil {
		return "", "", fmt.Errorf("resolve digest: %w", err)
	}
	return ref.Context().Name(), dig.String(), nil
}

// Materialize resolves the image, and if the digest is not already cached,
// flattens it into the requested rootfs form. It is idempotent: a cache hit is a
// no-op that still reports the resolved digest + config.
func Materialize(opts Options, pull Puller) (*Result, error) {
	if opts.Mode == "" {
		opts.Mode = ModeBlock
	}
	if pull == nil {
		pull = RemotePull
	}
	if opts.CacheDir == "" {
		return nil, fmt.Errorf("cache dir is required")
	}
	img, digest, err := pull(opts)
	if err != nil {
		return nil, err
	}
	cfg, err := configFromImage(img)
	if err != nil {
		return nil, err
	}
	rootfsPath := CachePathFor(opts.CacheDir, digest, opts.Mode)
	res := &Result{
		ImageRef:   opts.ImageRef,
		Digest:     digest,
		Mode:       opts.Mode,
		RootfsPath: rootfsPath,
		Config:     cfg,
	}

	// Fast path — cache hit without locking: the digest is immutable, so an
	// existing artifact is authoritative and the common case pays no lock cost.
	if fi, err := os.Stat(rootfsPath); err == nil {
		res.CacheHit = true
		res.SizeBytes = artifactSize(rootfsPath, fi)
		return res, nil
	}

	if err := os.MkdirAll(opts.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	// Slow path — acquire a node-local per-digest lock so concurrent materializes
	// of the SAME image on the SAME node serialize (warm-pool slots when
	// replicas > nodes, or co-located sandboxes of one image): the first process
	// pulls + extracts, the rest wait then hit the cache below — instead of N
	// redundant parallel pulls of the same layers. The lock file lives in the
	// shared hostPath cache, so the flock is visible across init containers on the
	// node. Best-effort: if locking fails we proceed unlocked (the temp-dir +
	// atomic-rename below still keeps concurrent writers correct, just not deduped).
	if unlock, err := lockDigest(opts.CacheDir, digest); err == nil {
		defer unlock()
		// Re-check under the lock: another process may have finished while we waited.
		if fi, err := os.Stat(rootfsPath); err == nil {
			res.CacheHit = true
			res.SizeBytes = artifactSize(rootfsPath, fi)
			return res, nil
		}
	}

	// Flatten the image (whiteouts applied) into a temp tree next to the target,
	// then atomically move it into place so a crashed run never leaves a partial
	// artifact at the cache key.
	tmp, err := os.MkdirTemp(opts.CacheDir, ".materialize-*")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	treeDir := filepath.Join(tmp, "tree")
	if err := os.MkdirAll(treeDir, 0o755); err != nil {
		return nil, err
	}
	treeBytes, err := extractToTree(img, treeDir)
	if err != nil {
		return nil, fmt.Errorf("extract image: %w", err)
	}

	switch opts.Mode {
	case ModeTree:
		if err := os.Rename(treeDir, rootfsPath); err != nil {
			return nil, fmt.Errorf("publish tree: %w", err)
		}
		res.SizeBytes = treeBytes
	default: // ModeBlock
		tmpExt4 := filepath.Join(tmp, "rootfs.ext4")
		size := ext4SizeBytes(treeBytes)
		if err := mkfsExt4(treeDir, tmpExt4, size); err != nil {
			return nil, fmt.Errorf("mkfs.ext4: %w", err)
		}
		if err := os.Rename(tmpExt4, rootfsPath); err != nil {
			return nil, fmt.Errorf("publish ext4: %w", err)
		}
		res.SizeBytes = size
	}
	return res, nil
}

// extractToTree flattens the image and unpacks it via the system `tar` (which
// faithfully reconstructs ownership, mode, setuid, symlinks, hardlinks and device
// nodes when run as root — reimplementing that in Go is a footgun). Returns the
// on-disk tree size in bytes.
func extractToTree(img v1.Image, destDir string) (int64, error) {
	rc := mutate.Extract(img) // flattened rootfs tar, whiteouts already applied
	defer rc.Close()

	cmd := exec.Command("tar", "-C", destDir, "-xf", "-")
	cmd.Stdin = rc
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("tar extract: %w", err)
	}
	return dirSize(destDir)
}

// mkfsExt4 builds a read-only-content ext4 populated from treeDir at creation
// time (mkfs.ext4 -d — no loop mount, unprivileged-capable). Mirrors the sandbox
// verify-boot path.
func mkfsExt4(treeDir, ext4Path string, sizeBytes int64) error {
	sizeMiB := sizeBytes / mib
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-L", "sandbox-root",
		"-d", treeDir, "-b", "4096", ext4Path, fmt.Sprintf("%dM", sizeMiB))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func artifactSize(path string, fi os.FileInfo) int64 {
	if fi.IsDir() {
		if n, err := dirSize(path); err == nil {
			return n
		}
		return 0
	}
	return fi.Size()
}

// WriteResult writes the Result as JSON to resultFile (e.g. the container
// termination-log). A best-effort operation — a write failure does not fail the
// materialize (the artifact is already published).
func WriteResult(resultFile string, res *Result) error {
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return os.WriteFile(resultFile, data, 0o644)
}

// sandbox-materialize is the SwiftSandbox rootfs init container. It turns an OCI
// image into a node-local VM root filesystem (a read-only ext4 by default, or an
// unpacked tree for virtio-fs), keyed by digest under a shared node cache so
// co-located sandboxes of the same image reuse one copy. The SwiftSandbox
// controller (P4) runs it before swiftletd and reads the result (digest + image
// config) from the container termination message.
//
// Runs as root: flattening a container rootfs preserves ownership/mode/setuid and
// mkfs.ext4 -d populates the image — both want root (the snapshot-s3 lesson).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kubeswift-io/kubeswift/internal/oci"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
	"github.com/kubeswift-io/kubeswift/internal/version"
)

// stage is where a run failed: it decides an optional artifact's skip reason.
type stage int

const (
	stageResolve stage = iota
	stageVerify
	stageMaterialize
)

func main() {
	var (
		image      = flag.String("image", "", "OCI image reference (a digest ref is strongly preferred)")
		cacheDir   = flag.String("cache-dir", "/var/lib/kubeswift/sandbox-rootfs", "node-local rootfs cache root")
		mode       = flag.String("mode", "block", "form: block (ext4 rootfs), tree (unpacked layers) or oci (an OCI image layout of the manifest as pushed)")
		pullSecret = flag.String("pull-secret", "", "path to a docker config.json for private registries")
		insecure   = flag.Bool("insecure", false, "allow a plain-HTTP registry (trusted in-cluster stores only)")
		verifyKey  = flag.String("verify-key", "", "path to a cosign public key; when set, cosign-verify image@digest BEFORE materializing (requires a TLS registry)")
		readOnly   = flag.Bool("read-only-artifact", false, "normalize a tree to read-only artifact modes (dirs 0555, files 0444, executables 0555) so an unprivileged guest can read it; never for a rootfs. An oci layout always is")
		maxBytes   = flag.Int64("max-bytes", materialize.DefaultArtifactMaxBytes, "size cap for a read-only artifact (an oci layout, or a tree with --read-only-artifact); 0 is none. Never applies to a rootfs")
		maxEntries = flag.Int("max-entries", materialize.DefaultArtifactMaxEntries, "entry cap for an unpacked read-only artifact; 0 is none")
		timeout    = flag.Duration("timeout", 30*time.Minute, "bound on the whole run (resolve, verify, pull); a registry that stops answering fails the init container instead of holding it forever")
		deadline   = flag.String("deadline", "", "RFC 3339 time the whole run must end by, in addition to --timeout (an optional artifact's deadline)")
		optional   = flag.Bool("optional", false, "an optional artifact: any failure is recorded in the result as a skip, with a reason, and the run exits 0; the artifact is not marked ok")
		okFile     = flag.String("ok-file", "", "written once this run has resolved, verified (when a key is given) and materialized the artifact; its absence means not to mount it")
		useDir     = flag.String("use-dir", "", "the artifact cache's use-lock directory: each use of an entry is recorded there (its last use), and limits are enforced (artifacts only)")
		cacheMax   = flag.Int64("cache-max-bytes", 0, "artifact cache limit in bytes (0: none); least recently used entries no sandbox holds are evicted after an entry is written, and a new entry is refused if the cache stays over it")
		cachePct   = flag.Int("cache-max-percent", 0, "artifact cache limit as a percentage of its filesystem (0: none); the smaller limit applies")
		cacheAge   = flag.Duration("cache-min-age", 10*time.Minute, "an entry used more recently than this is never evicted")
		cacheOld   = flag.Bool("cache-evict-legacy", false, "also evict entries written before sizes were recorded; only once no older launcher can be serving them")
		resultFile = flag.String("result-file", "/dev/termination-log", "where to write the JSON result")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("sandbox-materialize %s (%s, %s)\n", version.Version, version.GitCommit, version.BuildDate)
		return
	}
	if *image == "" {
		fmt.Fprintln(os.Stderr, "sandbox-materialize: --image is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if *deadline != "" {
		d, err := time.Parse(time.RFC3339, *deadline)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-materialize: --deadline: %v\n", err)
			os.Exit(2)
		}
		var dcancel context.CancelFunc
		ctx, dcancel = context.WithDeadline(ctx, d)
		defer dcancel()
	}

	// The pull Secret is the tenant's input: only its "auths" are kept, in a
	// private copy, so neither go-containerregistry nor cosign (which reads
	// $DOCKER_CONFIG too) runs a credential helper it names.
	pullConfig := *pullSecret
	if pullConfig != "" {
		raw, err := os.ReadFile(pullConfig)
		if err == nil {
			raw, err = materialize.SanitizeDockerConfig(raw)
		}
		var dir string
		if err == nil {
			dir, err = os.MkdirTemp("", "docker-config-*")
		}
		if err == nil {
			pullConfig = filepath.Join(dir, "config.json")
			err = os.WriteFile(pullConfig, raw, 0o600)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-materialize: pull secret: %v\n", err)
			os.Exit(1)
		}
	}

	opts := materialize.Options{
		Context:    ctx,
		ImageRef:   *image,
		CacheDir:   *cacheDir,
		Mode:       materialize.Mode(*mode),
		PullSecret: pullConfig,
		Insecure:   *insecure,

		ReadOnlyArtifact: *readOnly,
	}
	// An artifact (never a rootfs) is capped, and its verification recorded
	// for warm-pool checkouts (materialize/verified.go).
	artifact := opts.Mode == materialize.ModeLayout || *readOnly
	if artifact {
		opts.MaxBytes, opts.MaxEntries = *maxBytes, *maxEntries
	}

	cache := cacheOpts{useDir: *useDir, limits: materialize.CacheLimits{MaxBytes: *cacheMax, MaxPercent: *cachePct, MinAge: *cacheAge, EvictLegacy: *cacheOld}}
	if !artifact {
		cache = cacheOpts{}
	}
	res, failed, err := run(ctx, opts, *verifyKey, *insecure, artifact, cache)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-materialize: %v\n", err)
		if !*optional {
			os.Exit(1)
		}
		// An optional artifact the workload starts without: why goes in the
		// result, and no ok file is written, so nothing mounts it.
		skip := &materialize.Result{ImageRef: *image, Skipped: &materialize.Skip{
			Reason: skipReason(ctx, failed, err), Message: err.Error()}}
		if werr := materialize.WriteResult(*resultFile, skip); werr != nil {
			fmt.Fprintf(os.Stderr, "sandbox-materialize: warning: write result: %v\n", werr)
		}
		return
	}

	if *okFile != "" {
		// Last: only a run that got this far says the artifact may be mounted.
		if err := os.WriteFile(*okFile, []byte(res.Digest+"\n"), 0o444); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-materialize: write ok file: %v\n", err)
			if !*optional {
				os.Exit(1)
			}
		}
	}
	if err := materialize.WriteResult(*resultFile, res); err != nil {
		// Non-fatal: the artifact is published; only the result surface failed.
		fmt.Fprintf(os.Stderr, "sandbox-materialize: warning: write result: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "sandbox-materialize: %s -> %s (digest=%s cacheHit=%t size=%dMiB)\n",
		res.ImageRef, res.RootfsPath, res.Digest, res.CacheHit, res.SizeBytes>>20)
}

// cacheOpts are the artifact cache's use-lock directory and limits.
type cacheOpts struct {
	useDir string
	limits materialize.CacheLimits
}

// run resolves, verifies (with a key) and materializes, and says at which
// stage it failed.
func run(ctx context.Context, opts materialize.Options, verifyKey string, insecure, artifact bool, cache cacheOpts) (*materialize.Result, stage, error) {
	// Verify-before-boot: resolve the digest and cosign-verify image@digest against
	// the pinned public key BEFORE materializing a single layer. A missing/invalid
	// signature fails here, so the init container fails and the sandbox goes
	// Failed (an optional artifact is skipped) -- it never mounts unverified
	// content. cosign speaks HTTPS only.
	if verifyKey != "" {
		if insecure {
			return nil, stageVerify, errors.New("--verify-key requires a TLS registry (incompatible with --insecure)")
		}
		resolve := materialize.Resolve
		if opts.Mode == materialize.ModeLayout {
			// An artifact is verified at the digest of the manifest it names
			// (an index's own, not a platform image's).
			resolve = materialize.ResolveDescriptor
		}
		repo, digest, err := resolve(opts)
		if err != nil {
			return nil, stageResolve, fmt.Errorf("resolve for verify: %w", err)
		}
		// The key is read once: cosign checks a private copy of these bytes
		// and the verification record is named from the same bytes, so a
		// key Secret changed meanwhile cannot have its fingerprint recorded
		// for a check another key passed.
		key, kerr := os.ReadFile(verifyKey)
		if kerr != nil || len(key) == 0 {
			return nil, stageVerify, fmt.Errorf("read verify key: %v", kerr)
		}
		pinned, perr := materialize.PinFile("", "verify-key-*.pub", key)
		if perr != nil {
			return nil, stageVerify, fmt.Errorf("pin verify key: %w", perr)
		}
		defer os.Remove(pinned)
		if err := oci.Verify(ctx, repo, digest, pinned); err != nil {
			return nil, stageVerify, err
		}
		// Materialize the digest we just verified, not the original ref. Otherwise
		// Materialize re-resolves the tag independently and an attacker with push
		// rights but no signing key could swap the tag between the verify and the
		// pull -- we would verify a good image and boot a different one. The
		// sibling snapshot-oras download-image path pins the same way.
		opts.ImageRef = repo + "@" + digest
		fmt.Fprintf(os.Stderr, "sandbox-materialize: cosign-verified %s@%s\n", repo, digest)
		if artifact {
			if err := materialize.RecordVerified(opts.CacheDir, digest, key); err != nil {
				return nil, stageVerify, err
			}
		}
	}
	var res *materialize.Result
	var err error
	if opts.Mode == materialize.ModeLayout {
		res, err = materialize.MaterializeLayout(opts)
	} else {
		res, err = materialize.Materialize(opts, nil)
	}
	if err != nil {
		return nil, stageMaterialize, err
	}
	if cache.useDir != "" {
		if err := accountEntry(opts.CacheDir, cache, res); err != nil {
			return nil, stageMaterialize, err
		}
	}
	return res, stageMaterialize, nil
}

// accountEntry records this use of the entry, its size when just written, and
// keeps the cache within its limits (materialize/cachegc.go). A cache that
// stays full refuses the entry.
func accountEntry(cacheDir string, cache cacheOpts, res *materialize.Result) error {
	entry := filepath.Base(res.RootfsPath)
	now := time.Now()
	if err := materialize.TouchUse(cache.useDir, entry, now); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-materialize: warning: record use: %v\n", err)
	}
	if !res.CacheHit {
		if _, err := materialize.RecordSize(cacheDir, entry); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-materialize: warning: record size: %v\n", err)
		}
	}
	if !cache.limits.Enabled() {
		return nil
	}
	rep, err := materialize.EnforceLimits(cacheDir, cache.useDir, entry, cache.limits, now)
	res.Cache = &rep
	if rep.Evicted > 0 || rep.OverLimit {
		fmt.Fprintf(os.Stderr, "sandbox-materialize: cache %d of %d bytes; evicted %d (%d bytes); over limit %t\n",
			rep.Bytes, rep.LimitBytes, rep.Evicted, rep.EvictedBytes, rep.OverLimit)
	}
	return err
}

// skipReason names why an optional artifact was skipped, from a fixed set.
func skipReason(ctx context.Context, failed stage, err error) string {
	switch {
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		return "FetchTimeout"
	case failed == stageVerify:
		return "VerificationFailed"
	case materialize.RegistryRefused(err):
		return "ResolveRefused"
	case failed == stageResolve || materialize.RegistryTransient(err):
		return "RegistryUnavailable"
	}
	return "MaterializeFailed"
}

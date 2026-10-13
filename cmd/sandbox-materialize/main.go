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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kubeswift-io/kubeswift/internal/oci"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
	"github.com/kubeswift-io/kubeswift/internal/version"
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

	// Verify-before-boot: resolve the digest and cosign-verify image@digest against
	// the pinned public key BEFORE materializing a single layer. A missing/invalid
	// signature exits non-zero here, so the init container fails and the sandbox
	// goes Failed — it never boots an unverified rootfs. cosign speaks HTTPS only.
	if *verifyKey != "" {
		if *insecure {
			fmt.Fprintln(os.Stderr, "sandbox-materialize: --verify-key requires a TLS registry (incompatible with --insecure)")
			os.Exit(1)
		}
		resolve := materialize.Resolve
		if opts.Mode == materialize.ModeLayout {
			// An artifact is verified at the digest of the manifest it names
			// (an index's own, not a platform image's).
			resolve = materialize.ResolveDescriptor
		}
		repo, digest, rerr := resolve(opts)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "sandbox-materialize: resolve for verify: %v\n", rerr)
			os.Exit(1)
		}
		// The key is read once: cosign checks a private copy of these bytes
		// and the verification record is named from the same bytes, so a
		// key Secret changed meanwhile cannot have its fingerprint recorded
		// for a check another key passed.
		key, kerr := os.ReadFile(*verifyKey)
		if kerr != nil || len(key) == 0 {
			fmt.Fprintf(os.Stderr, "sandbox-materialize: read verify key: %v\n", kerr)
			os.Exit(1)
		}
		pinned, perr := materialize.PinFile("", "verify-key-*.pub", key)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "sandbox-materialize: pin verify key: %v\n", perr)
			os.Exit(1)
		}
		defer os.Remove(pinned)
		if verr := oci.Verify(ctx, repo, digest, pinned); verr != nil {
			fmt.Fprintf(os.Stderr, "sandbox-materialize: %v\n", verr)
			os.Exit(1)
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
				fmt.Fprintf(os.Stderr, "sandbox-materialize: %v\n", err)
				os.Exit(1)
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
		fmt.Fprintf(os.Stderr, "sandbox-materialize: %v\n", err)
		os.Exit(1)
	}

	if err := materialize.WriteResult(*resultFile, res); err != nil {
		// Non-fatal: the artifact is published; only the result surface failed.
		fmt.Fprintf(os.Stderr, "sandbox-materialize: warning: write result: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "sandbox-materialize: %s -> %s (digest=%s cacheHit=%t size=%dMiB)\n",
		res.ImageRef, res.RootfsPath, res.Digest, res.CacheHit, res.SizeBytes>>20)
}

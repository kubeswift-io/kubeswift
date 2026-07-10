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
	"flag"
	"fmt"
	"os"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
	"github.com/kubeswift-io/kubeswift/internal/version"
)

func main() {
	var (
		image      = flag.String("image", "", "OCI image reference (a digest ref is strongly preferred)")
		cacheDir   = flag.String("cache-dir", "/var/lib/kubeswift/sandbox-rootfs", "node-local rootfs cache root")
		mode       = flag.String("mode", "block", "rootfs form: block (ext4) or tree (virtio-fs)")
		pullSecret = flag.String("pull-secret", "", "path to a docker config.json for private registries")
		insecure   = flag.Bool("insecure", false, "allow a plain-HTTP registry (trusted in-cluster stores only)")
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

	res, err := materialize.Materialize(materialize.Options{
		ImageRef:   *image,
		CacheDir:   *cacheDir,
		Mode:       materialize.Mode(*mode),
		PullSecret: *pullSecret,
		Insecure:   *insecure,
	}, nil)
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

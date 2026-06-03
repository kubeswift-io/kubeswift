// snapshot-s3 — the Tier C (object-storage export) uploader/downloader for
// KubeSwift SwiftSnapshot/SwiftRestore (Snapshot Phase 3).
//
// It mirrors a node-local snapshot directory to/from an S3-compatible object
// store, with a checksummed manifest so a corrupt object fails the restore
// loudly rather than booting a broken guest. Run as a short-lived Job container:
//
//	snapshot-s3 --mode=upload   --dir=/snap --bucket=B --key-prefix=ns/snap [--endpoint=...] [--path-style]
//	snapshot-s3 --mode=download --dir=/snap --bucket=B --key-prefix=ns/snap [--endpoint=...] [--path-style]
//
// Credentials come from the standard AWS environment variables (mounted from a
// Secret by the controller) — never from flags, annotations, or logs.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func main() {
	mode := flag.String("mode", "", "upload | download")
	dir := flag.String("dir", "", "local snapshot directory (source for upload, destination for download)")
	bucket := flag.String("bucket", "", "S3 bucket")
	keyPrefix := flag.String("key-prefix", "", "object key prefix for this snapshot (e.g. backups/ns/snap)")
	endpoint := flag.String("endpoint", "", "S3-compatible endpoint host[:port]; empty = AWS s3.amazonaws.com")
	region := flag.String("region", "", "S3 region")
	pathStyle := flag.Bool("path-style", false, "use path-style addressing (required by MinIO / Ceph RGW)")
	insecure := flag.Bool("insecure", false, "allow a plaintext (http) endpoint — UNSAFE; in-cluster MinIO on a trusted network only")
	snapName := flag.String("snapshot", "", "ns/name of the SwiftSnapshot, recorded in the manifest (upload only)")
	includeMemory := flag.Bool("include-memory", false, "record includeMemory=true in the manifest (upload only)")
	flag.Parse()

	if err := run(runArgs{
		mode: *mode, dir: *dir, bucket: *bucket, keyPrefix: *keyPrefix,
		endpoint: *endpoint, region: *region, snapName: *snapName,
		pathStyle: *pathStyle, insecure: *insecure, includeMemory: *includeMemory,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-s3:", err)
		os.Exit(1)
	}
}

type runArgs struct {
	mode, dir, bucket, keyPrefix, endpoint, region, snapName string
	pathStyle, insecure, includeMemory                       bool
}

func (a runArgs) validate() error {
	if a.mode != "upload" && a.mode != "download" {
		return fmt.Errorf("--mode must be \"upload\" or \"download\"")
	}
	if a.dir == "" || a.bucket == "" || a.keyPrefix == "" {
		return fmt.Errorf("--dir, --bucket and --key-prefix are required")
	}
	if a.endpoint == "" && a.insecure {
		return fmt.Errorf("--insecure has no effect with the default AWS endpoint (always TLS)")
	}
	return nil
}

func run(a runArgs) error {
	if err := a.validate(); err != nil {
		return err
	}
	store, err := newMinioStore(a.endpoint, a.region, a.bucket, a.pathStyle, !a.insecure)
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch a.mode {
	case "upload":
		return runUpload(ctx, store, a.dir, a.keyPrefix, a.snapName, a.includeMemory)
	case "download":
		return runDownload(ctx, store, a.dir, a.keyPrefix)
	}
	return nil
}

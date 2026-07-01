// snapshot-oras — the OCI-registry (ORAS) uploader/downloader for KubeSwift
// SwiftSnapshot/SwiftRestore (Snapshot "oci" backend).
//
// It packages a node-local snapshot directory (config.json/state.json/
// memory-ranges) as an OCI artifact and pushes/pulls it to/from any OCI
// registry via ORAS. Each file becomes one title-annotated layer, so a shared
// golden base dedups by digest and ORAS verifies every blob on pull. The
// registry is a declared external dependency (Harbor / Zot / a cloud registry),
// never embedded. Run as a short-lived Job container:
//
//	snapshot-oras --mode=upload   --dir=/snap --repository=REPO --tag=TAG [--insecure] [--snapshot=ns/name] [--include-memory]
//	snapshot-oras --mode=download --dir=/snap --repository=REPO --tag=TAG [--digest=sha256:...] [--insecure]
//	snapshot-oras --mode=delete   --repository=REPO --tag=TAG [--insecure]
//
// Registry credentials come from a Docker config (DOCKER_CONFIG points at the
// dockerconfigjson pull-secret the controller mounts) — never from flags or logs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// terminationMessagePath is the default Kubernetes terminationMessagePath. On a
// successful transfer we write the transferStats JSON here; the kubelet copies
// it into pod.status.containerStatuses[].state.terminated.message, which the
// controller reads to stamp status.oci + the byte counters — no kube client or
// RBAC in this Job. Overridable for tests.
var terminationMessagePath = "/dev/termination-log"

// reportTransfer best-effort writes the transfer stats to the termination
// message file. A write failure is non-fatal: the transfer already succeeded
// and the report is a metrics/status surface only (the controller treats a
// missing report as nil, never a failure).
func reportTransfer(s transferStats) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(terminationMessagePath, data, 0o644)
}

func main() {
	mode := flag.String("mode", "", "upload | download | delete")
	dir := flag.String("dir", "", "local snapshot directory (source for upload, destination for download)")
	repository := flag.String("repository", "", "OCI repository without a tag (e.g. ghcr.io/org/vm-snapshots)")
	tag := flag.String("tag", "", "artifact tag")
	digest := flag.String("digest", "", "pull by this manifest digest instead of the tag — pins the artifact (download only)")
	insecure := flag.Bool("insecure", false, "allow a plaintext (http) registry — UNSAFE; in-cluster / test registry only")
	snapName := flag.String("snapshot", "", "ns/name of the SwiftSnapshot, recorded in the manifest annotations (upload only)")
	includeMemory := flag.Bool("include-memory", false, "record includeMemory=true in the manifest (upload only)")
	signKey := flag.String("sign-key", "", "path to a cosign private key; when set, cosign-sign the pushed artifact as an OCI referrer (upload only; COSIGN_PASSWORD from env)")
	flag.Parse()

	if err := run(runArgs{
		mode: *mode, dir: *dir, repository: *repository, tag: *tag, digest: *digest,
		insecure: *insecure, snapName: *snapName, includeMemory: *includeMemory, signKey: *signKey,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-oras:", err)
		os.Exit(1)
	}
}

type runArgs struct {
	mode, dir, repository, tag, digest, snapName, signKey string
	insecure, includeMemory                               bool
}

func (a runArgs) validate() error {
	switch a.mode {
	case "upload", "download":
		if a.dir == "" {
			return fmt.Errorf("--dir is required for %s", a.mode)
		}
	case "delete":
		// delete operates purely on the registry (no local dir).
	default:
		return fmt.Errorf("--mode must be \"upload\", \"download\", or \"delete\"")
	}
	if a.repository == "" {
		return fmt.Errorf("--repository is required")
	}
	if a.tag == "" {
		return fmt.Errorf("--tag is required")
	}
	return nil
}

func run(a runArgs) error {
	if err := a.validate(); err != nil {
		return err
	}
	repo, err := newRepository(a.repository, a.insecure)
	if err != nil {
		return err
	}
	ctx := context.Background()
	ref := a.repository + ":" + a.tag

	switch a.mode {
	case "upload":
		_, stats, err := packAndPush(ctx, a.dir, repo, a.tag, a.snapName, a.includeMemory)
		if err != nil {
			return err
		}
		stats.Reference = ref
		if a.signKey != "" {
			// Sign the pushed digest as an OCI referrer. Strict: a signing
			// failure fails the Job (and the snapshot) — never report an
			// unsigned artifact as signed.
			if err := signArtifact(ctx, a.repository, stats.ManifestDigest, a.signKey, a.insecure); err != nil {
				return err
			}
			stats.Signed = true
		}
		reportTransfer(stats)
	case "download":
		pullRef := a.tag
		if a.digest != "" {
			pullRef = a.digest // pinned pull by digest
		}
		desc, err := pullAndMaterialize(ctx, repo, pullRef, a.dir)
		if err != nil {
			return err
		}
		// Download reports the footprint on the status side; wire-vs-skip byte
		// accounting is an upload-side concern.
		reportTransfer(transferStats{Reference: ref, ManifestDigest: desc.Digest.String()})
	case "delete":
		return deleteArtifact(ctx, repo, a.tag)
	}
	return nil
}

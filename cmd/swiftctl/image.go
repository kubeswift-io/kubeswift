package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kubeswift-io/kubeswift/internal/oci"
)

// Golden-image publishing CLI (P3 producer side). `image publish` chunks a local
// raw/qcow2 golden VM disk and pushes it to an OCI registry as a sparse,
// zero-skipping, content-addressed artifact — the producer half of the
// SwiftImage.spec.source.oci consumer the controller already ships. It runs
// entirely client-side (no cluster needed), so it composes with packer /
// virt-install / CI pipelines. See docs/registry/golden-images.md.

var (
	publishTo        string
	publishTag       string
	publishChunkMiB  int
	publishOSType    string
	publishInsecure  bool
	publishSignKey   string
	publishKeepInput bool
)

// qcow2Magic is the 4-byte qcow2 header ("QFI\xfb"). A golden disk is often
// distributed as qcow2; the OCI artifact stores RAW (the import pipeline expects
// image.raw), so a qcow2 input is converted with qemu-img before chunking.
var qcow2Magic = []byte{0x51, 0x46, 0x49, 0xfb}

var imageCmd = &cobra.Command{
	Use:   "image",
	Short: "Publish and manage golden VM disk images in an OCI registry",
	Long: `Golden VM disk images in an OCI registry. "image publish" chunks a local raw
or qcow2 disk and pushes it as a content-addressed, zero-skipping artifact that a
SwiftImage can consume via spec.source.oci.`,
	RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
}

var imagePublishCmd = &cobra.Command{
	Use:          "publish [disk]",
	Short:        "Chunk a local raw/qcow2 golden disk and push it to an OCI registry",
	SilenceUsage: true,
	Long: `Chunk a local golden VM disk (raw or qcow2) and push it to an OCI registry as
a sparse, content-addressed artifact.

The disk is streamed in fixed-size windows: all-zero windows are never stored
(a raw disk is ~90% sparse) and windows already present in the registry are
deduped by content digest — so re-publishing a lightly-changed v1.1 transfers
only the changed blocks. A qcow2 input is converted to raw with qemu-img first
(the artifact always stores raw, which is what the SwiftImage import expects).

Credentials come from your Docker config (run "docker login <registry>" first);
the push is anonymous if none are present. --sign-key cosign-signs the pushed
artifact (COSIGN_PASSWORD from the environment) so a consumer can verify it.

Consume the result from a SwiftImage:

  spec:
    source:
      oci:
        repository: REPO
        tag: TAG`,
	Example: `  swiftctl image publish ubuntu-noble.qcow2 --to ghcr.io/acme/vm-images --tag noble-24.04
  swiftctl image publish golden.raw --to zot.registry.svc:5000/vm-images --tag base --insecure
  COSIGN_PASSWORD=... swiftctl image publish golden.raw \
    --to ghcr.io/acme/vm-images --tag base --sign-key cosign.key`,
	Args: cobra.ExactArgs(1),
	RunE: runImagePublish,
}

func init() {
	imagePublishCmd.Flags().StringVar(&publishTo, "to", "", "Target OCI repository WITHOUT a tag, e.g. ghcr.io/acme/vm-images (required)")
	imagePublishCmd.Flags().StringVar(&publishTag, "tag", "latest", "Artifact tag")
	imagePublishCmd.Flags().IntVar(&publishChunkMiB, "chunk-size-mib", 64, "Chunk size in MiB (larger = fewer layers; smaller = finer cross-version dedup)")
	imagePublishCmd.Flags().StringVar(&publishOSType, "os-type", "linux", "OS family recorded in the artifact config: linux | windows")
	imagePublishCmd.Flags().BoolVar(&publishInsecure, "insecure", false, "Allow a plaintext (http) registry — UNSAFE; in-cluster / test registry only")
	imagePublishCmd.Flags().StringVar(&publishSignKey, "sign-key", "", "Path to a cosign private key; when set, cosign-sign the pushed artifact (COSIGN_PASSWORD from env)")
	imagePublishCmd.Flags().BoolVar(&publishKeepInput, "keep-converted", false, "Keep the temporary raw file produced from a qcow2 input (default: delete it)")
	_ = imagePublishCmd.MarkFlagRequired("to")
	imageCmd.AddCommand(imagePublishCmd)
}

func runImagePublish(cmd *cobra.Command, args []string) error {
	inputPath := args[0]
	out := cmd.OutOrStdout()
	if publishChunkMiB <= 0 {
		return fmt.Errorf("--chunk-size-mib must be positive")
	}
	if _, err := os.Stat(inputPath); err != nil {
		return fmt.Errorf("input disk %q: %w", inputPath, err)
	}

	// A qcow2 input is converted to raw first — the artifact stores raw, which is
	// what the SwiftImage import pipeline (download-image -> image.raw) expects.
	rawPath, cleanup, err := ensureRaw(cmd, inputPath)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := context.Background()
	repo, err := oci.NewRepository(publishTo, publishInsecure)
	if err != nil {
		return err
	}
	ref := publishTo + ":" + publishTag
	fmt.Fprintf(out, "Publishing %s -> %s (chunk %d MiB)\n", filepath.Base(inputPath), ref, publishChunkMiB)

	_, res, err := oci.ChunkAndPush(ctx, rawPath, repo, publishTag, int64(publishChunkMiB)*1024*1024, "raw", publishOSType)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	// "Not transferred" = sparse (all-zero) windows never stored + chunks already
	// in the registry (deduped). SkippedBytes counts only the deduped subset.
	notTransferred := res.TotalBytes - res.TransferredBytes
	if notTransferred < 0 {
		notTransferred = 0
	}
	savedPct := 0.0
	if res.TotalBytes > 0 {
		savedPct = 100 * float64(notTransferred) / float64(res.TotalBytes)
	}
	fmt.Fprintf(out, "\nPushed %s\n", ref)
	fmt.Fprintf(out, "  digest:      %s\n", res.ManifestDigest)
	fmt.Fprintf(out, "  disk size:   %s\n", humanizeBytes(res.TotalBytes))
	fmt.Fprintf(out, "  transferred: %s (%.1f%% skipped — sparse + deduped)\n",
		humanizeBytes(res.TransferredBytes), savedPct)
	if res.SkippedBytes > 0 {
		fmt.Fprintf(out, "  deduped:     %s already present in the registry\n", humanizeBytes(res.SkippedBytes))
	}

	if publishSignKey != "" {
		// Strict: a signing failure is an error — never report an unsigned push
		// as signed. cosign verify is HTTPS-only, so an --insecure push cannot be
		// verified later (documented in docs/registry/golden-images.md).
		if err := oci.Sign(ctx, publishTo, res.ManifestDigest, publishSignKey, publishInsecure); err != nil {
			return err
		}
		fmt.Fprintf(out, "  signed:      true (cosign, key %s)\n", publishSignKey)
	}

	fmt.Fprintf(out, "\nConsume from a SwiftImage:\n")
	fmt.Fprintf(out, "  spec:\n    source:\n      oci:\n        repository: %s\n        tag: %s\n", publishTo, publishTag)
	if publishInsecure {
		fmt.Fprintf(out, "        insecure: true\n")
	}
	return nil
}

// ensureRaw returns a path to a RAW disk for chunking. If input is qcow2 it is
// converted with qemu-img to a temporary raw file (cleanup removes it unless
// --keep-converted); a raw input is used in place (cleanup is a no-op).
func ensureRaw(cmd *cobra.Command, input string) (rawPath string, cleanup func(), err error) {
	noop := func() {}
	isQcow2, err := looksLikeQCOW2(input)
	if err != nil {
		return "", noop, err
	}
	if !isQcow2 {
		return input, noop, nil // assume raw
	}
	qemuImg, lerr := exec.LookPath("qemu-img")
	if lerr != nil {
		return "", noop, fmt.Errorf("input %q is qcow2 but qemu-img is not on PATH; convert it first:\n  qemu-img convert -O raw %s golden.raw\nthen: swiftctl image publish golden.raw ...", input, input)
	}
	tmp, err := os.CreateTemp("", "swiftctl-golden-*.raw")
	if err != nil {
		return "", noop, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	cleanup = func() {
		if publishKeepInput {
			fmt.Fprintf(cmd.OutOrStdout(), "(kept converted raw: %s)\n", tmpPath)
			return
		}
		_ = os.Remove(tmpPath)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Converting qcow2 -> raw (qemu-img)...\n")
	conv := exec.CommandContext(context.Background(), qemuImg, "convert", "-O", "raw", input, tmpPath)
	conv.Stderr = cmd.ErrOrStderr()
	if err := conv.Run(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("qemu-img convert: %w", err)
	}
	return tmpPath, cleanup, nil
}

// looksLikeQCOW2 reports whether the file begins with the qcow2 magic. A read
// error is returned; a file shorter than the magic is treated as not-qcow2.
func looksLikeQCOW2(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, len(qcow2Magic))
	n, err := f.Read(head)
	if err != nil && n < len(qcow2Magic) {
		return false, nil // too short to be qcow2
	}
	return bytes.Equal(head, qcow2Magic), nil
}

// humanizeBytes renders a byte count as a human-readable string (binary units).
func humanizeBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// Cross-cluster snapshot-object transfer (the cold-migration recipe in
// docs/snapshots/cold-migration.md). A SwiftSnapshot's registry coordinates —
// the oci artifact refs/digests and the captured launcher-sufficient surface —
// live in STATUS, which `kubectl apply` cannot set. "export-manifest" emits a
// portable JSON manifest of a Ready snapshot; "import-manifest" recreates it in
// another cluster in two steps (create spec, then transplant status via the
// status subresource) so a source-independent `swiftctl guest import` can run
// there against the shared registry.

var (
	exportManifestOut         string
	exportManifestKeepDelPol  bool
	importManifestFile        string
	importManifestToNamespace string
)

var snapshotExportManifestCmd = &cobra.Command{
	Use:          "export-manifest [name]",
	Short:        "Emit a portable manifest of a Ready SwiftSnapshot for another cluster",
	SilenceUsage: true,
	Long: `Emit a portable JSON manifest of a Ready SwiftSnapshot — spec plus the
status the import side needs (oci artifact refs/digests + the captured guest
surface). Feed it to "swiftctl snapshot import-manifest" against the target
cluster; the artifacts themselves travel via the shared OCI registry.

By default the emitted spec is rewritten to deletionPolicy: Retain — the copy
must NOT own the shared registry artifacts' lifecycle (deleting a Delete-policy
copy in the target cluster would purge artifacts other clusters still use).
--keep-deletion-policy preserves the original value.`,
	Example: `  swiftctl snapshot export-manifest db-export -o db-export.json
  # then, against the target cluster:
  swiftctl snapshot import-manifest -f db-export.json`,
	Args: cobra.ExactArgs(1),
	RunE: runSnapshotExportManifest,
}

var snapshotImportManifestCmd = &cobra.Command{
	Use:          "import-manifest",
	Short:        "Recreate an exported SwiftSnapshot (spec + status) in this cluster",
	SilenceUsage: true,
	Long: `Recreate a SwiftSnapshot from an export-manifest file: creates the object,
then transplants the exported status via the status subresource (which kubectl
apply cannot set). Run against the TARGET cluster's kubeconfig/context.

For a full-state (cold-migration) snapshot, import it into a namespace with
the SAME NAME as the source's original namespace — the captured memory's
config.json records paths derived from it (see
docs/snapshots/cold-migration.md).`,
	Example: `  swiftctl snapshot import-manifest -f db-export.json
  swiftctl snapshot import-manifest -f db-export.json -n team-a`,
	RunE: runSnapshotImportManifest,
}

func init() {
	snapshotExportManifestCmd.Flags().StringVarP(&exportManifestOut, "output", "o", "", "Write the manifest to this file (default: stdout)")
	snapshotExportManifestCmd.Flags().BoolVar(&exportManifestKeepDelPol, "keep-deletion-policy", false, "Preserve the original spec.deletionPolicy instead of rewriting the copy to Retain")
	snapshotImportManifestCmd.Flags().StringVarP(&importManifestFile, "file", "f", "", "Manifest file from export-manifest (required)")
	_ = snapshotImportManifestCmd.MarkFlagRequired("file")

	snapshotCmd.AddCommand(snapshotExportManifestCmd)
	snapshotCmd.AddCommand(snapshotImportManifestCmd)
}

// portableSnapshotManifest strips cluster-local metadata (uid/resourceVersion/
// timestamps/managedFields/finalizers/ownerRefs) so the object is creatable in
// another cluster, keeps spec + status verbatim — except spec.deletionPolicy,
// which is rewritten to Retain unless keepDeletionPolicy: the copy must not own
// the shared registry artifacts' lifecycle (the target cluster's controller
// re-adds the cleanup finalizer to any Ready snapshot, and a Delete-policy copy
// would purge artifacts other clusters still use).
func portableSnapshotManifest(snap *snapshotv1alpha1.SwiftSnapshot, keepDeletionPolicy bool) *snapshotv1alpha1.SwiftSnapshot {
	out := &snapshotv1alpha1.SwiftSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: snapshotv1alpha1.GroupName + "/" + snapshotv1alpha1.Version,
			Kind:       "SwiftSnapshot",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        snap.Name,
			Namespace:   snap.Namespace,
			Labels:      snap.Labels,
			Annotations: snap.Annotations,
		},
		Spec:   snap.Spec,
		Status: snap.Status,
	}
	if !keepDeletionPolicy {
		out.Spec.DeletionPolicy = snapshotv1alpha1.SnapshotDeletionPolicyRetain
	}
	return out
}

// importSnapshotManifest recreates the manifest's snapshot: Create with the
// status stripped (the apiserver ignores status on create), then transplant the
// exported status via the status subresource. Fails if the object already
// exists — an existing snapshot may reference different artifacts, and silently
// merging would be worse than asking the operator to resolve it.
func importSnapshotManifest(ctx context.Context, c client.Client, manifest *snapshotv1alpha1.SwiftSnapshot) error {
	obj := portableSnapshotManifest(manifest, true) // manifest spec is authoritative; re-clean metadata defensively
	status := obj.Status
	obj.Status = snapshotv1alpha1.SwiftSnapshotStatus{}
	if err := c.Create(ctx, obj); err != nil {
		if errors.IsAlreadyExists(err) {
			return fmt.Errorf("SwiftSnapshot %s/%s already exists — delete it first (or import under a different name)", obj.Namespace, obj.Name)
		}
		return fmt.Errorf("create SwiftSnapshot: %w", err)
	}
	var created snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(ctx, client.ObjectKey{Name: obj.Name, Namespace: obj.Namespace}, &created); err != nil {
		return err
	}
	created.Status = status
	if err := c.Status().Update(ctx, &created); err != nil {
		return fmt.Errorf("transplant status (subresource): %w", err)
	}
	return nil
}

func runSnapshotExportManifest(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	var snap snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &snap); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("SwiftSnapshot %s/%s not found", ns, name)
		}
		return err
	}
	if snap.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseReady {
		return fmt.Errorf("SwiftSnapshot %s/%s is %s, not Ready — export a Ready snapshot (the manifest must carry final artifact refs)", ns, name, snap.Status.Phase)
	}
	out := portableSnapshotManifest(&snap, exportManifestKeepDelPol)
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if exportManifestOut == "" {
		_, err = cmd.OutOrStdout().Write(data)
		return err
	}
	if err := os.WriteFile(exportManifestOut, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s (SwiftSnapshot %s/%s", exportManifestOut, out.Namespace, out.Name)
	if !exportManifestKeepDelPol && snap.Spec.DeletionPolicy != snapshotv1alpha1.SnapshotDeletionPolicyRetain {
		fmt.Fprintf(cmd.OutOrStdout(), "; deletionPolicy rewritten to Retain for the copy")
	}
	fmt.Fprintf(cmd.OutOrStdout(), ")\nImport on the target cluster: swiftctl snapshot import-manifest -f %s\n", exportManifestOut)
	return nil
}

func runSnapshotImportManifest(cmd *cobra.Command, _ []string) error {
	data, err := os.ReadFile(importManifestFile)
	if err != nil {
		return err
	}
	var manifest snapshotv1alpha1.SwiftSnapshot
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse %s: %w", importManifestFile, err)
	}
	if manifest.Name == "" {
		return fmt.Errorf("%s carries no metadata.name — is this an export-manifest file?", importManifestFile)
	}
	// -n overrides the manifest's namespace (note the full-state same-name
	// caveat in the long help).
	if ns := getNamespace(); namespace != "" {
		manifest.Namespace = ns
	}
	if manifest.Namespace == "" {
		manifest.Namespace = getNamespace()
	}
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	if err := importSnapshotManifest(context.Background(), c, &manifest); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Imported SwiftSnapshot %s/%s (phase=%s", manifest.Namespace, manifest.Name, manifest.Status.Phase)
	if manifest.Status.OCI != nil {
		fmt.Fprintf(out, ", memory=%s", manifest.Status.OCI.ManifestDigest)
		if manifest.Status.OCI.Disk != nil {
			fmt.Fprintf(out, ", disk=%s", manifest.Status.OCI.Disk.ManifestDigest)
		}
	}
	fmt.Fprintln(out, ")")
	fmt.Fprintf(out, "Import a guest from it: swiftctl guest import <new-guest> --from-snapshot %s --target-node <node> --guest-class <class>\n", manifest.Name)
	return nil
}

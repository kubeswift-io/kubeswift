package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// Cold / suspended-state migration CLI (P4). "guest export" captures a running
// VM's full state (memory + disk) to an OCI registry via a full-state
// SwiftSnapshot (capture-then-terminate: the source is stopped at the snapshot
// instant); "guest import" resumes it elsewhere as a cloneFromSnapshot guest.
// Thin wrappers over the SwiftSnapshot(oci,includeDisk) + cloneFromSnapshot
// mechanism — see docs/snapshots/cold-migration.md.

var (
	exportRepo        string
	exportTag         string
	exportInsecure    bool
	exportCredsSecret string
	exportSignKey     string
	exportSnapName    string
	exportWait        bool
	exportTimeout     time.Duration

	importFromSnap   string
	importTargetNode string
	importGuestClass string
	importWait       bool
	importTimeout    time.Duration
)

var guestCmd = &cobra.Command{
	Use:   "guest",
	Short: "Cold / suspended-state migration of a SwiftGuest via an OCI registry",
	Long: `Move a VM's full state (memory + disk) between nodes or clusters through an
OCI registry. "guest export" suspends a running guest and pushes a full-state
artifact pair; "guest import" resumes it elsewhere. The registry is the async
seam — the source cluster and target cluster never talk directly.`,
	RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
}

var guestExportCmd = &cobra.Command{
	Use:          "export [guest]",
	Short:        "Capture a running SwiftGuest's full state (memory + disk) to an OCI registry",
	SilenceUsage: true,
	Long: `Capture a running SwiftGuest's full state (memory + disk) to an OCI registry
as a full-state SwiftSnapshot (backend=oci, includeMemory + includeDisk).

This is capture-then-terminate: the guest is paused, its memory is snapshotted,
the source is STOPPED (runPolicy=Stopped) at the snapshot instant, and a
memory + disk artifact pair is pushed to the registry. The source stays down —
this is a migration, not a backup. Resume it elsewhere (another node or cluster)
with "swiftctl guest import".`,
	Example: `  swiftctl guest export db --to ghcr.io/acme/vm-snapshots --wait
  swiftctl guest export db --to zot.registry.svc:5000/snapshots --insecure --wait
  swiftctl guest export db --to ghcr.io/acme/vm-snapshots \
    --credentials-secret regcreds --sign-key cosign-key`,
	Args: cobra.ExactArgs(1),
	RunE: runGuestExport,
}

var guestImportCmd = &cobra.Command{
	Use:          "import [new-guest]",
	Short:        "Resume a full-state snapshot as a new SwiftGuest (clone-from-snapshot)",
	SilenceUsage: true,
	Long: `Resume a full-state SwiftSnapshot as a new SwiftGuest via cloneFromSnapshot.
The new guest CH-restores the captured memory against a root disk materialized
from the snapshot's OCI disk artifact — it resumes where the source left off
(not a fresh boot).

The referenced SwiftSnapshot must be Ready in this namespace. --guest-class is
required (a clone needs a guestClassRef even though CPU/memory come from the
snapshot); --target-node pins where the clone runs (and where its disk is
downloaded).`,
	Example: `  swiftctl guest import db2 --from-snapshot db-export --target-node boba --guest-class ft-small --wait`,
	Args:    cobra.ExactArgs(1),
	RunE:    runGuestImport,
}

func init() {
	guestExportCmd.Flags().StringVar(&exportRepo, "to", "", "Target OCI repository WITHOUT a tag, e.g. ghcr.io/acme/vm-snapshots (required)")
	guestExportCmd.Flags().StringVar(&exportTag, "tag", "", "Artifact tag (default: <namespace>-<snapshot>)")
	guestExportCmd.Flags().BoolVar(&exportInsecure, "insecure", false, "Allow a plaintext (http) registry — UNSAFE; in-cluster / test registry only")
	guestExportCmd.Flags().StringVar(&exportCredsSecret, "credentials-secret", "", "Name of a kubernetes.io/dockerconfigjson Secret (same namespace) with registry credentials")
	guestExportCmd.Flags().StringVar(&exportSignKey, "sign-key", "", "Name of a Secret (same namespace) with a cosign keypair (cosign.key + cosign.password) to sign the artifact")
	guestExportCmd.Flags().StringVar(&exportSnapName, "name", "", "SwiftSnapshot name (default: <guest>-export)")
	guestExportCmd.Flags().BoolVar(&exportWait, "wait", false, "Wait for the export to reach Ready and print the artifact references")
	guestExportCmd.Flags().DurationVar(&exportTimeout, "timeout", 15*time.Minute, "Timeout for --wait")
	_ = guestExportCmd.MarkFlagRequired("to")

	guestImportCmd.Flags().StringVar(&importFromSnap, "from-snapshot", "", "Name of a Ready full-state SwiftSnapshot in this namespace (required)")
	guestImportCmd.Flags().StringVar(&importTargetNode, "target-node", "", "Node to run the resumed clone on (required for an oci snapshot)")
	guestImportCmd.Flags().StringVar(&importGuestClass, "guest-class", "", "SwiftGuestClass for the clone (required; resources come from the snapshot but a guestClassRef is mandatory)")
	guestImportCmd.Flags().BoolVar(&importWait, "wait", false, "Wait for the imported guest to reach Running")
	guestImportCmd.Flags().DurationVar(&importTimeout, "timeout", 15*time.Minute, "Timeout for --wait")
	_ = guestImportCmd.MarkFlagRequired("from-snapshot")
	_ = guestImportCmd.MarkFlagRequired("target-node")
	_ = guestImportCmd.MarkFlagRequired("guest-class")

	guestCmd.AddCommand(guestExportCmd)
	guestCmd.AddCommand(guestImportCmd)
}

func runGuestExport(cmd *cobra.Command, args []string) error {
	guestName := args[0]
	ns := getNamespace()
	snapName := exportSnapName
	if snapName == "" {
		snapName = guestName + "-export"
	}
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	snap := buildExportSnapshot(snapName, ns, guestName, exportRepo, exportTag, exportInsecure, exportCredsSecret, exportSignKey)
	if err := c.Create(context.Background(), snap); err != nil {
		return fmt.Errorf("create SwiftSnapshot: %w", err)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Exporting %s/%s -> SwiftSnapshot %s (full-state: memory + disk, backend=oci, repo=%s)\n",
		ns, guestName, snapName, exportRepo)
	fmt.Fprintf(out, "The source is stopped at the snapshot instant and stays down (cold migration).\n")
	if !exportWait {
		fmt.Fprintf(out, "\nWatch:  kubectl get swiftsnapshot -n %s %s -w\n", ns, snapName)
		fmt.Fprintf(out, "Import: swiftctl guest import <new-guest> --from-snapshot %s --target-node <node> --guest-class <class>\n", snapName)
		return nil
	}
	return waitExportReady(cmd, c, ns, snapName)
}

func waitExportReady(cmd *cobra.Command, c client.Client, ns, snapName string) error {
	out := cmd.OutOrStdout()
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()
	for {
		var got snapshotv1alpha1.SwiftSnapshot
		if err := c.Get(ctx, client.ObjectKey{Name: snapName, Namespace: ns}, &got); err != nil {
			return err
		}
		switch got.Status.Phase {
		case snapshotv1alpha1.SwiftSnapshotPhaseReady:
			fmt.Fprintf(out, "\nReady. Artifacts:\n")
			if got.Status.OCI != nil {
				fmt.Fprintf(out, "  memory: %s (%s)\n", got.Status.OCI.Reference, got.Status.OCI.ManifestDigest)
				if got.Status.OCI.Disk != nil {
					fmt.Fprintf(out, "  disk:   %s (%s)\n", got.Status.OCI.Disk.Reference, got.Status.OCI.Disk.ManifestDigest)
				}
				if got.Status.OCI.Signed {
					fmt.Fprintf(out, "  signed: true\n")
				}
			}
			fmt.Fprintf(out, "\nImport: swiftctl guest import <new-guest> --from-snapshot %s --target-node <node> --guest-class <class>\n", snapName)
			return nil
		case snapshotv1alpha1.SwiftSnapshotPhaseFailed:
			return fmt.Errorf("export failed: %s", terminalConditionMessage(got.Status.Conditions))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s waiting for %s to be Ready (phase=%s)", exportTimeout, snapName, got.Status.Phase)
		case <-time.After(2 * time.Second):
		}
	}
}

func runGuestImport(cmd *cobra.Command, args []string) error {
	newName := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	guest := buildImportGuest(newName, ns, importFromSnap, importTargetNode, importGuestClass)
	if err := c.Create(context.Background(), guest); err != nil {
		return fmt.Errorf("create SwiftGuest: %w", err)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Importing SwiftSnapshot %s -> SwiftGuest %s/%s (resume on node %s)\n",
		importFromSnap, ns, newName, importTargetNode)
	if !importWait {
		fmt.Fprintf(out, "\nWatch: kubectl get swiftguest -n %s %s -w\n", ns, newName)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()
	for {
		var got swiftv1alpha1.SwiftGuest
		if err := c.Get(ctx, client.ObjectKey{Name: newName, Namespace: ns}, &got); err != nil {
			return err
		}
		switch got.Status.Phase {
		case swiftv1alpha1.SwiftGuestPhaseRunning:
			fmt.Fprintf(out, "\nRunning on %s", got.Status.NodeName)
			if got.Status.Network != nil && got.Status.Network.PrimaryIP != "" {
				fmt.Fprintf(out, " (IP %s)", got.Status.Network.PrimaryIP)
			}
			fmt.Fprintln(out)
			return nil
		case swiftv1alpha1.SwiftGuestPhaseFailed:
			return fmt.Errorf("import failed (guest phase=Failed); see: swiftctl describe %s -n %s", newName, ns)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s waiting for %s to be Running (phase=%s)", importTimeout, newName, got.Status.Phase)
		case <-time.After(2 * time.Second):
		}
	}
}

// buildExportSnapshot constructs the full-state (memory + disk) oci SwiftSnapshot
// that "guest export" creates. Extracted for unit testing the spec shape.
func buildExportSnapshot(name, ns, guestName, repo, tag string, insecure bool, credsSecret, signKey string) *snapshotv1alpha1.SwiftSnapshot {
	oci := &snapshotv1alpha1.OCIBackend{
		Repository: repo,
		Tag:        tag,
		Insecure:   insecure,
	}
	if credsSecret != "" {
		oci.CredentialsSecretRef = &snapshotv1alpha1.SecretObjectReference{Name: credsSecret}
	}
	if signKey != "" {
		oci.SigningKeySecretRef = &snapshotv1alpha1.SecretObjectReference{Name: signKey}
	}
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef:      snapshotv1alpha1.SwiftSnapshotGuestRef{Name: guestName},
			IncludeMemory: true,
			IncludeDisk:   true,
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendOCI,
				OCI:  oci,
			},
		},
	}
}

// buildImportGuest constructs the cloneFromSnapshot SwiftGuest that "guest import"
// creates. Extracted for unit testing the spec shape.
func buildImportGuest(name, ns, fromSnap, targetNode, guestClass string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			CloneFromSnapshot: &swiftv1alpha1.CloneFromSnapshotSource{
				SnapshotRef: corev1.LocalObjectReference{Name: fromSnap},
				TargetNode:  targetNode,
			},
			GuestClassRef: corev1.LocalObjectReference{Name: guestClass},
			RunPolicy:     swiftv1alpha1.RunPolicyRunning,
		},
	}
}

// terminalConditionMessage returns the message of the first False condition, or
// a generic note. Used to surface why an export Failed.
func terminalConditionMessage(conds []metav1.Condition) string {
	for _, c := range conds {
		if c.Status == metav1.ConditionFalse && c.Message != "" {
			return c.Message
		}
	}
	return "see: swiftctl snapshot describe"
}

package main

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

var (
	snapshotGuestRef    string
	snapshotVSClass     string
	snapshotIncludeMem  bool
	snapshotResumeAfter bool
	snapshotListAllNS   bool
	snapshotBackend     string
	snapshotHostPath    string
)

var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Manage SwiftSnapshot resources (csi-volume-snapshot or local backend)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var snapshotCreateCmd = &cobra.Command{
	Use:          "create [name]",
	Short:        "Create a SwiftSnapshot of a SwiftGuest",
	SilenceUsage: true,
	Long: `Create a SwiftSnapshot. Two backends are supported:

  csi-volume-snapshot (default): captures the SwiftGuest's per-guest
    root-disk PVC crash-consistently — the VM is not paused. Disk
    state only; --include-memory has no effect on this backend.

  local: full VM state (memory + disks) captured into a hostPath
    directory on the node where the source VM is running. The VM is
    paused for the duration of the capture (~2.8s/GiB on Longhorn).
    Requires --hostpath under /var/lib/kubeswift/snapshots/.`,
	Example: `  swiftctl snapshot create db-2026-04-25 --guest db
  swiftctl snapshot create snap1 --guest myvm --vsclass csi-hostpath-snapclass
  swiftctl snapshot create db-mem-2026-04-26 --guest db --backend local \
    --hostpath /var/lib/kubeswift/snapshots/default-db-mem-2026-04-26`,
	Args: cobra.ExactArgs(1),
	RunE: runSnapshotCreate,
}

var snapshotListCmd = &cobra.Command{
	Use:          "list",
	Aliases:      []string{"ls"},
	Short:        "List SwiftSnapshots",
	SilenceUsage: true,
	RunE:         runSnapshotList,
}

var snapshotDescribeCmd = &cobra.Command{
	Use:          "describe [name]",
	Short:        "Print human-readable details of a SwiftSnapshot",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runSnapshotDescribe,
}

var snapshotDeleteCmd = &cobra.Command{
	Use:          "delete [name]",
	Short:        "Delete a SwiftSnapshot (and its underlying VolumeSnapshot)",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runSnapshotDelete,
}

func init() {
	snapshotCreateCmd.Flags().StringVar(&snapshotGuestRef, "guest", "", "SwiftGuest to snapshot (required)")
	snapshotCreateCmd.Flags().StringVar(&snapshotBackend, "backend", "csi-volume-snapshot", "Snapshot backend: csi-volume-snapshot or local")
	snapshotCreateCmd.Flags().StringVar(&snapshotVSClass, "vsclass", "", "VolumeSnapshotClass name (csi-volume-snapshot only; default: cluster default)")
	snapshotCreateCmd.Flags().StringVar(&snapshotHostPath, "hostpath", "", "On-node directory for local backend (required when --backend=local; must be under /var/lib/kubeswift/snapshots/)")
	snapshotCreateCmd.Flags().BoolVar(&snapshotIncludeMem, "include-memory", true, "Capture memory (ignored on csi-volume-snapshot)")
	snapshotCreateCmd.Flags().BoolVar(&snapshotResumeAfter, "resume", true, "Resume the source VM after snapshot (no-op on csi-volume-snapshot)")
	_ = snapshotCreateCmd.MarkFlagRequired("guest")

	snapshotListCmd.Flags().BoolVarP(&snapshotListAllNS, "all-namespaces", "A", false, "List across all namespaces")

	snapshotCmd.AddCommand(snapshotCreateCmd)
	snapshotCmd.AddCommand(snapshotListCmd)
	snapshotCmd.AddCommand(snapshotDescribeCmd)
	snapshotCmd.AddCommand(snapshotDeleteCmd)
}

func newSnapshotClient() (client.Client, error) {
	cfg, err := kubeConfig.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: scheme.Scheme})
}

func runSnapshotCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	backend, err := parseBackendFlag(snapshotBackend)
	if err != nil {
		return err
	}
	spec := snapshotv1alpha1.SwiftSnapshotSpec{
		GuestRef:            snapshotv1alpha1.SwiftSnapshotGuestRef{Name: snapshotGuestRef},
		Backend:             snapshotv1alpha1.SwiftSnapshotBackend{Type: backend},
		IncludeMemory:       snapshotIncludeMem,
		ResumeAfterSnapshot: snapshotResumeAfter,
	}
	switch backend {
	case snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot:
		spec.Backend.CSIVolumeSnapshot = &snapshotv1alpha1.CSIVolumeSnapshotBackend{
			VolumeSnapshotClassName: snapshotVSClass,
		}
		if snapshotHostPath != "" {
			return fmt.Errorf("--hostpath is only valid for --backend=local")
		}
	case snapshotv1alpha1.SnapshotBackendLocal:
		if snapshotHostPath == "" {
			return fmt.Errorf("--hostpath is required for --backend=local")
		}
		if snapshotVSClass != "" {
			return fmt.Errorf("--vsclass is only valid for --backend=csi-volume-snapshot")
		}
		spec.Backend.Local = &snapshotv1alpha1.LocalBackend{HostPath: snapshotHostPath}
	}
	snap := &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
	if err := c.Create(context.Background(), snap); err != nil {
		return fmt.Errorf("create SwiftSnapshot: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created SwiftSnapshot %s/%s (guest=%s, backend=%s)\n",
		ns, name, snapshotGuestRef, backend)
	return nil
}

// parseBackendFlag maps the --backend flag value to a SnapshotBackendType.
// Catches typos at the CLI level rather than letting them surface as a
// webhook rejection later.
func parseBackendFlag(v string) (snapshotv1alpha1.SnapshotBackendType, error) {
	switch v {
	case "csi-volume-snapshot", "":
		return snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot, nil
	case "local":
		return snapshotv1alpha1.SnapshotBackendLocal, nil
	default:
		return "", fmt.Errorf("unknown --backend %q (want csi-volume-snapshot or local)", v)
	}
}

func runSnapshotList(cmd *cobra.Command, _ []string) error {
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	var list snapshotv1alpha1.SwiftSnapshotList
	opts := []client.ListOption{}
	if !snapshotListAllNS {
		opts = append(opts, client.InNamespace(getNamespace()))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tGUEST\tBACKEND\tPHASE\tSIZE\tAGE")
	for _, s := range list.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			s.Namespace, s.Name, s.Spec.GuestRef.Name, s.Spec.Backend.Type,
			s.Status.Phase, s.Status.TotalSizeBytes,
			cliAge(s.CreationTimestamp.Time))
	}
	return w.Flush()
}

func runSnapshotDescribe(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	var s snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &s); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("SwiftSnapshot %s/%s not found", ns, name)
		}
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Name:        %s\n", s.Name)
	fmt.Fprintf(out, "Namespace:   %s\n", s.Namespace)
	fmt.Fprintf(out, "Guest:       %s\n", s.Spec.GuestRef.Name)
	fmt.Fprintf(out, "Backend:     %s\n", s.Spec.Backend.Type)
	if s.Spec.Backend.CSIVolumeSnapshot != nil && s.Spec.Backend.CSIVolumeSnapshot.VolumeSnapshotClassName != "" {
		fmt.Fprintf(out, "VSClass:     %s\n", s.Spec.Backend.CSIVolumeSnapshot.VolumeSnapshotClassName)
	}
	if s.Spec.Backend.Local != nil {
		fmt.Fprintf(out, "HostPath:    %s\n", s.Spec.Backend.Local.HostPath)
	}
	fmt.Fprintf(out, "Phase:       %s\n", s.Status.Phase)
	if s.Status.CapturedAt != nil {
		fmt.Fprintf(out, "CapturedAt:  %s\n", s.Status.CapturedAt.Time.Format("2006-01-02 15:04:05Z"))
	}
	if s.Status.Hypervisor != "" {
		fmt.Fprintf(out, "Hypervisor:  %s\n", s.Status.Hypervisor)
	}
	if s.Status.HypervisorVersion != "" {
		fmt.Fprintf(out, "HV Version:  %s\n", s.Status.HypervisorVersion)
	}
	if s.Status.NodeName != "" {
		fmt.Fprintf(out, "Node:        %s\n", s.Status.NodeName)
	}
	if s.Status.SnapshotDirVersion != "" {
		fmt.Fprintf(out, "Dir Version: %s\n", s.Status.SnapshotDirVersion)
	}
	if s.Status.ObservedPauseWindowMs > 0 {
		fmt.Fprintf(out, "Pause Window: %dms\n", s.Status.ObservedPauseWindowMs)
	}
	if s.Status.GuestSpec != nil && s.Status.GuestSpec.ImageName != "" {
		fmt.Fprintf(out, "Image:       %s\n", s.Status.GuestSpec.ImageName)
	}
	for _, d := range s.Status.Disks {
		fmt.Fprintf(out, "Disk:        role=%s size=%d handle=%s\n", d.Role, d.SizeBytes, d.Handle)
	}
	if s.Status.MemorySnapshot != nil {
		fmt.Fprintf(out, "Memory:      handle=%s\n", s.Status.MemorySnapshot.Handle)
	}
	if len(s.Status.Conditions) > 0 {
		fmt.Fprintln(out, "Conditions:")
		for _, c := range s.Status.Conditions {
			fmt.Fprintf(out, "  %s=%s reason=%s message=%q\n", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	return nil
}

func runSnapshotDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	snap := &snapshotv1alpha1.SwiftSnapshot{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := c.Delete(context.Background(), snap); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("SwiftSnapshot %s/%s not found", ns, name)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Deleted SwiftSnapshot %s/%s\n", ns, name)
	return nil
}

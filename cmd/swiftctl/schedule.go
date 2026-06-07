package main

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

var (
	scheduleGuestRef    string
	scheduleCron        string
	scheduleBackend     string
	scheduleVSClass     string
	scheduleHostPath    string
	scheduleIncludeMem  bool
	scheduleKeepLast    int
	scheduleSuspend     bool
	scheduleConcurrency string
	scheduleListAllNS   bool
)

var scheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Manage SwiftSnapshotSchedule resources (cron-scheduled snapshots + keep-N retention)",
	RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
}

var scheduleCreateCmd = &cobra.Command{
	Use:          "create [name]",
	Short:        "Create a SwiftSnapshotSchedule",
	SilenceUsage: true,
	Long: `Create a SwiftSnapshotSchedule: snapshot a SwiftGuest on a cron schedule,
keeping only the most recent --keep-last.

Backends mirror 'swiftctl snapshot create' (csi-volume-snapshot default, or
local). For the s3 backend, apply a YAML manifest (it needs bucket/endpoint/
credentials). NOTE: the local backend writes a fixed --hostpath, so scheduled
local snapshots overwrite each other — use csi-volume-snapshot (or s3) for
scheduling.`,
	Example: `  swiftctl schedule create nightly-db --guest db --schedule "0 2 * * *" --keep-last 7
  swiftctl schedule create hourly --guest web --schedule "0 * * * *" --keep-last 24 --vsclass csi-hostpath-snapclass`,
	Args: cobra.ExactArgs(1),
	RunE: runScheduleCreate,
}

var scheduleListCmd = &cobra.Command{
	Use:          "list",
	Aliases:      []string{"ls"},
	Short:        "List SwiftSnapshotSchedules",
	SilenceUsage: true,
	RunE:         runScheduleList,
}

var scheduleDescribeCmd = &cobra.Command{
	Use:          "describe [name]",
	Short:        "Print human-readable details of a SwiftSnapshotSchedule",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runScheduleDescribe,
}

var scheduleDeleteCmd = &cobra.Command{
	Use:          "delete [name]",
	Short:        "Delete a SwiftSnapshotSchedule (its snapshots are cascade-deleted)",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runScheduleDelete,
}

func init() {
	scheduleCreateCmd.Flags().StringVar(&scheduleGuestRef, "guest", "", "SwiftGuest to snapshot (required)")
	scheduleCreateCmd.Flags().StringVar(&scheduleCron, "schedule", "", "5-field cron expression, UTC (required), e.g. \"0 2 * * *\"")
	scheduleCreateCmd.Flags().StringVar(&scheduleBackend, "backend", "csi-volume-snapshot", "Snapshot backend: csi-volume-snapshot or local")
	scheduleCreateCmd.Flags().StringVar(&scheduleVSClass, "vsclass", "", "VolumeSnapshotClass name (csi-volume-snapshot only)")
	scheduleCreateCmd.Flags().StringVar(&scheduleHostPath, "hostpath", "", "On-node directory for local backend (under /var/lib/kubeswift/snapshots/)")
	scheduleCreateCmd.Flags().BoolVar(&scheduleIncludeMem, "include-memory", true, "Backend-determined (no-op on csi-volume-snapshot, which is disk-only)")
	scheduleCreateCmd.Flags().IntVar(&scheduleKeepLast, "keep-last", 0, "Keep only the most recent N Ready snapshots (0 = keep all; rely on per-snapshot ttl)")
	scheduleCreateCmd.Flags().BoolVar(&scheduleSuspend, "suspend", false, "Create the schedule suspended")
	scheduleCreateCmd.Flags().StringVar(&scheduleConcurrency, "concurrency", "Forbid", "Concurrency policy: Forbid (skip while a prior capture runs) or Allow")
	_ = scheduleCreateCmd.MarkFlagRequired("guest")
	_ = scheduleCreateCmd.MarkFlagRequired("schedule")

	scheduleListCmd.Flags().BoolVarP(&scheduleListAllNS, "all-namespaces", "A", false, "List across all namespaces")

	scheduleCmd.AddCommand(scheduleCreateCmd, scheduleListCmd, scheduleDescribeCmd, scheduleDeleteCmd)
}

func runScheduleCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	backend, err := parseBackendFlag(scheduleBackend)
	if err != nil {
		return err
	}
	tmpl := snapshotv1alpha1.SwiftSnapshotSpec{
		GuestRef:      snapshotv1alpha1.SwiftSnapshotGuestRef{Name: scheduleGuestRef},
		Backend:       snapshotv1alpha1.SwiftSnapshotBackend{Type: backend},
		IncludeMemory: scheduleIncludeMem,
	}
	switch backend {
	case snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot:
		tmpl.Backend.CSIVolumeSnapshot = &snapshotv1alpha1.CSIVolumeSnapshotBackend{VolumeSnapshotClassName: scheduleVSClass}
		if scheduleHostPath != "" {
			return fmt.Errorf("--hostpath is only valid for --backend=local")
		}
	case snapshotv1alpha1.SnapshotBackendLocal:
		if scheduleHostPath == "" {
			return fmt.Errorf("--hostpath is required for --backend=local")
		}
		tmpl.Backend.Local = &snapshotv1alpha1.LocalBackend{HostPath: scheduleHostPath}
	}

	sched := &snapshotv1alpha1.SwiftSnapshotSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotScheduleSpec{
			Schedule:          scheduleCron,
			Suspend:           scheduleSuspend,
			ConcurrencyPolicy: snapshotv1alpha1.SnapshotConcurrencyPolicy(scheduleConcurrency),
			Template:          snapshotv1alpha1.SnapshotTemplate{Spec: tmpl},
		},
	}
	if scheduleKeepLast > 0 {
		sched.Spec.Retention = &snapshotv1alpha1.SnapshotScheduleRetention{KeepLast: ptr.To(int32(scheduleKeepLast))}
	}
	if err := c.Create(context.Background(), sched); err != nil {
		return fmt.Errorf("create SwiftSnapshotSchedule: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created SwiftSnapshotSchedule %s/%s (guest=%s, schedule=%q, keepLast=%d)\n",
		ns, name, scheduleGuestRef, scheduleCron, scheduleKeepLast)
	return nil
}

func runScheduleList(cmd *cobra.Command, _ []string) error {
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	var list snapshotv1alpha1.SwiftSnapshotScheduleList
	opts := []client.ListOption{}
	if !scheduleListAllNS {
		opts = append(opts, client.InNamespace(getNamespace()))
	}
	if err := c.List(context.Background(), &list, opts...); err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tSCHEDULE\tSUSPEND\tGUEST\tKEEP\tLAST-SCHEDULE\tAGE")
	for _, s := range list.Items {
		keep := "-"
		if s.Spec.Retention != nil && s.Spec.Retention.KeepLast != nil {
			keep = fmt.Sprintf("%d", *s.Spec.Retention.KeepLast)
		}
		last := "-"
		if s.Status.LastScheduleTime != nil {
			last = cliAge(s.Status.LastScheduleTime.Time)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\t%s\t%s\t%s\n",
			s.Namespace, s.Name, s.Spec.Schedule, s.Spec.Suspend,
			s.Spec.Template.Spec.GuestRef.Name, keep, last, cliAge(s.CreationTimestamp.Time))
	}
	return w.Flush()
}

func runScheduleDescribe(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	var s snapshotv1alpha1.SwiftSnapshotSchedule
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &s); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("SwiftSnapshotSchedule %s/%s not found", ns, name)
		}
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Name:               %s\n", s.Name)
	fmt.Fprintf(out, "Namespace:          %s\n", s.Namespace)
	fmt.Fprintf(out, "Schedule:           %s\n", s.Spec.Schedule)
	fmt.Fprintf(out, "Suspend:            %t\n", s.Spec.Suspend)
	fmt.Fprintf(out, "ConcurrencyPolicy:  %s\n", s.Spec.ConcurrencyPolicy)
	if s.Spec.StartingDeadlineSeconds != nil {
		fmt.Fprintf(out, "StartingDeadline:   %ds\n", *s.Spec.StartingDeadlineSeconds)
	}
	if s.Spec.Retention != nil && s.Spec.Retention.KeepLast != nil {
		fmt.Fprintf(out, "KeepLast:           %d\n", *s.Spec.Retention.KeepLast)
	}
	fmt.Fprintf(out, "Template Guest:     %s\n", s.Spec.Template.Spec.GuestRef.Name)
	fmt.Fprintf(out, "Template Backend:   %s\n", s.Spec.Template.Spec.Backend.Type)
	fmt.Fprintf(out, "Template Memory:    %t\n", s.Spec.Template.Spec.IncludeMemory)
	if s.Status.LastScheduleTime != nil {
		fmt.Fprintf(out, "Last Schedule:      %s\n", s.Status.LastScheduleTime.Time.Format("2006-01-02 15:04:05 MST"))
	}
	if s.Status.LastSuccessfulTime != nil {
		fmt.Fprintf(out, "Last Successful:    %s\n", s.Status.LastSuccessfulTime.Time.Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Fprintf(out, "Active snapshots:   %d\n", len(s.Status.Active))
	for _, a := range s.Status.Active {
		fmt.Fprintf(out, "  - %s\n", a)
	}
	return nil
}

func runScheduleDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	s := &snapshotv1alpha1.SwiftSnapshotSchedule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := c.Delete(context.Background(), s); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("SwiftSnapshotSchedule %s/%s not found", ns, name)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Deleted SwiftSnapshotSchedule %s/%s\n", ns, name)
	return nil
}

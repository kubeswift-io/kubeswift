package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/actions"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// migrationProgressEstimateAnnotation mirrors the key swiftletd-source
// writes during the live send RPC (rust/swiftletd/src/action.rs
// MIGRATION_PROGRESS_ESTIMATE_KEY). Per Phase 3b design doc §5.4 the
// estimate is annotation-only (not mirrored to a CRD status field), so
// swiftctl reads it directly off the source pod when describing an
// in-flight live migration.
const migrationProgressEstimateAnnotation = "kubeswift.io/migration-progress-estimate"

var (
	migrateTargetNode    string
	migrateAllowIPChange bool
	migrateName          string
	migratePreferredMode string
	migrateTimeout       time.Duration
	migrateTTL           time.Duration
	migrateCheck         bool
	migrationListAllNS   bool
)

// migrateCmd is the operator-friendly entry point: `swiftctl migrate
// <guest> --to <node>`. Constructs a SwiftMigration with a generated
// name and applies it. Mirrors `swiftctl restore` shape.
var migrateCmd = &cobra.Command{
	Use:   "migrate <guest>",
	Short: "Move a SwiftGuest to another node by creating a SwiftMigration",
	Long: `Create a SwiftMigration that moves a SwiftGuest to a target node.

--preferred-mode selects the strategy:

  auto    (default) live-migrate when the guest is eligible
          (ReadWriteMany+Block storage or kernel-boot, and no VFIO/
          SR-IOV), otherwise fall back to offline. Read status.mode
          to see which the controller picked.
  live    live migration: memory + device state stream to the target
          while the guest keeps running; sub-3s operator-visible
          downtime. Rejected if the guest is ineligible.
  offline the source guest is fully stopped, its root-disk PVC is
          detached on the source node, and the guest is recreated on
          the target with the same disk content. Downtime ~70s on
          Longhorn full-copy, ~25s on true CoW drivers. The only mode
          for VFIO/SR-IOV guests.

VFIO/SR-IOV guests cannot live-migrate (no release-and-reallocate
primitive yet — Phase 4+ work); use offline. The webhook rejects an
explicit --preferred-mode live for an ineligible guest with a clear
error.

Default node-local-bridge networking does not preserve guest IPs
across nodes. Pass --allow-ip-change to acknowledge and proceed,
or attach the guest to a multi-node network (Multus + macvlan or
OVN-K layer-2) for IP preservation.`,
	Example: `  swiftctl migrate db --to worker-3
  swiftctl migrate db --to worker-3 --check   # preflight only, creates nothing
  swiftctl migrate web --to worker-3 --preferred-mode live --allow-ip-change
  swiftctl migrate db --to worker-3 --preferred-mode offline
  swiftctl migrate db --to worker-3 --name db-rebalance-2026-04-28`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runMigrate,
}

// migrationCmd groups the read/manage subcommands. Pattern matches
// the snapshot/restore command groups.
var migrationCmd = &cobra.Command{
	Use:   "migration",
	Short: "Manage SwiftMigration resources",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var migrationListCmd = &cobra.Command{
	Use:          "list",
	Aliases:      []string{"ls"},
	Short:        "List SwiftMigrations",
	SilenceUsage: true,
	RunE:         runMigrationList,
}

var migrationDescribeCmd = &cobra.Command{
	Use:          "describe [name]",
	Short:        "Print human-readable details of a SwiftMigration",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runMigrationDescribe,
}

var migrationCancelCmd = &cobra.Command{
	Use:          "cancel [name]",
	Short:        "Cancel an in-flight SwiftMigration (deletes the resource; controller runs cleanup)",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runMigrationCancel,
}

func init() {
	migrateCmd.Flags().StringVar(&migrateTargetNode, "to", "", "Target node name (required)")
	migrateCmd.Flags().BoolVar(&migrateCheck, "check", false,
		"Preflight only: report node readiness/capacity, IP-preservation, mode, and CPU compatibility without creating the migration")
	migrateCmd.Flags().BoolVar(&migrateAllowIPChange, "allow-ip-change", false,
		"Acknowledge that the guest IP will change cross-node on default node-local networking")
	migrateCmd.Flags().StringVar(&migrateName, "name", "",
		"Optional SwiftMigration resource name (default: <guest>-migrate-<rand>)")
	migrateCmd.Flags().DurationVar(&migrateTTL, "ttl", 0,
		"Auto-delete this migration record this long after it finishes (e.g. 1h); unset = keep")
	migrateCmd.Flags().DurationVar(&migrateTimeout, "timeout", 0,
		"Runaway backstop for the whole migration (e.g. 10m); 0 uses the CRD default of 30m")
	migrateCmd.Flags().StringVar(&migratePreferredMode, "preferred-mode", "auto",
		"Migration mode: auto (live when eligible, else offline), live, or offline")
	_ = migrateCmd.MarkFlagRequired("to")

	migrationListCmd.Flags().BoolVarP(&migrationListAllNS, "all-namespaces", "A", false, "List across all namespaces")

	migrationCmd.AddCommand(migrationListCmd)
	migrationCmd.AddCommand(migrationDescribeCmd)
	migrationCmd.AddCommand(migrationCancelCmd)
}

func newMigrationClient() (client.Client, error) {
	cfg, err := kubeConfig.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: scheme.Scheme})
}

// parsePreferredMode validates the --preferred-mode flag and maps it to
// the SwiftMigration spec.mode value. The flag is named --preferred-mode
// (design doc Section 6.1) but the CRD field is spec.mode; this is the
// translation point. Empty is treated as auto.
func parsePreferredMode(s string) (migrationv1alpha1.SwiftMigrationMode, error) {
	switch m := migrationv1alpha1.SwiftMigrationMode(s); m {
	case "", migrationv1alpha1.SwiftMigrationModeAuto:
		return migrationv1alpha1.SwiftMigrationModeAuto, nil
	case migrationv1alpha1.SwiftMigrationModeLive, migrationv1alpha1.SwiftMigrationModeOffline:
		return m, nil
	default:
		return "", fmt.Errorf("invalid --preferred-mode %q (must be auto, live, or offline)", s)
	}
}

func runMigrate(cmd *cobra.Command, args []string) error {
	guestName := args[0]
	ns := getNamespace()
	mode, err := parsePreferredMode(migratePreferredMode)
	if err != nil {
		return err
	}
	if migrateCheck {
		// Preflight only — the typed client drives the read-only checks; it
		// creates nothing.
		c, err := newMigrationClient()
		if err != nil {
			return err
		}
		return runMigratePreflight(cmd, c, guestName, ns, mode)
	}

	dyn, err := newDynamicClient()
	if err != nil {
		return err
	}
	// migrateName is "" by default, so actions.Migrate falls back to the
	// "<guest>-migrate-" generateName prefix (the apiserver assigns a unique
	// suffix even if the operator runs `swiftctl migrate` twice in quick
	// succession). --timeout/--ttl of 0 are omitted so the CRD defaults apply
	// (spec.timeout defaults to 30m; spec.ttl unset = keep).
	created, err := actions.Migrate(context.Background(), dyn, actions.MigrateParams{
		Namespace:     ns,
		GuestName:     guestName,
		TargetNode:    migrateTargetNode,
		Mode:          string(mode),
		AllowIPChange: migrateAllowIPChange,
		Name:          migrateName,
		Timeout:       migrateTimeout,
		TTL:           migrateTTL,
	})
	if err != nil {
		return fmt.Errorf("create SwiftMigration: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "swiftmigration.migration.kubeswift.io/%s created\n", created.GetName())
	return nil
}

func runMigrationList(cmd *cobra.Command, _ []string) error {
	c, err := newMigrationClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	var list migrationv1alpha1.SwiftMigrationList
	opts := []client.ListOption{}
	if !migrationListAllNS {
		opts = append(opts, client.InNamespace(getNamespace()))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tGUEST\tFROM\tTO\tMODE\tPHASE\tDOWNTIME\tTRANSFER\tAGE")
	for _, m := range list.Items {
		downtime := "-"
		if m.Status.ObservedDowntime != nil {
			downtime = m.Status.ObservedDowntime.Duration.Truncate(1e9).String()
		}
		transfer := "-"
		if m.Status.ObservedTransferDuration != nil {
			transfer = m.Status.ObservedTransferDuration.Duration.Truncate(1e9).String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Namespace, m.Name, m.Spec.GuestRef.Name,
			emptyDash(m.Status.SourceNode), emptyDash(m.Status.DestinationNode),
			emptyDash(string(m.Status.Mode)), m.Status.Phase, downtime, transfer,
			cliAge(m.CreationTimestamp.Time))
	}
	return w.Flush()
}

func runMigrationDescribe(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newMigrationClient()
	if err != nil {
		return err
	}
	var m migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &m); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("SwiftMigration %s/%s not found", ns, name)
		}
		return err
	}
	// Best-effort: the live-transfer progress estimate lives only on the
	// source pod's annotation (design §5.4, not mirrored to status). Fetch
	// it while a live transfer is in flight so the renderer can surface
	// it. Ignore errors — progress is informational, never load-bearing.
	var srcPod *corev1.Pod
	if isLiveTransferInFlight(&m) {
		var pod corev1.Pod
		if getErr := c.Get(context.Background(), client.ObjectKey{Name: m.Status.SourcePodRef.Name, Namespace: ns}, &pod); getErr == nil {
			srcPod = &pod
		}
	}
	renderMigrationDescribe(cmd.OutOrStdout(), &m, srcPod)
	return nil
}

// isLiveTransferInFlight reports whether a live migration is in its
// memory-transfer phase, where the source pod carries the progress
// estimate. Live mode reuses the StopAndCopy phase constant
// (distinguished by status.mode), so there is no StopAndCopyLive phase
// to match on — gate on phase==StopAndCopy AND mode==live.
func isLiveTransferInFlight(m *migrationv1alpha1.SwiftMigration) bool {
	return m.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseStopAndCopy &&
		m.Status.Mode == migrationv1alpha1.SwiftMigrationModeLive &&
		m.Status.SourcePodRef != nil && m.Status.SourcePodRef.Name != ""
}

// renderMigrationDescribe writes the human-readable describe output.
// Pure (no cluster access) so it is unit-testable: srcPod is the already-
// fetched source pod (nil unless a live transfer is in flight).
func renderMigrationDescribe(out io.Writer, m *migrationv1alpha1.SwiftMigration, srcPod *corev1.Pod) {
	fmt.Fprintf(out, "Name:           %s\n", m.Name)
	fmt.Fprintf(out, "Namespace:      %s\n", m.Namespace)
	fmt.Fprintf(out, "Guest:          %s\n", m.Spec.GuestRef.Name)
	fmt.Fprintf(out, "Target:         nodeName=%s\n", m.Spec.Target.NodeName)
	fmt.Fprintf(out, "Mode (spec):    %s\n", emptyDash(string(m.Spec.Mode)))
	if m.Spec.AllowIPChange {
		fmt.Fprintf(out, "AllowIPChange:  true\n")
	}
	fmt.Fprintf(out, "Phase:          %s\n", m.Status.Phase)
	if m.Status.PhaseDetail != "" {
		fmt.Fprintf(out, "PhaseDetail:    %s\n", m.Status.PhaseDetail)
	}
	if m.Status.Mode != "" {
		fmt.Fprintf(out, "Mode (resolved): %s\n", m.Status.Mode)
	}
	if m.Status.SourceNode != "" {
		fmt.Fprintf(out, "Source Node:    %s\n", m.Status.SourceNode)
	}
	if m.Status.DestinationNode != "" {
		fmt.Fprintf(out, "Dest Node:      %s\n", m.Status.DestinationNode)
	}
	if m.Status.StartedAt != nil {
		fmt.Fprintf(out, "Started:        %s\n", m.Status.StartedAt.Format("2006-01-02 15:04:05Z"))
	}
	if m.Status.CompletedAt != nil {
		fmt.Fprintf(out, "Completed:      %s\n", m.Status.CompletedAt.Format("2006-01-02 15:04:05Z"))
	}
	if m.Status.ObservedDowntime != nil {
		fmt.Fprintf(out, "Downtime:       %s\n", m.Status.ObservedDowntime.Duration.Truncate(1e9))
	}
	// Transfer duration: the full vm.send-migration RPC window (live mode
	// only — offline leaves it nil).
	if m.Status.ObservedTransferDuration != nil {
		fmt.Fprintf(out, "Transfer:       %s\n", m.Status.ObservedTransferDuration.Duration.Truncate(1e9))
	}
	// Applied downtime ceiling (CH downtime_ms; live mode, when
	// spec.downtimeTarget was set). A BOUND on the vCPU-stop window, not a
	// measurement — CH v52 does not report the achieved value.
	if m.Status.AppliedDowntimeMs != nil {
		fmt.Fprintf(out, "Downtime cap:   %dms (target; achieved <= this, not measured)\n", *m.Status.AppliedDowntimeMs)
	}
	if m.Status.FailureMessage != "" {
		fmt.Fprintf(out, "Failure:        %s\n", m.Status.FailureMessage)
	}
	if len(m.Status.Conditions) > 0 {
		fmt.Fprintln(out, "Conditions:")
		for _, c := range m.Status.Conditions {
			fmt.Fprintf(out, "  %s=%s reason=%s msg=%s\n",
				c.Type, c.Status, c.Reason, c.Message)
		}
	}
	// Live-transfer progress estimate (best-effort, heuristic).
	if srcPod != nil {
		if pct := srcPod.Annotations[migrationProgressEstimateAnnotation]; pct != "" {
			fmt.Fprintf(out, "Progress (estimate): %s%%\n", pct)
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "  Note: Progress is a heuristic based on ~108 MB/s baseline pod-network")
			fmt.Fprintln(out, "  bandwidth (spike Q4); actual rate depends on the workload's memory")
			fmt.Fprintln(out, "  dirty rate. The guest stays responsive throughout the transfer.")
		}
	}
	// Completed live migration: gloss the two metrics, which mean
	// different things. Offline leaves ObservedTransferDuration nil, so
	// this only fires for live (offline downtime is self-explanatory).
	if m.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseCompleted && m.Status.ObservedTransferDuration != nil {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Downtime is the operator-visible cluster downtime window (cutover")
		fmt.Fprintln(out, "dispatch -> guest healthy on destination). Transfer is the full")
		fmt.Fprintln(out, "vm.send-migration RPC (pre-copy + stop-and-copy + finalize); the")
		fmt.Fprintln(out, "guest stays responsive throughout most of that window.")
	}
	// UX hint: Resuming is boot-bound, not stuck. Operators reading
	// this output during the boot window should see the explanation
	// inline rather than reach for kubectl describe.
	if m.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseResuming {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Note: Resuming waits for the guest VM to boot on the destination")
		fmt.Fprintln(out, "node (~17s on a warm cache). The controller is not stuck.")
	}
}

func runMigrationCancel(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newMigrationClient()
	if err != nil {
		return err
	}
	var m migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &m); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("SwiftMigration %s/%s not found", ns, name)
		}
		return err
	}
	if err := c.Delete(context.Background(), &m); err != nil {
		return fmt.Errorf("delete SwiftMigration: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"swiftmigration.migration.kubeswift.io/%s deletion requested; controller will run cleanup before the resource is GC'd\n",
		m.Name)
	return nil
}

// emptyDash returns "-" for empty strings — used in the list output
// so empty status fields render as "-" instead of blank columns.
func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

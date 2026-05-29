package main

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

var (
	migrateTargetNode    string
	migrateAllowIPChange bool
	migrateName          string
	migratePreferredMode string
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
	migrateCmd.Flags().BoolVar(&migrateAllowIPChange, "allow-ip-change", false,
		"Acknowledge that the guest IP will change cross-node on default node-local networking")
	migrateCmd.Flags().StringVar(&migrateName, "name", "",
		"Optional SwiftMigration resource name (default: <guest>-migrate-<rand>)")
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
	c, err := newMigrationClient()
	if err != nil {
		return err
	}
	name := migrateName
	if name == "" {
		// Use the apiserver's GenerateName so the resource gets a
		// unique suffix even if the operator runs `swiftctl migrate`
		// twice in quick succession.
		name = ""
	}
	mig := &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
		},
		Spec: migrationv1alpha1.SwiftMigrationSpec{
			GuestRef:      migrationv1alpha1.SwiftMigrationGuestRef{Name: guestName},
			Target:        migrationv1alpha1.SwiftMigrationTarget{NodeName: migrateTargetNode},
			Mode:          mode,
			AllowIPChange: migrateAllowIPChange,
		},
	}
	if name == "" {
		mig.GenerateName = guestName + "-migrate-"
	} else {
		mig.Name = name
	}
	if err := c.Create(context.Background(), mig); err != nil {
		return fmt.Errorf("create SwiftMigration: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "swiftmigration.migration.kubeswift.io/%s created\n", mig.Name)
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
	fmt.Fprintln(w, "NAMESPACE\tNAME\tGUEST\tFROM\tTO\tMODE\tPHASE\tDOWNTIME\tAGE")
	for _, m := range list.Items {
		downtime := "-"
		if m.Status.ObservedDowntime != nil {
			downtime = m.Status.ObservedDowntime.Duration.Truncate(1e9).String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Namespace, m.Name, m.Spec.GuestRef.Name,
			emptyDash(m.Status.SourceNode), emptyDash(m.Status.DestinationNode),
			emptyDash(string(m.Status.Mode)), m.Status.Phase, downtime,
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
	out := cmd.OutOrStdout()
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
	// UX hint: Resuming is boot-bound, not stuck. Operators reading
	// this output during the boot window should see the explanation
	// inline rather than reach for kubectl describe.
	if m.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseResuming {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Note: Resuming waits for the guest VM to boot on the destination")
		fmt.Fprintln(out, "node (~17s on a warm cache). The controller is not stuck.")
	}
	return nil
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

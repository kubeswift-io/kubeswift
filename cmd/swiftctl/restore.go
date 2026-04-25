package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

var (
	restoreSnapshot  string
	restoreTarget    string
	restoreOverwrite bool
	restoreNoResume  bool
	restoreListAllNS bool
)

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Manage SwiftRestore resources",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var restoreCreateCmd = &cobra.Command{
	Use:          "create [name]",
	Short:        "Restore a SwiftSnapshot into a new SwiftGuest",
	SilenceUsage: true,
	Long: `Create a SwiftRestore that materializes a SwiftSnapshot as a new SwiftGuest.
The new guest's root-disk PVC is sourced from the snapshot's VolumeSnapshot;
the spec is copied from the source guest. The source SwiftGuest must still
exist (the controller reads its spec).`,
	Example: `  swiftctl restore create r1 --snapshot db-2026-04-25 --target db-restored
  swiftctl restore create r1 --snapshot s1 --target g1 --no-resume`,
	Args: cobra.ExactArgs(1),
	RunE: runRestoreCreate,
}

var restoreListCmd = &cobra.Command{
	Use:          "list",
	Aliases:      []string{"ls"},
	Short:        "List SwiftRestores",
	SilenceUsage: true,
	RunE:         runRestoreList,
}

var restoreDescribeCmd = &cobra.Command{
	Use:          "describe [name]",
	Short:        "Print human-readable details of a SwiftRestore",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runRestoreDescribe,
}

var restoreDeleteCmd = &cobra.Command{
	Use:          "delete [name]",
	Short:        "Delete a SwiftRestore",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runRestoreDelete,
}

func init() {
	restoreCreateCmd.Flags().StringVar(&restoreSnapshot, "snapshot", "", "SwiftSnapshot to restore from (required)")
	restoreCreateCmd.Flags().StringVar(&restoreTarget, "target", "", "Name of the resulting SwiftGuest (required)")
	restoreCreateCmd.Flags().BoolVar(&restoreOverwrite, "overwrite-existing", false, "Replace an existing SwiftGuest with the same target name")
	restoreCreateCmd.Flags().BoolVar(&restoreNoResume, "no-resume", false, "Leave the restored SwiftGuest in runPolicy=Stopped")
	_ = restoreCreateCmd.MarkFlagRequired("snapshot")
	_ = restoreCreateCmd.MarkFlagRequired("target")

	restoreListCmd.Flags().BoolVarP(&restoreListAllNS, "all-namespaces", "A", false, "List across all namespaces")

	restoreCmd.AddCommand(restoreCreateCmd)
	restoreCmd.AddCommand(restoreListCmd)
	restoreCmd.AddCommand(restoreDescribeCmd)
	restoreCmd.AddCommand(restoreDeleteCmd)
}

func runRestoreCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	r := &snapshotv1alpha1.SwiftRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftRestoreSpec{
			SnapshotRef: snapshotv1alpha1.SwiftRestoreSnapshotRef{Name: restoreSnapshot},
			TargetGuest: snapshotv1alpha1.SwiftRestoreTarget{
				Name:              restoreTarget,
				OverwriteExisting: restoreOverwrite,
			},
			ResumeAfterRestore: !restoreNoResume,
		},
	}
	if err := c.Create(context.Background(), r); err != nil {
		return fmt.Errorf("create SwiftRestore: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created SwiftRestore %s/%s (snapshot=%s -> target=%s)\n",
		ns, name, restoreSnapshot, restoreTarget)
	return nil
}

func runRestoreList(cmd *cobra.Command, _ []string) error {
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	var list snapshotv1alpha1.SwiftRestoreList
	opts := []client.ListOption{}
	if !restoreListAllNS {
		opts = append(opts, client.InNamespace(getNamespace()))
	}
	if err := c.List(ctx, &list, opts...); err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tSNAPSHOT\tTARGET\tPHASE\tAGE")
	for _, r := range list.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Namespace, r.Name, r.Spec.SnapshotRef.Name, r.Spec.TargetGuest.Name,
			r.Status.Phase, cliAge(r.CreationTimestamp.Time))
	}
	return w.Flush()
}

func runRestoreDescribe(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	var r snapshotv1alpha1.SwiftRestore
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &r); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("SwiftRestore %s/%s not found", ns, name)
		}
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Name:        %s\n", r.Name)
	fmt.Fprintf(out, "Namespace:   %s\n", r.Namespace)
	fmt.Fprintf(out, "Snapshot:    %s\n", r.Spec.SnapshotRef.Name)
	fmt.Fprintf(out, "Target:      %s\n", r.Spec.TargetGuest.Name)
	fmt.Fprintf(out, "Overwrite:   %t\n", r.Spec.TargetGuest.OverwriteExisting)
	fmt.Fprintf(out, "Resume:      %t\n", r.Spec.ResumeAfterRestore)
	fmt.Fprintf(out, "Phase:       %s\n", r.Status.Phase)
	if r.Status.GuestRef != nil {
		fmt.Fprintf(out, "GuestRef:    %s\n", r.Status.GuestRef.Name)
	}
	if r.Status.StartedAt != nil {
		fmt.Fprintf(out, "StartedAt:   %s\n", r.Status.StartedAt.Time.Format(time.RFC3339))
	}
	if r.Status.CompletedAt != nil {
		fmt.Fprintf(out, "CompletedAt: %s\n", r.Status.CompletedAt.Time.Format(time.RFC3339))
	}
	if len(r.Status.Conditions) > 0 {
		fmt.Fprintln(out, "Conditions:")
		for _, c := range r.Status.Conditions {
			fmt.Fprintf(out, "  %s=%s reason=%s message=%q\n", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	return nil
}

func runRestoreDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	r := &snapshotv1alpha1.SwiftRestore{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := c.Delete(context.Background(), r); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("SwiftRestore %s/%s not found", ns, name)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Deleted SwiftRestore %s/%s\n", ns, name)
	return nil
}

// cliAge returns a short human-readable age string ("3d", "5h", "12m", "now").
func cliAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

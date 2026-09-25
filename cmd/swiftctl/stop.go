package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kubeswift-io/kubeswift/internal/actions"
)

var stopCmd = &cobra.Command{
	Use:          "stop [guest-name]",
	Short:        "Stop a SwiftGuest",
	SilenceUsage: true,
	Long: `Stop a SwiftGuest by setting spec.runPolicy=Stopped and deleting the launcher pod.

Deleting the launcher pod triggers swiftletd's graceful SIGTERM shutdown (an
ACPI power-off) within the pod's termination grace period, and the controller
does not recreate it while runPolicy is Stopped.

Setting runPolicy=Stopped any other way (kubectl, GitOps) stops a running
guest too: the controller deletes its launcher pod the same way. It waits
until no migration, restore or snapshot capture of the guest is in flight,
and reports the wait as a StopDeferred event on the guest. This command does
not wait; it deletes the pod at once.`,
	Example: `  swiftctl stop sample
  swiftctl -n myns stop my-guest`,
	Args: cobra.ExactArgs(1),
	RunE: runStop,
}

func runStop(cmd *cobra.Command, args []string) error {
	guestName := args[0]
	ns := getNamespace()

	dyn, err := newDynamicClient()
	if err != nil {
		return err
	}

	if _, err := actions.Stop(context.Background(), dyn, ns, guestName); err != nil {
		return fmt.Errorf("failed to stop guest: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s/%s\n", ns, guestName)
	return nil
}

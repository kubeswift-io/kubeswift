package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/projectbeskar/kubeswift/internal/actions"
)

var stopCmd = &cobra.Command{
	Use:          "stop [guest-name]",
	Short:        "Stop a SwiftGuest",
	SilenceUsage: true,
	Long: `Stop a SwiftGuest by setting spec.runPolicy=Stopped and deleting the launcher pod.

Both steps are required: the SwiftGuest stop guard is reactive — it prevents
the pod from being recreated, it does not stop a running VM — so a runPolicy
patch alone leaves the guest running. Deleting the launcher pod triggers
swiftletd's graceful SIGTERM shutdown within the pod's termination grace
period; the guard then keeps it stopped.`,
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

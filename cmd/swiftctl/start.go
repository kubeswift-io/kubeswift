package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kubeswift-io/kubeswift/internal/actions"
)

var startCmd = &cobra.Command{
	Use:          "start [guest-name]",
	Short:        "Start a SwiftGuest",
	SilenceUsage: true,
	Long: `Start a SwiftGuest by setting spec.runPolicy=Running.
The controller creates a new launcher pod; swiftletd launches the VM.
If the guest's launcher has exited (the guest was shut down from inside, or
its launcher failed), start removes that pod so the controller creates a new
one. A running guest is left as it is: use restart to recreate its pod.`,
	Example: `  swiftctl start sample
  swiftctl -n myns start my-guest`,
	Args: cobra.ExactArgs(1),
	RunE: runStart,
}

func runStart(cmd *cobra.Command, args []string) error {
	guestName := args[0]
	ns := getNamespace()

	dyn, err := newDynamicClient()
	if err != nil {
		return err
	}

	// Start patches runPolicy=Running and removes an exited launcher pod; the
	// controller creates a new one. (To recreate the pod of an already-running
	// guest, use `restart`.)
	if _, err := actions.Start(context.Background(), dyn, ns, guestName); err != nil {
		return fmt.Errorf("failed to start guest: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Started %s/%s (runPolicy=Running; controller will recreate the pod)\n", ns, guestName)
	return nil
}

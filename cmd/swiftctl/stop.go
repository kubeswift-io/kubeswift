package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/cli"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

var stopCmd = &cobra.Command{
	Use:          "stop [guest-name]",
	Short:        "Stop a SwiftGuest",
	SilenceUsage: true,
	Long: `Stop a SwiftGuest by setting spec.runPolicy=Stopped and deleting the pod.
The controller will create a new pod with lifecycle=stop; swiftletd will exit without launching.`,
	Example: `  swiftctl stop sample
  swiftctl -n myns stop my-guest`,
	Args: cobra.ExactArgs(1),
	RunE: runStop,
}

func runStop(cmd *cobra.Command, args []string) error {
	guestName := args[0]
	ns := getNamespace()

	config, err := kubeConfig.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}

	c, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	resolver := &cli.GuestResolver{Client: c}
	ctx := context.Background()

	guest, err := resolver.ResolveGuest(ctx, ns, guestName)
	if err != nil {
		return err
	}

	// Patch runPolicy to Stopped
	if err := resolver.PatchRunPolicy(ctx, guest, swiftv1alpha1.RunPolicyStopped); err != nil {
		return fmt.Errorf("failed to stop guest: %w", err)
	}

	// Delete pod if it exists so controller recreates with lifecycle=stop
	pod, err := resolver.ResolvePod(ctx, guest)
	if err != nil {
		// Pod might not exist (e.g. already stopped)
		fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s/%s (runPolicy=Stopped)\n", ns, guestName)
		return nil
	}

	if err := resolver.DeletePod(ctx, pod); err != nil {
		return fmt.Errorf("failed to delete pod: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s/%s (pod deleted, controller will recreate with lifecycle=stop)\n", ns, guestName)
	return nil
}

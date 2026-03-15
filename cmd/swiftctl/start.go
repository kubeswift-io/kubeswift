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

var startCmd = &cobra.Command{
	Use:          "start [guest-name]",
	Short:        "Start a SwiftGuest",
	SilenceUsage: true,
	Long: `Start a SwiftGuest by setting spec.runPolicy=Running and recreating the pod.
The controller will create a new pod with lifecycle=start; swiftletd will launch the VM.`,
	Example: `  swiftctl start sample
  swiftctl -n myns start my-guest`,
	Args: cobra.ExactArgs(1),
	RunE: runStart,
}

func runStart(cmd *cobra.Command, args []string) error {
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

	// Patch runPolicy to Running
	if err := resolver.PatchRunPolicy(ctx, guest, swiftv1alpha1.RunPolicyRunning); err != nil {
		return fmt.Errorf("failed to start guest: %w", err)
	}

	// Delete pod if it exists so controller recreates with lifecycle=start
	pod, err := resolver.ResolvePod(ctx, guest)
	if err == nil {
		if err := resolver.DeletePod(ctx, pod); err != nil {
			return fmt.Errorf("failed to delete pod: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Started %s/%s (pod deleted, controller will recreate)\n", ns, guestName)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Started %s/%s (runPolicy=Running)\n", ns, guestName)
	}

	return nil
}

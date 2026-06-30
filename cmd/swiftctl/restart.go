package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/cli"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

var restartCmd = &cobra.Command{
	Use:          "restart [guest-name]",
	Short:        "Restart a SwiftGuest",
	SilenceUsage: true,
	Long: `Restart a SwiftGuest by deleting its pod.
The controller will recreate the pod; swiftletd will launch the VM again.
Requires spec.runPolicy=Running.`,
	Example: `  swiftctl restart sample
  swiftctl -n myns restart my-guest`,
	Args: cobra.ExactArgs(1),
	RunE: runRestart,
}

func runRestart(cmd *cobra.Command, args []string) error {
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

	info, err := resolver.Resolve(ctx, ns, guestName)
	if err != nil {
		return err
	}

	if info.Guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyStopped {
		return fmt.Errorf("guest %s/%s has runPolicy=Stopped; use 'swiftctl start' first", ns, guestName)
	}

	if err := resolver.DeletePod(ctx, info.Pod); err != nil {
		return fmt.Errorf("failed to restart guest: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Restarted %s/%s (pod deleted, controller will recreate)\n", ns, guestName)
	return nil
}

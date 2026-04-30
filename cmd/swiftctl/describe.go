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

var describeCmd = &cobra.Command{
	Use:          "describe [guest-name]",
	Short:        "Describe a SwiftGuest",
	SilenceUsage: true,
	Long:         `Output a human-readable summary of a SwiftGuest.`,
	Example: `  swiftctl describe sample
  swiftctl -n myns describe my-guest`,
	Args: cobra.ExactArgs(1),
	RunE: runDescribe,
}

func runDescribe(cmd *cobra.Command, args []string) error {
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

	out := cmd.OutOrStdout()

	// Basic info
	fmt.Fprintf(out, "Name:        %s\n", guest.Name)
	fmt.Fprintf(out, "Namespace:   %s\n", guest.Namespace)
	fmt.Fprintf(out, "Phase:       %s\n", guest.Status.Phase)
	nodeName := guest.Status.NodeName
	if nodeName == "" {
		nodeName = "(none)"
	}
	fmt.Fprintf(out, "Node:        %s\n", nodeName)
	fmt.Fprintf(out, "RunPolicy:   %s\n", orDefault(string(guest.Spec.RunPolicy), "Running"))

	// Spec
	seedProfile := "(none)"
	if guest.Spec.SeedProfileRef != nil && guest.Spec.SeedProfileRef.Name != "" {
		seedProfile = guest.Spec.SeedProfileRef.Name
	}
	imageRef := "(none)"
	if guest.Spec.ImageRef != nil && guest.Spec.ImageRef.Name != "" {
		imageRef = guest.Spec.ImageRef.Name
	}
	kernelRef := "(none)"
	if guest.Spec.KernelRef != nil && guest.Spec.KernelRef.Name != "" {
		kernelRef = guest.Spec.KernelRef.Name
	}
	fmt.Fprintf(out, "\nSpec:\n")
	fmt.Fprintf(out, "  Image:       %s\n", imageRef)
	fmt.Fprintf(out, "  Kernel:      %s\n", kernelRef)
	fmt.Fprintf(out, "  GuestClass:  %s\n", guest.Spec.GuestClassRef.Name)
	fmt.Fprintf(out, "  SeedProfile: %s\n", seedProfile)

	// Runtime
	hypervisor := "(unknown)"
	pidStr := "(unknown)"
	if guest.Status.Runtime != nil {
		if guest.Status.Runtime.Hypervisor != "" {
			hypervisor = guest.Status.Runtime.Hypervisor
		}
		if guest.Status.Runtime.PID != 0 {
			pidStr = fmt.Sprintf("%d", guest.Status.Runtime.PID)
		}
	}
	fmt.Fprintf(out, "\nRuntime:\n")
	fmt.Fprintf(out, "  Hypervisor:  %s\n", hypervisor)
	fmt.Fprintf(out, "  PID:         %s\n", pidStr)

	// Console
	serialSocket := "(none)"
	if guest.Status.Console != nil && guest.Status.Console.SerialSocket != "" {
		serialSocket = guest.Status.Console.SerialSocket
	}
	fmt.Fprintf(out, "\nConsole:\n")
	fmt.Fprintf(out, "  SerialSocket: %s\n", serialSocket)

	// Network
	primaryIP := "(none)"
	if guest.Status.Network != nil && guest.Status.Network.PrimaryIP != "" {
		primaryIP = guest.Status.Network.PrimaryIP
	}
	fmt.Fprintf(out, "\nNetwork:\n")
	fmt.Fprintf(out, "  PrimaryIP:   %s\n", primaryIP)
	fmt.Fprintf(out, "  Interfaces:\n")
	if guest.Status.Network != nil && len(guest.Status.Network.Interfaces) > 0 {
		for _, iface := range guest.Status.Network.Interfaces {
			fmt.Fprintf(out, "    - %s: %s\n", iface.Name, iface.IP)
		}
	}

	// Storage
	fmt.Fprintf(out, "\nStorage:\n")
	if guest.Status.Storage != nil {
		access := string(guest.Status.Storage.AccessMode)
		volume := string(guest.Status.Storage.VolumeMode)
		sc := guest.Status.Storage.StorageClassName
		if sc == "" {
			sc = "(inherited from source SwiftImage's PVC)"
		}
		fmt.Fprintf(out, "  AccessMode:           %s\n", access)
		fmt.Fprintf(out, "  VolumeMode:           %s\n", volume)
		fmt.Fprintf(out, "  StorageClass:         %s\n", sc)
		// liveMigrationCapable is recomputed (not stored in status) to
		// avoid the controller-write-back race during cluster restore.
		// The webhook recomputes the same way at admission time.
		fmt.Fprintf(out, "  LiveMigrationCapable: %t\n", swiftv1alpha1.IsLiveMigrationCapable(guest.Status.Storage))
	} else {
		fmt.Fprintf(out, "  (not yet resolved)\n")
	}

	// Conditions
	fmt.Fprintf(out, "\nConditions:\n")
	for _, c := range guest.Status.Conditions {
		fmt.Fprintf(out, "  %s: %s — %s\n", c.Type, c.Status, c.Message)
	}

	// Pod
	podName := "(none)"
	podNamespace := "(none)"
	if guest.Status.PodRef != nil {
		if guest.Status.PodRef.Name != "" {
			podName = guest.Status.PodRef.Name
		}
		if guest.Status.PodRef.Namespace != "" {
			podNamespace = guest.Status.PodRef.Namespace
		}
	}
	fmt.Fprintf(out, "\nPod:\n")
	fmt.Fprintf(out, "  Name:      %s\n", podName)
	fmt.Fprintf(out, "  Namespace: %s\n", podNamespace)

	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

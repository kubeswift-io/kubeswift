package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"

	"github.com/kubeswift-io/kubeswift/internal/cli"
	"github.com/kubeswift-io/kubeswift/internal/version"
)

func init() {
	rootCmd.Version = fmt.Sprintf("%s (git %s)", version.Version, version.GitCommit)
}

var (
	kubeConfig cli.KubeConfig
	namespace  string
)

var rootCmd = &cobra.Command{
	Use:   "swiftctl",
	Short: "KubeSwift operator CLI for SwiftGuest operability",
	Long: `swiftctl is the canonical CLI for managing SwiftGuest resources.
It provides lifecycle commands (start, stop, restart) and console access.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "Kubernetes namespace (default: default)")
	rootCmd.PersistentFlags().StringVar(&kubeConfig.Kubeconfig, "kubeconfig", "", "Path to kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	rootCmd.PersistentFlags().StringVar(&kubeConfig.Context, "context", "", "Kubernetes context")
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(restartCmd)
	rootCmd.AddCommand(describeCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(consoleCmd)
	rootCmd.AddCommand(sshCmd)
	rootCmd.AddCommand(debugCmd)
	rootCmd.AddCommand(snapshotCmd)
	rootCmd.AddCommand(restoreCmd)
	rootCmd.AddCommand(scheduleCmd)
	rootCmd.AddCommand(migrateCmd)
	rootCmd.AddCommand(migrationCmd)
	rootCmd.AddCommand(guestCmd)
}

// newDynamicClient builds a dynamic client from the resolved kubeconfig. The
// shared lifecycle actions (start/stop/migrate, internal/actions) operate over a
// dynamic.Interface — the same model the gateway uses — so swiftctl builds one
// here instead of a typed controller-runtime client for those commands.
func newDynamicClient() (dynamic.Interface, error) {
	cfg, err := kubeConfig.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return dynamic.NewForConfig(cfg)
}

func getNamespace() string {
	if namespace != "" {
		return namespace
	}
	// Default to "default" when not set
	return "default"
}

func exitErr(err error) {
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}

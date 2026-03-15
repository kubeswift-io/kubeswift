package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/projectbeskar/kubeswift/internal/cli"
	"github.com/projectbeskar/kubeswift/internal/version"
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
	rootCmd.AddCommand(consoleCmd)
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

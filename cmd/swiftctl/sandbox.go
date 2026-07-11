package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kubeswift-io/kubeswift/internal/cli"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var sandboxCmd = &cobra.Command{
	Use:          "sandbox",
	Short:        "Manage SwiftSandbox microVMs",
	SilenceUsage: true,
	RunE:         func(cmd *cobra.Command, args []string) error { return cmd.Help() },
}

var sandboxLogsFollow bool

var sandboxLogsCmd = &cobra.Command{
	Use:   "logs [sandbox-name]",
	Short: "Stream a sandbox workload's console output",
	Long: `Streams a SwiftSandbox guest console — the workload's stdout/stderr, captured to a
host file by swiftletd — by exec-ing into the launcher pod. Use -f to follow.`,
	Example: `  swiftctl sandbox logs my-job
  swiftctl -n ci sandbox logs my-job -f`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runSandboxLogs,
}

func init() {
	sandboxLogsCmd.Flags().BoolVarP(&sandboxLogsFollow, "follow", "f", false, "Follow the log output")
	sandboxCmd.AddCommand(sandboxLogsCmd)
}

func runSandboxLogs(cmd *cobra.Command, args []string) error {
	name := args[0]
	ns := getNamespace()

	config, err := kubeConfig.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	// The sandbox launcher pod is named after the sandbox; the guest console is
	// captured to <run>/serial.sock.log (swiftletd's --serial file= for sandboxes).
	serialLog := "/var/lib/kubeswift/run/" + cli.GuestID(ns, name) + "/serial.sock.log"
	shellCmd := "cat " + serialLog
	if sandboxLogsFollow {
		// tail from the start, then follow; keep waiting even if the file appears late.
		shellCmd = "tail -n +1 -F " + serialLog
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(ns).
		Name(name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: cli.LauncherContainer,
			Command:   []string{"sh", "-c", shellCmd},
			Stdout:    true,
			Stderr:    true,
		}, clientgoscheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if err := exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}); err != nil {
		return fmt.Errorf("stream sandbox logs (is %s/%s running?): %w", ns, name, err)
	}
	return nil
}

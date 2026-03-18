package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/cli"
	"github.com/projectbeskar/kubeswift/internal/scheme"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

	if err := resolver.PatchRunPolicy(ctx, guest, swiftv1alpha1.RunPolicyStopped); err != nil {
		return fmt.Errorf("failed to stop guest: %w", err)
	}

	if guest.Status.Phase != swiftv1alpha1.SwiftGuestPhaseRunning {
		fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s/%s\n", ns, guestName)
		return nil
	}

	pod, err := resolver.ResolvePod(ctx, guest)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s/%s\n", ns, guestName)
		return nil
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	pid := int64(0)
	if guest.Status.Runtime != nil {
		pid = guest.Status.Runtime.PID
	}

	if pid != 0 {
		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(pod.Namespace).
			Name(pod.Name).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: cli.LauncherContainer,
				Command:   []string{"kill", "-TERM", fmt.Sprintf("%d", pid)},
				Stdin:     false,
				Stdout:    true,
				Stderr:    true,
				TTY:       false,
			}, clientgoscheme.ParameterCodec)

		executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			return fmt.Errorf("failed to create executor: %w", err)
		}

		_ = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: cmd.OutOrStdout(),
			Stderr: cmd.ErrOrStderr(),
		})
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, err := clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s/%s\n", ns, guestName)
				return nil
			}
			return fmt.Errorf("failed to check pod: %w", err)
		}
		time.Sleep(2 * time.Second)
	}

	fmt.Fprintln(cmd.ErrOrStderr(), "graceful shutdown timed out, forcing pod deletion")
	if err := clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete pod: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s/%s\n", ns, guestName)
	return nil
}

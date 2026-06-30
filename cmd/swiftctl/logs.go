package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubeswift-io/kubeswift/internal/cli"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

var (
	followFlag bool
	tailLines  int
)

var logsCmd = &cobra.Command{
	Use:          "logs [guest-name]",
	Short:        "Stream launcher pod logs",
	SilenceUsage: true,
	Long:         `Stream logs from the launcher pod of a SwiftGuest.`,
	Example: `  swiftctl logs sample
  swiftctl logs -f sample
  swiftctl logs --tail 100 sample`,
	Args: cobra.ExactArgs(1),
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().BoolVarP(&followFlag, "follow", "f", false, "stream logs")
	logsCmd.Flags().IntVar(&tailLines, "tail", 50, "number of lines to show from end")
}

func runLogs(cmd *cobra.Command, args []string) error {
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

	pod, err := resolver.ResolvePod(ctx, guest)
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	tailLines64 := int64(tailLines)
	req := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: cli.LauncherContainer,
		Follow:    followFlag,
		TailLines: &tailLines64,
	})

	streamCtx := ctx
	if followFlag {
		var cancel context.CancelFunc
		streamCtx, cancel = context.WithCancel(ctx)
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
		}()
	}

	stream, err := req.Stream(streamCtx)
	if err != nil {
		return fmt.Errorf("get logs: %w", err)
	}
	defer stream.Close()

	_, err = io.Copy(os.Stdout, stream)
	if err != nil && streamCtx.Err() != nil {
		return nil // user interrupted
	}
	return err
}

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/projectbeskar/kubeswift/internal/cli"
	"github.com/projectbeskar/kubeswift/internal/scheme"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	sshUser     string
	sshIdentity string
)

var sshCmd = &cobra.Command{
	Use:          "ssh [guest-name]",
	Short:        "SSH into the guest VM",
	SilenceUsage: true,
	Long: `SSH into the guest VM using status.network.primaryIP.
Execs into the launcher pod and runs ssh to the guest. Requires the guest to be Running
and status.network.primaryIP to be populated.`,
	Example: `  swiftctl ssh sample
  swiftctl ssh -u ubuntu -i ~/.ssh/mykey sample`,
	Args: cobra.ExactArgs(1),
	RunE: runSSH,
}

func init() {
	sshCmd.Flags().StringVarP(&sshUser, "user", "u", "kubeswift", "SSH username")
	sshCmd.Flags().StringVarP(&sshIdentity, "identity", "i", "~/.ssh/id_rsa", "Path to SSH private key")
}

func expandPath(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

func runSSH(cmd *cobra.Command, args []string) error {
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

	if guest.Status.Phase != "Running" {
		return fmt.Errorf("guest %s/%s is not Running (phase: %s)", ns, guestName, guest.Status.Phase)
	}

	if guest.Status.Network == nil || guest.Status.Network.PrimaryIP == "" {
		return fmt.Errorf("guest %s/%s has no primaryIP (status.network.primaryIP not set)", ns, guestName)
	}

	primaryIP := guest.Status.Network.PrimaryIP

	pod, err := resolver.ResolvePod(ctx, guest)
	if err != nil {
		return err
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("stdin is not a terminal; run swiftctl ssh from an interactive terminal (e.g. not piped)")
	}

	identityPath, err := expandPath(sshIdentity)
	if err != nil {
		return err
	}

	keyData, err := os.ReadFile(identityPath)
	if err != nil {
		return fmt.Errorf("read identity %q: %w", identityPath, err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	sshCmdStr := fmt.Sprintf(`KEY=$(mktemp) && chmod 600 "$KEY" && cat > "$KEY" << 'KUBESWIFT_EOF'
%s
KUBESWIFT_EOF
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$KEY" %s@%s; rm -f "$KEY"`,
		string(keyData), sshUser, primaryIP)

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: cli.LauncherContainer,
			Command:   []string{"sh", "-c", sshCmdStr},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, clientgoscheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %w", err)
	}

	var restore func()
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		state, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("terminal raw mode: %w", err)
		}
		restore = func() { _ = term.Restore(fd, state) }
		defer restore()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	var sizeQueue remotecommand.TerminalSizeQueue
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		if w, h, err := term.GetSize(fd); err == nil {
			sizeQueue = &fixedSizeQueue{size: &remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}}
		}
	}

	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
		Tty:               true,
		TerminalSizeQueue: sizeQueue,
	})
	if streamErr != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("failed to attach via SSH: %w", streamErr)
	}

	return nil
}

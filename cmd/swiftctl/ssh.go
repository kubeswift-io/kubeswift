package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubeswift-io/kubeswift/internal/cli"
	"github.com/kubeswift-io/kubeswift/internal/scheme"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// execStream runs one command in a pod container and wires the given streams to
// it. A nil stream is not requested. Shared by the key-staging exec and the ssh
// exec so both go through the same SPDY setup.
func execStream(
	ctx context.Context,
	config *rest.Config,
	clientset kubernetes.Interface,
	namespace, podName, container string,
	command []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	tty bool,
	sizeQueue remotecommand.TerminalSizeQueue,
) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
			TTY:       tty,
		}, clientgoscheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             stdin,
		Stdout:            stdout,
		Stderr:            stderr,
		Tty:               tty,
		TerminalSizeQueue: sizeQueue,
	})
}

var (
	sshUser     string
	sshIdentity string
)

var sshCmd = &cobra.Command{
	Use:          "ssh [guest-name] [-- command...]",
	Short:        "SSH into the guest VM",
	SilenceUsage: true,
	Long: `SSH into the guest VM using status.network.primaryIP.
Execs into the launcher pod and runs ssh to the guest. Requires the guest to be Running
and status.network.primaryIP to be populated.

With no trailing command, opens an interactive shell (requires a terminal). Pass a
command after "--" to run it non-interactively and return its output, like
"ssh host <command>" or "kubectl exec pod -- <command>".`,
	Example: `  swiftctl ssh sample
  swiftctl ssh -u ubuntu -i ~/.ssh/mykey sample
  swiftctl ssh sample -- 'uptime; cat /tmp/counter'`,
	Args: cobra.MinimumNArgs(1),
	RunE: runSSH,
}

func init() {
	sshCmd.Flags().StringVarP(&sshUser, "user", "u", "kubeswift", "SSH username")
	sshCmd.Flags().StringVarP(&sshIdentity, "identity", "i", "~/.ssh/id_rsa", "Path to SSH private key")
}

// sshExecCommand builds the launcher exec command that runs ssh to the guest.
// Every value is a quoted positional arg, so the untrusted primaryIP (a pod
// annotation) and the user cannot inject shell: $1 keyPath, $2 user, $3 host,
// $4 optional joined remote command. The EXIT trap removes the staged key even
// if ssh is interrupted (no `exec`, so the trap still fires), and ssh's exit
// code is propagated. The private key is never referenced here — only its path.
func sshExecCommand(keyPath, user, host string, remoteArgs []string) []string {
	const script = `trap 'rm -f "$1"' EXIT; ` +
		`ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$1" "$2"@"$3" ${4:+"$4"}; exit $?`
	command := []string{"sh", "-c", script, "swiftctl-ssh", keyPath, user, host}
	if len(remoteArgs) > 0 {
		// ssh concatenates its trailing args with spaces, so joining matches
		// `ssh host a b c`.
		command = append(command, strings.Join(remoteArgs, " "))
	}
	return command
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
	remoteArgs := args[1:]
	// Interactive shell when no command is given; otherwise run the command
	// non-interactively and stream its output (ssh host <command> semantics).
	interactive := len(remoteArgs) == 0
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

	// Only an interactive shell needs a terminal; a "-- command" run is scriptable.
	if interactive && !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("stdin is not a terminal; run swiftctl ssh from an interactive terminal, or pass a command to run non-interactively: swiftctl ssh %s -- <command>", guestName)
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

	// Stage the private key into the launcher over the exec STDIN, never on a
	// command line. The key used to ride a heredoc inside the exec command,
	// which the apiserver records in its audit log's request URI and which shows
	// in /proc/<pid>/cmdline of the sh process for the whole session — readable
	// by anyone with exec ("console") into this privileged pod. Exec stdin
	// content is not logged that way. This first exec reads the key from stdin
	// into a mode-0600 temp file and prints its path; the ssh exec below runs
	// `ssh -i <path>` and removes it on exit.
	var keyPathBuf strings.Builder
	stageCmd := []string{"sh", "-c", `K=$(mktemp) && chmod 600 "$K" && cat > "$K" && printf %s "$K"`}
	if err := execStream(ctx, config, clientset, pod.Namespace, pod.Name, cli.LauncherContainer,
		stageCmd, strings.NewReader(string(keyData)), &keyPathBuf, os.Stderr, false, nil); err != nil {
		return fmt.Errorf("stage ssh key in launcher: %w", err)
	}
	keyPath := strings.TrimSpace(keyPathBuf.String())
	if keyPath == "" || strings.ContainsAny(keyPath, " \t\r\n") {
		return fmt.Errorf("staging ssh key: unexpected key path %q from launcher", keyPath)
	}

	command := sshExecCommand(keyPath, sshUser, primaryIP, remoteArgs)

	var restore func()
	if interactive {
		if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
			state, err := term.MakeRaw(fd)
			if err != nil {
				return fmt.Errorf("terminal raw mode: %w", err)
			}
			restore = func() { _ = term.Restore(fd, state) }
			defer restore()
		}
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
	if interactive {
		if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
			if w, h, err := term.GetSize(fd); err == nil {
				sizeQueue = &fixedSizeQueue{size: &remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}}
			}
		}
	}

	streamErr := execStream(ctx, config, clientset, pod.Namespace, pod.Name, cli.LauncherContainer,
		command, os.Stdin, os.Stdout, os.Stderr, interactive, sizeQueue)
	if streamErr != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("failed to attach via SSH: %w", streamErr)
	}

	return nil
}

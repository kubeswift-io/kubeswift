package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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

// fixedSizeQueue returns the initial terminal size once, then nil.
type fixedSizeQueue struct {
	size *remotecommand.TerminalSize
}

func (f *fixedSizeQueue) Next() *remotecommand.TerminalSize {
	s := f.size
	f.size = nil
	return s
}

var consoleCmd = &cobra.Command{
	Use:          "console [guest-name]",
	Short:        "Attach to VM serial console",
	SilenceUsage: true,
	Long: `Attach to the VM serial console for interactive keyboard access.
Execs into the launcher pod and connects to the serial socket via socat.
Requires the guest to be Running. Use Ctrl+C to exit.`,
	Example: `  swiftctl console sample
  swiftctl -n myns console my-guest`,
	Args: cobra.ExactArgs(1),
	RunE: runConsole,
}

func runConsole(cmd *cobra.Command, args []string) error {
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

	pod, err := resolver.ResolvePod(ctx, guest)
	if err != nil {
		return err
	}

	// Console requires a TTY for interactive use
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("stdin is not a terminal; run swiftctl console from an interactive terminal (e.g. not piped)")
	}

	serialSocket := "/var/lib/kubeswift/run/" + cli.GuestID(ns, guestName) + "/serial.sock"

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	// Wait for socket (up to 15s) then connect. CH creates the socket when the VM starts.
	// If socket never appears: ensure swiftletd image was rebuilt with --serial socket= support.
	// socat: raw mode for serial; crnl can corrupt binary/control chars from guest
	waitAndSocat := fmt.Sprintf("for i in $(seq 1 15); do test -S %q && break; sleep 1; done; test -S %q || { echo 'serial socket not found at %s'; exit 1; }; exec socat -,raw,echo=0 UNIX-CONNECT:%s", serialSocket, serialSocket, serialSocket, serialSocket)

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: cli.LauncherContainer,
			Command:   []string{"sh", "-c", waitAndSocat},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, clientgoscheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to attach to console: %w", err)
	}

	// Put terminal in raw mode for interactive console (like kubectl exec -it).
	// Without this, input is line-buffered and characters don't reach the guest immediately.
	var restore func()
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		state, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("terminal raw mode: %w", err)
		}
		restore = func() { _ = term.Restore(fd, state) }
		defer restore()
	}

	// Handle SIGINT for clean exit
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// TerminalSizeQueue for resize support (optional; nil is ok)
	var sizeQueue remotecommand.TerminalSizeQueue
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		if w, h, err := term.GetSize(fd); err == nil {
			sizeQueue = &fixedSizeQueue{size: &remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}}
		}
	}

	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
		Tty:               true,
		TerminalSizeQueue: sizeQueue,
	})
	if streamErr != nil {
		if ctx.Err() != nil {
			return nil // user interrupted
		}
		return fmt.Errorf("failed to attach to console: %w", streamErr)
	}

	return nil
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/cli"
	"github.com/kubeswift-io/kubeswift/internal/guestagent"
	"github.com/kubeswift-io/kubeswift/internal/scheme"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// agentVsockPort is the AF_VSOCK port the in-guest agent listens on (matches
// cmd/kubeswift-guest-agent DefaultPort and swift-vsock-client).
const agentVsockPort = 1024

// sandboxTargetPod returns the launcher pod that actually runs the sandbox's
// guest. For a warm-pool checkout the guest runs in the CLAIMED SLOT pod, whose
// name differs from the sandbox (status.podRef = <pool>-slot-<x>); for a cold or
// non-pooled sandbox the launcher pod is named after the sandbox. swiftletd keys
// the guest's run dir (serial.sock.log, vsock.sock) on the launcher pod identity,
// so BOTH the exec target pod AND the cli.GuestID run-dir segment must be this
// value — otherwise logs/exec/attach target the wrong (or non-existent) pod for a
// checked-out sandbox. Falls back to name when status.podRef is unset (early
// Pending) — same as the pre-checkout behavior.
func sandboxTargetPod(ns, name string) (string, error) {
	cfg, err := kubeConfig.ToRESTConfig()
	if err != nil {
		return "", fmt.Errorf("kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return "", fmt.Errorf("create client: %w", err)
	}
	var sb sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sb); err != nil {
		return "", fmt.Errorf("get swiftsandbox %s/%s: %w", ns, name, err)
	}
	if sb.Status.PodRef != "" {
		return sb.Status.PodRef, nil
	}
	return name, nil
}

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

var sandboxExecCmd = &cobra.Command{
	Use:   "exec [sandbox-name] -- command [args...]",
	Short: "Run a command inside a running sandbox (over vsock)",
	Long: `Runs a command inside a running SwiftSandbox via the in-guest agent over vsock. The
command runs in the sandbox's OCI root filesystem. stdout and stderr stream back LIVE
and the command's exit code is propagated. Use -i to forward stdin and -t for an
interactive TTY (e.g. -it -- /bin/sh); "sandbox attach" is shorthand for exec -it.`,
	Example: `  swiftctl sandbox exec my-job -- ls -la /
  swiftctl sandbox exec -i my-job -- sh   < script.sh
  swiftctl sandbox exec -it my-job -- /bin/sh`,
	Args:         cobra.MinimumNArgs(2),
	SilenceUsage: true,
	RunE:         runSandboxExec,
}

var sandboxAttachCmd = &cobra.Command{
	Use:   "attach [sandbox-name] [-- command [args...]]",
	Short: "Open an interactive shell inside a running sandbox (over vsock)",
	Long: `Attaches an interactive TTY to a running SwiftSandbox — shorthand for
"sandbox exec -it". Defaults to /bin/sh; pass -- <command> to run something else.
Exit the shell (Ctrl-D or 'exit') to detach.`,
	Example: `  swiftctl sandbox attach my-job
  swiftctl -n ci sandbox attach my-job -- /bin/bash`,
	Args:         cobra.MinimumNArgs(1),
	SilenceUsage: true,
	RunE:         runSandboxAttach,
}

var (
	sandboxExecEnv     []string
	sandboxExecWorkdir string
	sandboxExecStdin   bool
	sandboxExecTTY     bool
)

func init() {
	sandboxLogsCmd.Flags().BoolVarP(&sandboxLogsFollow, "follow", "f", false, "Follow the log output")
	sandboxExecCmd.Flags().StringArrayVarP(&sandboxExecEnv, "env", "e", nil, "Environment variable KEY=VALUE (repeatable)")
	sandboxExecCmd.Flags().StringVarP(&sandboxExecWorkdir, "workdir", "w", "", "Working directory inside the sandbox")
	sandboxExecCmd.Flags().BoolVarP(&sandboxExecStdin, "stdin", "i", false, "Forward stdin to the command")
	sandboxExecCmd.Flags().BoolVarP(&sandboxExecTTY, "tty", "t", false, "Allocate an interactive TTY (implies -i)")
	sandboxCmd.AddCommand(sandboxLogsCmd)
	sandboxCmd.AddCommand(sandboxExecCmd)
	sandboxCmd.AddCommand(sandboxAttachCmd)
}

func runSandboxExec(cmd *cobra.Command, args []string) error {
	dash := cmd.ArgsLenAtDash()
	if dash != 1 || dash >= len(args) {
		return fmt.Errorf("usage: swiftctl sandbox exec [-it] <name> -- command [args...]")
	}
	return execOrAttach(getNamespace(), args[0], args[dash:],
		sandboxExecEnv, sandboxExecWorkdir, sandboxExecStdin || sandboxExecTTY, sandboxExecTTY)
}

func runSandboxAttach(cmd *cobra.Command, args []string) error {
	name := args[0]
	command := []string{"/bin/sh"}
	if dash := cmd.ArgsLenAtDash(); dash >= 1 && dash < len(args) {
		command = args[dash:]
	}
	return execOrAttach(getNamespace(), name, command, nil, "", true, true)
}

// execOrAttach builds the streaming exec request and dispatches to the TTY-attach or
// plain-stream client. A non-zero workload exit propagates via os.Exit.
func execOrAttach(ns, name string, command, env []string, workdir string, stdin, tty bool) error {
	config, err := kubeConfig.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	// A warm-pool checkout runs in the claimed slot pod, not a pod named after the
	// sandbox — target the pod (and its run dir) that actually holds the guest.
	target, err := sandboxTargetPod(ns, name)
	if err != nil {
		return err
	}

	vsockSock := "/var/lib/kubeswift/run/" + cli.GuestID(ns, target) + "/vsock.sock"
	reqObj := map[string]interface{}{"v": 1, "op": "exec", "argv": command, "stream": true}
	if len(env) > 0 {
		reqObj["env"] = env
	}
	if workdir != "" {
		reqObj["cwd"] = workdir
	}
	if stdin {
		reqObj["stdin"] = true
	}
	if tty {
		reqObj["tty"] = true
		if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			reqObj["rows"] = rows
			reqObj["cols"] = cols
		}
	}
	req, _ := json.Marshal(reqObj)

	var code int
	if tty {
		code, err = agentAttachTTY(config, clientset, ns, target, vsockSock, req)
	} else {
		code, err = agentExecStream(config, clientset, ns, target, vsockSock, req, stdin)
	}
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// dialAgent execs socat in the launcher pod (a raw pipe to CH's vsock unix socket) and
// performs CH's hybrid-vsock CONNECT handshake to the in-guest agent on agentVsockPort.
// It returns a reader over the agent stream, a writer to it, and the executor's done
// channel; the request must be written AFTER the returned handshake succeeds.
func dialAgent(config *rest.Config, clientset *kubernetes.Clientset, ns, pod, vsockSock string) (*bufio.Reader, *io.PipeWriter, chan error, error) {
	waitAndSocat := fmt.Sprintf("for i in $(seq 1 10); do test -S %q && break; sleep 1; done; exec socat -t10 - UNIX-CONNECT:%s", vsockSock, vsockSock)
	execReq := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: cli.LauncherContainer,
			Command:   []string{"sh", "-c", waitAndSocat},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, clientgoscheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(config, "POST", execReq.URL())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("exec setup: %w", err)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- executor.StreamWithContext(context.Background(), remotecommand.StreamOptions{
			Stdin: inR, Stdout: outW, Stderr: os.Stderr,
		})
		outW.Close()
	}()

	br := bufio.NewReader(outR)
	if _, err := io.WriteString(inW, fmt.Sprintf("CONNECT %d\n", agentVsockPort)); err != nil {
		return nil, nil, nil, fmt.Errorf("vsock connect: %w", err)
	}
	okLine, err := br.ReadString('\n')
	if err != nil {
		return nil, nil, nil, fmt.Errorf("vsock handshake (is the sandbox running with an agent?): %w", err)
	}
	if !strings.HasPrefix(okLine, "OK ") {
		return nil, nil, nil, fmt.Errorf("vsock handshake failed: %q", strings.TrimSpace(okLine))
	}
	return br, inW, done, nil
}

// agentExecStream sends a streaming exec request and copies the agent's framed
// stdout/stderr to os.Stdout/os.Stderr LIVE, returning the workload's exit code when
// the terminal Exit frame arrives (see internal/guestagent). When stdin is set it also
// forwards os.Stdin as FrameStdin frames (line-buffered; the raw-TTY path is
// agentAttachTTY).
func agentExecStream(config *rest.Config, clientset *kubernetes.Clientset, ns, pod, vsockSock string, req []byte, stdin bool) (int, error) {
	br, inW, done, err := dialAgent(config, clientset, ns, pod, vsockSock)
	if err != nil {
		return 1, err
	}
	defer func() { inW.Close(); <-done }()

	if _, err := inW.Write(append(req, '\n')); err != nil {
		return 1, fmt.Errorf("send request: %w", err)
	}
	if stdin {
		go forwardStdin(guestagent.NewFrameWriter(inW))
	}
	for {
		typ, payload, err := guestagent.ReadFrame(br)
		if err != nil {
			if err == io.EOF {
				return 0, nil // stream ended without an explicit Exit frame
			}
			return 1, fmt.Errorf("read agent stream: %w", err)
		}
		switch typ {
		case guestagent.FrameStdout:
			os.Stdout.Write(payload)
		case guestagent.FrameStderr:
			os.Stderr.Write(payload)
		case guestagent.FrameExit:
			return guestagent.DecodeExitCode(payload), nil
		}
	}
}

// agentAttachTTY runs an interactive exec: it puts the local terminal in raw mode,
// forwards os.Stdin as FrameStdin and terminal-resize (SIGWINCH) as FrameResize, and
// copies the guest PTY's output frames to the terminal until the Exit frame.
func agentAttachTTY(config *rest.Config, clientset *kubernetes.Clientset, ns, pod, vsockSock string, req []byte) (int, error) {
	br, inW, done, err := dialAgent(config, clientset, ns, pod, vsockSock)
	if err != nil {
		return 1, err
	}
	defer func() { inW.Close(); <-done }()

	if _, err := inW.Write(append(req, '\n')); err != nil {
		return 1, fmt.Errorf("send request: %w", err)
	}
	fw := guestagent.NewFrameWriter(inW)

	// raw mode so keystrokes (incl. Ctrl-C / Ctrl-D) reach the guest shell verbatim.
	var restore func()
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			restore = func() { _ = term.Restore(int(os.Stdin.Fd()), old) }
			defer restore()
		}
	}

	go forwardStdin(fw)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
				_ = fw.Write(guestagent.FrameResize, guestagent.ResizePayload(uint16(rows), uint16(cols)))
			}
		}
	}()

	for {
		typ, payload, err := guestagent.ReadFrame(br)
		if err != nil {
			if err == io.EOF {
				return 0, nil
			}
			return 1, fmt.Errorf("read agent stream: %w", err)
		}
		switch typ {
		case guestagent.FrameStdout:
			os.Stdout.Write(payload)
		case guestagent.FrameStderr:
			os.Stderr.Write(payload)
		case guestagent.FrameExit:
			if restore != nil {
				restore()
			}
			return guestagent.DecodeExitCode(payload), nil
		}
	}
}

// forwardStdin copies os.Stdin into FrameStdin frames until EOF, then sends a
// FrameStdinClose so the guest closes the command's stdin (a piped `cat`/`sh` sees EOF
// and exits rather than hanging).
func forwardStdin(fw *guestagent.FrameWriter) {
	buf := make([]byte, 32*1024)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if werr := fw.Write(guestagent.FrameStdin, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			_ = fw.Write(guestagent.FrameStdinClose, nil)
			return
		}
	}
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

	// The guest console is captured to <run>/serial.sock.log (swiftletd's
	// --serial file= for sandboxes). For a warm-pool checkout that run dir lives
	// in the claimed slot pod, not a pod named after the sandbox — resolve both.
	target, err := sandboxTargetPod(ns, name)
	if err != nil {
		return err
	}
	serialLog := "/var/lib/kubeswift/run/" + cli.GuestID(ns, target) + "/serial.sock.log"
	shellCmd := "cat " + serialLog
	if sandboxLogsFollow {
		// tail from the start, then follow; keep waiting even if the file appears late.
		shellCmd = "tail -n +1 -F " + serialLog
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(ns).
		Name(target).
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

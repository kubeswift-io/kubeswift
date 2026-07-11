package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kubeswift-io/kubeswift/internal/cli"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// agentVsockPort is the AF_VSOCK port the in-guest agent listens on (matches
// cmd/kubeswift-guest-agent DefaultPort and swift-vsock-client).
const agentVsockPort = 1024

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
command runs in the sandbox's OCI root filesystem. Non-interactive (no stdin/TTY);
stdout and stderr are returned and the command's exit code is propagated.`,
	Example: `  swiftctl sandbox exec my-job -- ls -la /
  swiftctl -n ci sandbox exec my-job -- cat /etc/os-release`,
	Args:         cobra.MinimumNArgs(2),
	SilenceUsage: true,
	RunE:         runSandboxExec,
}

var (
	sandboxExecEnv     []string
	sandboxExecWorkdir string
)

func init() {
	sandboxLogsCmd.Flags().BoolVarP(&sandboxLogsFollow, "follow", "f", false, "Follow the log output")
	sandboxExecCmd.Flags().StringArrayVarP(&sandboxExecEnv, "env", "e", nil, "Environment variable KEY=VALUE (repeatable)")
	sandboxExecCmd.Flags().StringVarP(&sandboxExecWorkdir, "workdir", "w", "", "Working directory inside the sandbox")
	sandboxCmd.AddCommand(sandboxLogsCmd)
	sandboxCmd.AddCommand(sandboxExecCmd)
}

func runSandboxExec(cmd *cobra.Command, args []string) error {
	dash := cmd.ArgsLenAtDash()
	if dash != 1 || dash >= len(args) {
		return fmt.Errorf("usage: swiftctl sandbox exec <name> -- command [args...]")
	}
	name := args[0]
	command := args[dash:]
	ns := getNamespace()

	config, err := kubeConfig.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	vsockSock := "/var/lib/kubeswift/run/" + cli.GuestID(ns, name) + "/vsock.sock"
	reqObj := map[string]interface{}{"v": 1, "op": "exec", "argv": command}
	if len(sandboxExecEnv) > 0 {
		reqObj["env"] = sandboxExecEnv
	}
	if sandboxExecWorkdir != "" {
		reqObj["cwd"] = sandboxExecWorkdir
	}
	req, _ := json.Marshal(reqObj)

	resp, err := agentRequest(config, clientset, ns, name, vsockSock, req)
	if err != nil {
		return err
	}
	if resp.Stdout != "" {
		fmt.Fprint(os.Stdout, resp.Stdout)
	}
	if resp.Stderr != "" {
		fmt.Fprint(os.Stderr, resp.Stderr)
	}
	if !resp.OK {
		return fmt.Errorf("agent exec failed: %s", resp.Error)
	}
	if resp.ExitCode != nil && *resp.ExitCode != 0 {
		os.Exit(*resp.ExitCode)
	}
	return nil
}

// agentResponse is the subset of the guest agent's reply swiftctl needs.
type agentResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode *int   `json:"exitCode"`
}

// agentRequest execs socat in the launcher pod (a raw pipe to CH's vsock unix socket),
// performs CH's hybrid-vsock CONNECT handshake, sends one JSON request to the in-guest
// agent on agentVsockPort, and returns the parsed JSON response. The request must be
// sent AFTER the "OK" line, so swiftctl drives the protocol over the exec stdin/stdout.
func agentRequest(config *rest.Config, clientset *kubernetes.Clientset, ns, pod, vsockSock string, req []byte) (*agentResponse, error) {
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
		return nil, fmt.Errorf("exec setup: %w", err)
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
		return nil, fmt.Errorf("vsock connect: %w", err)
	}
	okLine, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("vsock handshake (is the sandbox running with an agent?): %w", err)
	}
	if !strings.HasPrefix(okLine, "OK ") {
		return nil, fmt.Errorf("vsock handshake failed: %q", strings.TrimSpace(okLine))
	}
	if _, err := inW.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	respLine, err := br.ReadString('\n')
	if err != nil && respLine == "" {
		return nil, fmt.Errorf("read agent response: %w", err)
	}
	inW.Close()
	<-done

	var resp agentResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(respLine)), &resp); err != nil {
		return nil, fmt.Errorf("parse agent response %q: %w", strings.TrimSpace(respLine), err)
	}
	return &resp, nil
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

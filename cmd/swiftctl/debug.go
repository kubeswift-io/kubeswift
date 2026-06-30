package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubeswift-io/kubeswift/internal/cli"
	"github.com/kubeswift-io/kubeswift/internal/scheme"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	debugShell bool
)

var debugCmd = &cobra.Command{
	Use:          "debug [guest-name]",
	Short:        "Troubleshoot guest VM (runtime dir, CH process, serial socket)",
	SilenceUsage: true,
	Long: `Run diagnostics on the launcher pod to troubleshoot VM issues.
Shows runtime directory contents, Cloud Hypervisor process and args, and serial socket status.
Use --shell to drop into an interactive shell in the launcher container.`,
	Example: `  swiftctl debug sample
  swiftctl debug sample --shell
  swiftctl -n myns debug my-guest`,
	Args: cobra.ExactArgs(1),
	RunE: runDebug,
}

func init() {
	debugCmd.Flags().BoolVar(&debugShell, "shell", false, "Drop into interactive shell in launcher container")
}

func runDebug(cmd *cobra.Command, args []string) error {
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

	guestID := cli.GuestID(ns, guestName)
	runtimeDir := "/var/lib/kubeswift/run/" + guestID
	serialSocket := runtimeDir + "/serial.sock"

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	execInPod := func(command []string) (string, error) {
		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(pod.Namespace).
			Name(pod.Name).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: cli.LauncherContainer,
				Command:   command,
				Stdout:    true,
				Stderr:    true,
			}, clientgoscheme.ParameterCodec)

		exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			return "", err
		}

		var buf bytes.Buffer
		err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: &buf,
			Stderr: &buf,
		})
		if err != nil {
			return buf.String(), err
		}
		return buf.String(), nil
	}

	if debugShell {
		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(pod.Namespace).
			Name(pod.Name).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: cli.LauncherContainer,
				Command:   []string{"sh"},
				Stdin:     true,
				Stdout:    true,
				Stderr:    true,
				TTY:       true,
			}, clientgoscheme.ParameterCodec)

		exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			return fmt.Errorf("failed to exec: %w", err)
		}

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
		}()

		fmt.Fprintf(os.Stderr, "=== Shell in launcher pod %s/%s (guest %s) ===\n", pod.Namespace, pod.Name, guestName)
		fmt.Fprintf(os.Stderr, "You are in the LAUNCHER container (host), not the VM. To connect to the VM, run: swiftctl console %s\n", guestName)
		fmt.Fprintf(os.Stderr, "Runtime dir: %s\n", runtimeDir)
		fmt.Fprintf(os.Stderr, "Serial socket: %s\n", serialSocket)
		fmt.Fprintf(os.Stderr, "To test VM serial manually: socat -,raw,echo=0 UNIX-CONNECT:%s\n", serialSocket)
		fmt.Fprintf(os.Stderr, "Use 'exit' to leave.\n\n")

		streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin:  os.Stdin,
			Stdout: os.Stdout,
			Stderr: os.Stderr,
			Tty:    true,
		})
		if streamErr != nil && ctx.Err() == nil {
			return fmt.Errorf("shell failed: %w", streamErr)
		}
		return nil
	}

	// Run diagnostics
	fmt.Println("=== swiftctl debug:", guestName, "===")
	fmt.Println("Pod:", pod.Namespace+"/"+pod.Name)
	fmt.Println("Guest phase:", guest.Status.Phase)
	fmt.Println("Runtime dir:", runtimeDir)
	fmt.Println("Serial socket:", serialSocket)
	fmt.Println()

	// 1. Runtime dir contents
	fmt.Println("--- Runtime directory contents ---")
	out, err := execInPod([]string{"ls", "-la", runtimeDir})
	if err != nil {
		fmt.Printf("  (ls failed: %v)\n", err)
	} else {
		scanner := bufio.NewScanner(bytes.NewReader([]byte(out)))
		for scanner.Scan() {
			fmt.Println(" ", scanner.Text())
		}
	}
	fmt.Println()

	// 2. Serial socket check
	fmt.Println("--- Serial socket ---")
	out, err = execInPod([]string{"sh", "-c", "test -S " + serialSocket + " && echo 'EXISTS' || echo 'NOT FOUND'"})
	if err != nil {
		fmt.Printf("  (check failed: %v)\n", err)
	} else {
		if bytes.Contains([]byte(out), []byte("EXISTS")) {
			fmt.Println("  serial.sock: EXISTS")
		} else {
			fmt.Println("  serial.sock: NOT FOUND")
		}
	}
	fmt.Println()

	// 3. CH command line (from /proc — no ps/pgrep needed; cmdline is NOT
	// truncated to 15 chars like /proc/<pid>/comm, so it reliably matches the
	// 16-char "cloud-hypervisor" name, TFU #8). Anchor on argv[0]'s basename so
	// the scan does not match unrelated processes whose args merely MENTION
	// cloud-hypervisor — including this very `sh -c` scan command (which contains
	// the literal string in its case pattern and would otherwise list itself).
	fmt.Println("--- Cloud Hypervisor command line (from /proc) ---")
	out, err = execInPod([]string{"sh", "-c", `for d in /proc/[0-9]*; do pid=${d#/proc/}; [ -r "/proc/$pid/cmdline" ] || continue; argv0=$(tr '\0' '\n' < "/proc/$pid/cmdline" 2>/dev/null | head -1); [ "${argv0##*/}" = "cloud-hypervisor" ] || continue; echo "PID $pid: $(tr '\0' ' ' < /proc/$pid/cmdline 2>/dev/null)"; done`})
	if err != nil {
		fmt.Printf("  (failed: %v)\n", err)
	} else if len(bytes.TrimSpace([]byte(out))) == 0 {
		fmt.Println("  No cloud-hypervisor process found (check launcher logs for spawn args)")
	} else {
		scanner := bufio.NewScanner(bytes.NewReader([]byte(out)))
		for scanner.Scan() {
			fmt.Println(" ", scanner.Text())
		}
	}
	fmt.Println()

	// 5. Runtime intent
	fmt.Println("--- Runtime intent (guestId) ---")
	out, err = execInPod([]string{"cat", "/var/lib/kubeswift/intent/runtime-intent.json"})
	if err != nil {
		fmt.Printf("  (read failed: %v)\n", err)
	} else {
		fmt.Println(" ", out)
	}
	fmt.Println()

	fmt.Println("=== End debug ===")
	fmt.Println("Tip: Use 'swiftctl debug", guestName, "--shell' to get an interactive shell.")

	return nil
}

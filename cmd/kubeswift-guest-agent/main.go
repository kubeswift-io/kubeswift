package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	maxRequestBytes = 64 * 1024
	recvTimeoutSecs = 30
)

func main() {
	port := flag.Int("port", DefaultPort, "AF_VSOCK port to listen on")
	execRoot := flag.String("exec-root", "", "chroot dir for the exec op (empty = agent's own root; SwiftSandbox passes /newroot)")
	runMode := flag.Bool("run", false, "one-shot: exec the argv after -- chrooted into --exec-root at --cwd, inheriting stdio; exit with the child's code. Used by the sandbox bridge to honor spec.workingDir without a guest shell.")
	runCwd := flag.String("cwd", "", "working directory INSIDE --exec-root for --run (must exist in the rootfs)")
	var id identity
	flag.StringVar(&id.User, "user", "", "who the workload runs as (user, uid, user:group, uid:gid; resolved in --exec-root's /etc/passwd and /etc/group); also the default for exec")
	flag.BoolVar(&id.NoNewPrivs, "no-new-privs", false, "run the workload (and exec) with no_new_privs")
	flag.BoolVar(&id.NonRoot, "non-root", false, "refuse to run the workload (or exec) as uid 0")
	ownList := flag.String("own-list", "", "--run: file listing paths inside --exec-root (one per line) to give to the workload user first")
	mkdirMode := flag.Bool("mkdir", false, "one-shot: create the directory named by the argument inside --exec-root, resolving the image's symlinks as the workload would, and print its path from outside --exec-root (the sandbox bridge mounts there)")
	writeMode := flag.Bool("write-file", false, "one-shot: write stdin to the file named by the argument inside --exec-root, resolving the image's symlinks as the workload would")
	fileMode := flag.String("mode", "0400", "--write-file: the file's octal mode")
	flag.Parse()

	// One-shot local exec (the sandbox cold-boot path): chroot + chdir + exec via
	// the same buildExecCmd the vsock exec op uses (in-chroot PATH resolution, works
	// for distroless — no guest shell). The bridge runs this as a FOREGROUND child so
	// it stays PID 1 and still captures the exit code + powers the VM off.
	if *runMode {
		os.Exit(runOneShot(*execRoot, *runCwd, id, *ownList, flag.Args()))
	}
	if *mkdirMode || *writeMode {
		os.Exit(pathOneShot(*execRoot, *mkdirMode, *fileMode, flag.Args()))
	}
	if env := os.Getenv("KUBESWIFT_GUEST_AGENT_PORT"); env != "" {
		if p, err := strconv.Atoi(env); err == nil {
			*port = p
		}
	}

	h := &handler{sys: realSystem{}, version: version, execRoot: *execRoot}
	h.workload.set(id)

	lfd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		log.Fatalf("kubeswift-guest-agent: AF_VSOCK socket: %v", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(*port)}
	if err := unix.Bind(lfd, sa); err != nil {
		log.Fatalf("kubeswift-guest-agent: bind vsock port %d: %v", *port, err)
	}
	if err := unix.Listen(lfd, 16); err != nil {
		log.Fatalf("kubeswift-guest-agent: listen: %v", err)
	}
	log.Printf("kubeswift-guest-agent %s listening on AF_VSOCK port %d", version, *port)

	for {
		nfd, _, err := unix.Accept(lfd)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			log.Printf("kubeswift-guest-agent: accept: %v", err)
			continue
		}
		go serve(h, nfd)
	}
}

// runOneShot chroots into root, chdirs to cwd, and execs argv as a foreground
// child inheriting the caller's stdio, returning the child's exit code. Env is the
// caller's own environment (the bridge exports the config-disk ENV before invoking
// this), matching what a plain `chroot root argv` would inherit — buildExecCmd's
// synthetic PATH/HOME defaults are deliberately overridden (HOME still follows
// the workload user when the bridge left it at "/"). The workload runs as id;
// the paths in ownList are given to that user first. Returns 127 when the
// command cannot be built or started (mirrors the shell "command not found"
// code), 126 when the identity cannot be honoured.
func runOneShot(root, cwd string, id identity, ownList string, argv []string) int {
	h := &handler{sys: realSystem{}, version: version, execRoot: root}
	h.workload.set(id)
	cmd, plan, err := h.buildExecCmd(Request{Argv: argv, Cwd: cwd})
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubeswift-guest-agent --run:", err)
		if errors.Is(err, errIdentity) {
			return 126
		}
		return 127
	}
	if ownList != "" {
		b, err := os.ReadFile(ownList)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "kubeswift-guest-agent --run:", err)
			return 126
		}
		if err := chownPaths(root, plan.creds, strings.Split(string(b), "\n")); err != nil {
			fmt.Fprintln(os.Stderr, "kubeswift-guest-agent --run:", err)
			return 126
		}
	}
	cmd.Env = withHome(os.Environ(), plan.creds) // inherit the bridge's env, exactly like `chroot root argv` does
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := runPlanned(cmd, plan); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "kubeswift-guest-agent --run:", err)
		return 127
	}
	return 0
}

// pathOneShot is --mkdir and --write-file, for the sandbox bridge's cold
// boot: the same root-confined resolution the warm checkout path uses, so an
// image symlink cannot redirect a secret file or a mount point out of the
// sandbox root. Exits 0 on success, 1 with the error on stderr otherwise.
func pathOneShot(root string, mkdir bool, mode string, args []string) int {
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "kubeswift-guest-agent:", err)
		return 1
	}
	if len(args) != 1 {
		return fail(errors.New("--mkdir and --write-file take exactly one path"))
	}
	if mkdir {
		dir, err := mkdirInRoot(root, args[0], 0o755)
		if err != nil {
			return fail(fmt.Errorf("mkdir %s: %w", args[0], err))
		}
		fmt.Println(dir)
		return 0
	}
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil || m > 0o7777 {
		return fail(fmt.Errorf("--mode %q is not an octal file mode", mode))
	}
	if err := writeFileInRoot(root, args[0], os.Stdin, os.FileMode(m)); err != nil {
		return fail(fmt.Errorf("write %s: %w", args[0], err))
	}
	return 0
}

// serve reads one newline-delimited JSON request and dispatches it. Single-shot ops
// write one JSON response and close. A streaming exec (op=exec, stream=true) hands the
// connection to execStream, which writes framed stdout/stderr/exit for the command's
// lifetime (see internal/guestagent).
func serve(h *handler, nfd int) {
	defer unix.Close(nfd)
	// bound the request read so a stuck/hostile client cannot pin a goroutine
	tv := unix.Timeval{Sec: recvTimeoutSecs}
	_ = unix.SetsockoptTimeval(nfd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

	line, rest := readRequestLine(nfd)

	var req Request
	if json.Unmarshal(line, &req) == nil && req.Op == "exec" && req.Stream {
		// clear the read deadline — a streamed command may run far longer than the
		// request-read timeout, and stdin/attach keeps the host sending frames.
		_ = unix.SetsockoptTimeval(nfd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{})
		rw := &fdReadWriter{fd: nfd, rest: rest}
		if req.TTY {
			h.execAttach(rw, req)
		} else {
			h.execStream(rw, req)
		}
		return
	}

	writeAll(nfd, h.handle(line))
}

// readRequestLine reads one newline-delimited request (bounded by maxRequestBytes) and
// returns the line plus any bytes already read past the newline (the start of the frame
// stream for a streaming/attach request).
func readRequestLine(nfd int) (line, rest []byte) {
	var buf []byte
	tmp := make([]byte, 4096)
	for len(buf) < maxRequestBytes {
		n, err := unix.Read(nfd, tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				return buf[:i], buf[i+1:]
			}
		}
		if err != nil || n == 0 {
			break
		}
	}
	return buf, nil
}

func writeAll(nfd int, resp []byte) {
	for len(resp) > 0 {
		n, err := unix.Write(nfd, resp)
		if n > 0 {
			resp = resp[n:]
		}
		if err != nil {
			break
		}
	}
}

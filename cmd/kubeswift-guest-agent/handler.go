// Command kubeswift-guest-agent is a tiny in-guest agent that regenerates a
// cloneFromSnapshot clone's identity (machine-id / SSH host keys / hostname /
// MAC) and renews its DHCP lease IN PLACE — with no reboot — over a host-only
// vsock channel.
//
// WHY THIS EXISTS (load-bearing — read before refactoring):
//
// A cloneFromSnapshot SwiftGuest boots via Cloud Hypervisor `--restore`: it
// RESUMES the source's captured RAM byte-for-byte. A resume is NOT a boot —
// cloud-init does not re-run — so every clone inherits the source's
// /etc/machine-id, /etc/ssh/ssh_host_*, hostname, and the cached eth0 MAC + IP.
// The old remedy ("reboot the clone once") is BROKEN on Cloud Hypervisor v52: a
// restored guest's reboot hangs in EDK2 firmware. This agent is the
// real fix — it regenerates identity without a reboot.
//
// THE LOAD-BEARING CONSTRAINT: because a clone never boots, this agent must
// already be RUNNING in the SOURCE guest at snapshot-capture time so it is part
// of the captured RAM and resumes — alive and listening — in every clone.
// Installing/starting it on the clone is impossible (nothing new starts on a
// resume).
//
// SECURITY: for identity guests the command surface is exactly two ops (ping,
// regenerate-identity) with a fixed schema — inputs (MAC, hostname) are validated
// then passed as argv (never shell-interpolated). The `exec` op (run an arbitrary
// command in the guest) is ENABLED ONLY when the agent is started with --exec-root —
// i.e. SwiftSandbox, which runs untrusted code the host already fully controls;
// identity guests run WITHOUT --exec-root, so exec is unavailable there. The transport
// is vsock — host<->guest only, not network-reachable — so only the (trusted) host can
// reach any op.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ProtocolVersion is the wire-protocol version (design §Q3). Bump only on a
// breaking change; the agent accepts any request and degrades per-item.
const ProtocolVersion = 1

// DefaultPort is the AF_VSOCK port the agent listens on (above the privileged
// range; shared constant with the host-side swift-vsock-client).
const DefaultPort = 1024

// Identity item names — MUST match api/swift/v1alpha1.CloneIdentityItem so the
// controller can pass SwiftGuest.spec.cloneFromSnapshot.regenerate verbatim.
const (
	itemHostname     = "hostname"
	itemMachineID    = "machineId"
	itemSSHHostKeys  = "sshHostKeys"
	itemMACAddresses = "macAddresses"
)

// allItems is the default set when a request omits items (matches the CRD's
// "empty defaults to all four").
var allItems = []string{itemMachineID, itemSSHHostKeys, itemHostname, itemMACAddresses}

// Request is one host->guest command (newline-delimited JSON, one per connection).
type Request struct {
	V          int      `json:"v"`
	Op         string   `json:"op"`
	Items      []string `json:"items,omitempty"`
	MAC        string   `json:"mac,omitempty"`
	Hostname   string   `json:"hostname,omitempty"`
	RenewLease bool     `json:"renewLease,omitempty"`
	// exec op:
	Argv   []string `json:"argv,omitempty"`
	Env    []string `json:"env,omitempty"`
	Cwd    string   `json:"cwd,omitempty"`
	Stream bool     `json:"stream,omitempty"` // stream stdout/stderr/exit as frames (see internal/guestagent); dispatched in serve(), not handle()
	Stdin  bool     `json:"stdin,omitempty"`  // read host FrameStdin frames into the command's stdin
	TTY    bool     `json:"tty,omitempty"`    // allocate a PTY (interactive attach); implies stdin
	Rows   uint16   `json:"rows,omitempty"`   // initial TTY rows (tty only)
	Cols   uint16   `json:"cols,omitempty"`   // initial TTY cols (tty only)
}

// Response is the guest->host reply.
type Response struct {
	V            int      `json:"v"`
	OK           bool     `json:"ok"`
	Op           string   `json:"op,omitempty"`
	AgentVersion string   `json:"agentVersion,omitempty"`
	Regenerated  []string `json:"regenerated,omitempty"`
	NewIP        string   `json:"newIP,omitempty"`
	MachineID    string   `json:"machineId,omitempty"`
	Hostname     string   `json:"hostname,omitempty"`
	MAC          string   `json:"mac,omitempty"`
	IP           string   `json:"ip,omitempty"`
	Error        string   `json:"error,omitempty"`
	// exec op:
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

var (
	macRE      = regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)
	hostnameRE = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
)

// system abstracts every side effect so the dispatch + validation logic is unit
// testable without root. The real implementation (realSystem) shells out to
// ip/hostnamectl/ssh-keygen/dhclient and touches the filesystem.
type system interface {
	run(name string, args ...string) (string, error)
	readFile(path string) (string, error)
	writeFile(path string, data []byte, perm os.FileMode) error
	remove(path string) error
	glob(pattern string) ([]string, error)
	hostname() (string, error)
}

type handler struct {
	sys     system
	version string
	// execRoot is the chroot dir for the exec op (empty = run in the agent's own root).
	// SwiftSandbox passes /newroot (the OCI overlay) so exec runs in the workload's
	// filesystem; identity guests leave it empty.
	execRoot string
}

// handle parses one request, dispatches it, and returns the JSON response bytes.
// It NEVER returns a transport error to the caller for a malformed/uop request —
// it returns a Response with ok=false so the host always sees a structured
// answer (no silent failure, design principle #6).
func (h *handler) handle(raw []byte) []byte {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return h.encode(Response{V: ProtocolVersion, OK: false, Error: "bad request: " + err.Error()})
	}
	var resp Response
	switch req.Op {
	case "ping", "status", "":
		resp = h.status()
	case "regenerate-identity":
		resp = h.regenerate(req)
	case "exec":
		resp = h.exec(req)
	default:
		resp = Response{OK: false, Error: "unknown op: " + req.Op}
	}
	resp.V = ProtocolVersion
	resp.Op = req.Op
	resp.AgentVersion = h.version
	return h.encode(resp)
}

func (h *handler) encode(r Response) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		// last-resort, should never happen for our flat struct
		return []byte(`{"v":1,"ok":false,"error":"encode failed"}` + "\n")
	}
	return append(b, '\n')
}

// status reports the guest's current identity (used as the liveness/readiness
// probe before a regen, and to confirm a regen's effect).
func (h *handler) status() Response {
	r := Response{OK: true}
	r.MachineID = h.machineID()
	if hn, err := h.sys.hostname(); err == nil {
		r.Hostname = hn
	}
	ifc, _ := h.primaryIface()
	r.MAC = h.ifaceMAC(ifc)
	r.IP = h.ifaceIP(ifc)
	return r
}

// regenerate applies the requested identity changes in place. Order matters:
// machine-id / ssh / hostname first, then the MAC, then the lease renew (so the
// renew happens after the MAC + machine-id-derived DUID change).
func (h *handler) regenerate(req Request) Response {
	items := req.Items
	if len(items) == 0 {
		items = allItems
	}
	want := map[string]bool{}
	for _, it := range items {
		want[it] = true
	}

	resp := Response{OK: true}
	var done []string
	var firstErr string
	fail := func(msg string) {
		if firstErr == "" {
			firstErr = msg
		}
	}

	if want[itemMachineID] {
		if err := h.regenMachineID(); err != nil {
			fail("machineId: " + err.Error())
		} else {
			done = append(done, itemMachineID)
		}
	}
	if want[itemSSHHostKeys] {
		if err := h.regenSSHHostKeys(); err != nil {
			fail("sshHostKeys: " + err.Error())
		} else {
			done = append(done, itemSSHHostKeys)
		}
	}
	if want[itemHostname] {
		if err := h.setHostname(req.Hostname); err != nil {
			fail("hostname: " + err.Error())
		} else {
			done = append(done, itemHostname)
		}
	}
	if want[itemMACAddresses] {
		if err := h.setMAC(req.MAC); err != nil {
			fail("macAddresses: " + err.Error())
		} else {
			done = append(done, itemMACAddresses)
		}
	}
	if req.RenewLease {
		ip, err := h.renewLease()
		if err != nil {
			fail("renewLease: " + err.Error())
		}
		resp.NewIP = ip
	}

	sort.Strings(done)
	resp.Regenerated = done
	resp.Error = firstErr
	resp.OK = firstErr == ""
	// echo the resulting identity so the host can confirm without a second probe
	resp.MachineID = h.machineID()
	if hn, err := h.sys.hostname(); err == nil {
		resp.Hostname = hn
	}
	ifc, _ := h.primaryIface()
	resp.MAC = h.ifaceMAC(ifc)
	if resp.IP == "" {
		resp.IP = h.ifaceIP(ifc)
	}
	return resp
}

// --- identity ops -------------------------------------------------------------

func (h *handler) machineID() string {
	s, _ := h.sys.readFile("/etc/machine-id")
	return strings.TrimSpace(s)
}

func (h *handler) regenMachineID() error {
	// Remove the inherited id (and the dbus copy, which is often a separate file
	// on older images), then let systemd mint a fresh one.
	_ = h.sys.remove("/etc/machine-id")
	_ = h.sys.remove("/var/lib/dbus/machine-id")
	if _, err := h.sys.run("systemd-machine-id-setup"); err != nil {
		// fallback for images without systemd-machine-id-setup: dbus-uuidgen
		if _, e2 := h.sys.run("dbus-uuidgen", "--ensure=/etc/machine-id"); e2 != nil {
			return err
		}
	}
	return nil
}

func (h *handler) regenSSHHostKeys() error {
	keys, err := h.sys.glob("/etc/ssh/ssh_host_*")
	if err != nil {
		return err
	}
	for _, k := range keys {
		_ = h.sys.remove(k)
	}
	if _, err := h.sys.run("ssh-keygen", "-A"); err != nil {
		return err
	}
	// Reload sshd so it picks up the new host keys without dropping the agent.
	// Best-effort: the service unit name differs across distros (ssh vs sshd).
	if _, err := h.sys.run("systemctl", "reload", "ssh"); err != nil {
		_, _ = h.sys.run("systemctl", "reload", "sshd")
	}
	return nil
}

func (h *handler) setHostname(name string) error {
	if name == "" {
		return fmt.Errorf("empty hostname")
	}
	if !hostnameRE.MatchString(name) {
		return fmt.Errorf("invalid hostname %q", name)
	}
	if _, err := h.sys.run("hostnamectl", "set-hostname", name); err != nil {
		// fallback for images without hostnamectl
		if e2 := h.sys.writeFile("/etc/hostname", []byte(name+"\n"), 0o644); e2 != nil {
			return err
		}
		if _, e3 := h.sys.run("hostname", name); e3 != nil {
			return err
		}
	}
	return nil
}

func (h *handler) setMAC(mac string) error {
	if mac == "" {
		return fmt.Errorf("empty mac")
	}
	if !macRE.MatchString(mac) {
		return fmt.Errorf("invalid mac %q", mac)
	}
	ifc, err := h.primaryIface()
	if err != nil {
		return err
	}
	// down -> set address -> up on the LIVE virtio-net link. The PR-0 spike
	// confirmed this does not wedge the resumed link (ping/fdb/ARP all recover
	// on the new MAC).
	_, _ = h.sys.run("ip", "link", "set", ifc, "down")
	if _, err := h.sys.run("ip", "link", "set", ifc, "address", mac); err != nil {
		_, _ = h.sys.run("ip", "link", "set", ifc, "up")
		return err
	}
	_, err = h.sys.run("ip", "link", "set", ifc, "up")
	return err
}

// renewLease re-DHCPs the primary interface so a lease lands in THIS pod's
// dnsmasq for the (v0.4.3) restore lease-poller to discover. It does NOT aim for
// a globally-distinct IP — each clone has its own pod-netns dnsmasq, so the
// guest-internal address is pod-isolated (spike correction; the MAC-set above is
// for guest-MAC <-> host-fdb/DNAT consistency, not IP-distinctness).
func (h *handler) renewLease() (string, error) {
	ifc, err := h.primaryIface()
	if err != nil {
		return "", err
	}
	// dhclient first (isc client keys by MAC); fall back to systemd-networkd
	// (Ubuntu Noble default) which keys by a machine-id-derived DUID.
	_, _ = h.sys.run("dhclient", "-r", ifc)
	if _, err := h.sys.run("dhclient", "-1", ifc); err != nil {
		_, _ = h.sys.run("networkctl", "renew", ifc)
		_, _ = h.sys.run("networkctl", "reconfigure", ifc)
	}
	// give the lease a moment to land, then read the address back
	time.Sleep(2 * time.Second)
	return h.ifaceIP(ifc), nil
}

// --- interface detection ------------------------------------------------------

// primaryIface returns the guest's primary NIC. It does NOT assume "eth0":
// a stock cloud image under predictable naming calls it enp0sN (PR-0 spike
// finding). Prefer the default-route iface; else the first non-virtual link.
func (h *handler) primaryIface() (string, error) {
	if out, err := h.sys.run("ip", "-o", "-4", "route", "show", "default"); err == nil {
		fields := strings.Fields(out)
		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				return fields[i+1], nil
			}
		}
	}
	links, err := h.sys.glob("/sys/class/net/*")
	if err != nil {
		return "", err
	}
	sort.Strings(links)
	for _, l := range links {
		n := filepath.Base(l)
		if n == "lo" || strings.HasPrefix(n, "veth") || strings.HasPrefix(n, "docker") ||
			strings.HasPrefix(n, "br") || strings.HasPrefix(n, "virbr") {
			continue
		}
		// a physical/virtio NIC has a device symlink; bridges/veths don't
		if _, err := h.sys.readFile(l + "/device/uevent"); err == nil {
			return n, nil
		}
	}
	return "", fmt.Errorf("no primary interface found")
}

func (h *handler) ifaceMAC(ifc string) string {
	if ifc == "" {
		return ""
	}
	s, _ := h.sys.readFile("/sys/class/net/" + ifc + "/address")
	return strings.TrimSpace(s)
}

func (h *handler) ifaceIP(ifc string) string {
	if ifc == "" {
		return ""
	}
	out, err := h.sys.run("ip", "-4", "-o", "addr", "show", "dev", ifc)
	if err != nil {
		return ""
	}
	for _, tok := range strings.Fields(out) {
		if strings.Contains(tok, "/") && len(tok) > 0 && tok[0] >= '0' && tok[0] <= '9' {
			return strings.SplitN(tok, "/", 2)[0]
		}
	}
	return ""
}

// --- real system implementation ----------------------------------------------

type realSystem struct{}

func (realSystem) run(name string, args ...string) (string, error) {
	// exec.Command uses argv directly — no shell, so validated MAC/hostname args
	// can never be interpreted (security: no shell interpolation).
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
func (realSystem) readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
func (realSystem) writeFile(p string, data []byte, perm os.FileMode) error {
	return os.WriteFile(p, data, perm)
}
func (realSystem) remove(p string) error           { return os.Remove(p) }
func (realSystem) glob(p string) ([]string, error) { return filepath.Glob(p) }
func (realSystem) hostname() (string, error)       { return os.Hostname() }

// execPathDirs is the PATH searched (inside the chroot) for a bare command name.
var execPathDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// execOutputCap bounds each of stdout/stderr for the non-streaming exec op. Large or
// long-running output is a streaming follow-up.
const execOutputCap = 1 << 20 // 1 MiB

// buildExecCmd resolves argv + chroot + cwd + env into an *exec.Cmd, shared by the
// single-shot exec (buffered response) and execStream (framed) paths. The command
// binary is resolved against a PATH INSIDE the chroot — Go's LookPath would search
// the agent's own (initramfs) root. It returns an error (never runs) when exec is
// disabled or argv is empty.
func (h *handler) buildExecCmd(req Request) (*exec.Cmd, error) {
	// exec is enabled ONLY when the agent was started with --exec-root (SwiftSandbox).
	// Identity guests run without it, so they keep the minimal two-op surface — no
	// arbitrary command execution, even over the host-only vsock.
	if h.execRoot == "" {
		return nil, fmt.Errorf("exec is disabled (no --exec-root; SwiftSandbox only)")
	}
	if len(req.Argv) == 0 {
		return nil, fmt.Errorf("exec: empty argv")
	}
	root := h.execRoot
	prog := req.Argv[0]
	if !strings.Contains(prog, "/") {
		for _, dir := range execPathDirs {
			if fi, err := os.Stat(root + dir + "/" + prog); err == nil && !fi.IsDir() {
				prog = dir + "/" + prog
				break
			}
		}
	}
	cmd := exec.Command(prog, req.Argv[1:]...)
	cmd.Path = prog // skip Go's parent-context LookPath (wrong filesystem)
	dir := req.Cwd
	if root != "" && root != "/" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
		if dir == "" {
			dir = "/" // cwd must exist in the new root; the chroot root always does
		}
	}
	cmd.Dir = dir // "" inherits the agent's cwd (no-chroot case); otherwise relative to the chroot
	cmd.Env = execEnv(req.Env)
	return cmd, nil
}

// exec runs argv chrooted into the sandbox root (h.execRoot) and returns its stdout,
// stderr and exit code in one response.
func (h *handler) exec(req Request) Response {
	cmd, err := h.buildExecCmd(req)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	stdout := &capBuffer{max: execOutputCap}
	stderr := &capBuffer{max: execOutputCap}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return Response{OK: false, Error: "exec: " + err.Error()}
		}
	}
	return Response{OK: true, Stdout: stdout.buf.String(), Stderr: stderr.buf.String(), ExitCode: &code}
}

func execEnv(reqEnv []string) []string {
	env := []string{"PATH=" + strings.Join(execPathDirs, ":"), "HOME=/", "TERM=xterm"}
	return append(env, reqEnv...)
}

// capBuffer accumulates up to max bytes then silently drops the rest, so a runaway
// command can't OOM the agent while still running to completion.
type capBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if rem := c.max - c.buf.Len(); rem > 0 {
		if len(p) > rem {
			c.buf.Write(p[:rem])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

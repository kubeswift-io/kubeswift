package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kubeswift-io/kubeswift/internal/guestagent"
)

// fakeSys records run() invocations and serves canned file/glob/hostname data so
// the dispatch + validation logic is exercised with no root and no real exec.
type fakeSys struct {
	calls    []string
	runErr   map[string]error // keyed by the joined "name arg0 arg1"
	runOut   map[string]string
	files    map[string]string
	globs    map[string][]string
	host     string
	removed  []string
	hostSet  string // captured from `hostname <x>` / writeFile
	writeErr error
}

func newFakeSys() *fakeSys {
	return &fakeSys{
		runErr: map[string]error{}, runOut: map[string]string{},
		files: map[string]string{}, globs: map[string][]string{},
		host: "vsock-src",
	}
}

func key(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

func (f *fakeSys) run(name string, args ...string) (string, error) {
	k := key(name, args...)
	f.calls = append(f.calls, k)
	return f.runOut[k], f.runErr[k]
}
func (f *fakeSys) readFile(p string) (string, error) {
	if v, ok := f.files[p]; ok {
		return v, nil
	}
	return "", os.ErrNotExist
}
func (f *fakeSys) writeFile(p string, data []byte, _ os.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[p] = string(data)
	return nil
}
func (f *fakeSys) remove(p string) error { f.removed = append(f.removed, p); return nil }
func (f *fakeSys) glob(p string) ([]string, error) {
	return f.globs[p], nil
}
func (f *fakeSys) hostname() (string, error) { return f.host, nil }

func (f *fakeSys) called(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// newHandler wires a handler whose fake guest has a default-route iface enp0s4.
func newHandler() (*handler, *fakeSys) {
	f := newFakeSys()
	f.runOut[key("ip", "-o", "-4", "route", "show", "default")] = "default via 192.168.99.1 dev enp0s4 proto dhcp"
	f.runOut[key("ip", "-4", "-o", "addr", "show", "dev", "enp0s4")] = "2: enp0s4    inet 192.168.99.44/24 brd 192.168.99.255 scope global enp0s4"
	f.files["/etc/machine-id"] = "5d0ca4597dbf4f538ba33a3262b3be7f\n"
	f.files["/sys/class/net/enp0s4/address"] = "52:54:00:11:11:11\n"
	return &handler{sys: f, version: "test"}, f
}

func decode(t *testing.T, b []byte) Response {
	t.Helper()
	var r Response
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("decode response: %v (raw=%s)", err, b)
	}
	return r
}

func TestPingReportsIdentity(t *testing.T) {
	h, _ := newHandler()
	r := decode(t, h.handle([]byte(`{"op":"ping"}`)))
	if !r.OK || r.V != ProtocolVersion {
		t.Fatalf("ping not ok: %+v", r)
	}
	if r.MachineID != "5d0ca4597dbf4f538ba33a3262b3be7f" || r.MAC != "52:54:00:11:11:11" || r.IP != "192.168.99.44" {
		t.Fatalf("ping identity wrong: %+v", r)
	}
}

func TestUnknownOp(t *testing.T) {
	h, _ := newHandler()
	r := decode(t, h.handle([]byte(`{"op":"bogus-op"}`)))
	if r.OK || !strings.Contains(r.Error, "unknown op") {
		t.Fatalf("unknown op should fail loudly: %+v", r)
	}
}

func TestExec(t *testing.T) {
	h, _ := newHandler()
	// exec is gated on --exec-root: disabled without it (identity-guest posture).
	if r := decode(t, h.handle([]byte(`{"op":"exec","argv":["echo","hi"]}`))); r.OK || !strings.Contains(r.Error, "disabled") {
		t.Errorf("exec must be disabled without --exec-root: %+v", r)
	}
	h.execRoot = "/" // enable exec; "/" means no chroot, so it runs host commands in the test
	if r := decode(t, h.handle([]byte(`{"op":"exec"}`))); r.OK {
		t.Errorf("empty argv should fail: %+v", r)
	}
	r := decode(t, h.handle([]byte(`{"op":"exec","argv":["echo","hi"]}`)))
	if !r.OK || r.Stdout != "hi\n" || r.ExitCode == nil || *r.ExitCode != 0 {
		t.Errorf("echo: %+v", r)
	}
	r = decode(t, h.handle([]byte(`{"op":"exec","argv":["sh","-c","exit 3"]}`)))
	if !r.OK || r.ExitCode == nil || *r.ExitCode != 3 {
		t.Errorf("exit 3 should propagate: %+v", r)
	}
	// env + cwd are applied.
	r = decode(t, h.handle([]byte(`{"op":"exec","argv":["sh","-c","echo $FOO; pwd"],"env":["FOO=bar"],"cwd":"/tmp"}`)))
	if !r.OK || r.Stdout != "bar\n/tmp\n" {
		t.Errorf("env+cwd: %+v", r)
	}
}

func TestExecStream(t *testing.T) {
	h, _ := newHandler()
	// streaming exec is gated the same way: no --exec-root => a stderr frame + non-zero exit.
	var off bytes.Buffer
	h.execStream(&off, Request{Op: "exec", Stream: true, Argv: []string{"echo", "hi"}})
	if _, _, code := drainFrames(t, &off); code != 255 {
		t.Fatalf("stream exec must be disabled without --exec-root (exit 255), got %d", code)
	}

	h.execRoot = "/" // enable (no chroot in the test)
	var buf bytes.Buffer
	h.execStream(&buf, Request{Op: "exec", Stream: true,
		Argv: []string{"sh", "-c", "echo out; echo err 1>&2; exit 5"}})
	stdout, stderr, code := drainFrames(t, &buf)
	if stdout != "out\n" {
		t.Errorf("stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "err") {
		t.Errorf("stderr=%q", stderr)
	}
	if code != 5 {
		t.Errorf("exit code=%d, want 5", code)
	}

	// env + cwd flow through the same buildExecCmd path.
	var buf2 bytes.Buffer
	h.execStream(&buf2, Request{Op: "exec", Stream: true,
		Argv: []string{"sh", "-c", "echo $FOO; pwd"}, Env: []string{"FOO=bar"}, Cwd: "/tmp"})
	if out, _, _ := drainFrames(t, &buf2); out != "bar\n/tmp\n" {
		t.Errorf("env+cwd stream stdout=%q", out)
	}
}

// drainFrames reads a completed execStream buffer into (stdout, stderr, exitCode).
func drainFrames(t *testing.T, r *bytes.Buffer) (string, string, int) {
	t.Helper()
	var out, errb strings.Builder
	code := -1
	for {
		typ, pay, err := guestagent.ReadFrame(r)
		if err != nil {
			break
		}
		switch typ {
		case guestagent.FrameStdout:
			out.Write(pay)
		case guestagent.FrameStderr:
			errb.Write(pay)
		case guestagent.FrameExit:
			code = guestagent.DecodeExitCode(pay)
		}
	}
	return out.String(), errb.String(), code
}

func TestExecAttachPTY(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no /dev/ptmx in this environment")
	}
	h, _ := newHandler()
	h.execRoot = "/" // enable exec, no chroot in the test

	stdinR, stdinW := io.Pipe() // host->guest; left open so the reader goroutine blocks
	out := &safeBuffer{}
	rw := &rwPair{r: stdinR, w: out}

	done := make(chan struct{})
	go func() {
		// `test -t 1` is true ONLY on a real TTY — proves a PTY was allocated.
		h.execAttach(rw, Request{Op: "exec", Stream: true, TTY: true,
			Argv: []string{"sh", "-c", "test -t 1 && echo HASTTY; exit 4"}})
		close(done)
	}()
	<-done
	stdinW.Close()

	stdout, _, code := drainFrames(t, out.buffer())
	if !strings.Contains(stdout, "HASTTY") {
		t.Errorf("command did not see a TTY (stdout=%q)", stdout)
	}
	if code != 4 {
		t.Errorf("exit code=%d, want 4", code)
	}
}

// rwPair is an io.ReadWriter joining a separate reader and writer for the attach test.
type rwPair struct {
	r io.Reader
	w io.Writer
}

func (p *rwPair) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *rwPair) Write(b []byte) (int, error) { return p.w.Write(b) }

// safeBuffer is a concurrency-safe bytes.Buffer (the frame writer serializes, but the
// output pump and the terminal Exit write happen on different goroutines).
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(b)
}
func (s *safeBuffer) buffer() *bytes.Buffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.NewBuffer(s.buf.Bytes())
}

func TestBadJSON(t *testing.T) {
	h, _ := newHandler()
	r := decode(t, h.handle([]byte(`{not json`)))
	if r.OK || r.Error == "" {
		t.Fatalf("bad json should fail loudly: %+v", r)
	}
}

func TestRegenerateAllItems(t *testing.T) {
	h, f := newHandler()
	req := `{"op":"regenerate-identity","items":["machineId","sshHostKeys","hostname","macAddresses"],"mac":"52:54:00:22:22:22","hostname":"ft-clone-a","renewLease":true}`
	r := decode(t, h.handle([]byte(req)))
	if !r.OK {
		t.Fatalf("regenerate failed: %+v", r)
	}
	got := strings.Join(r.Regenerated, ",")
	for _, it := range []string{"machineId", "sshHostKeys", "hostname", "macAddresses"} {
		if !strings.Contains(got, it) {
			t.Errorf("item %q not regenerated: %v", it, r.Regenerated)
		}
	}
	if !f.called("systemd-machine-id-setup") {
		t.Error("machine-id not regenerated")
	}
	if !f.called("ssh-keygen -A") {
		t.Error("ssh host keys not regenerated")
	}
	if !f.called("hostnamectl set-hostname ft-clone-a") {
		t.Error("hostname not set")
	}
	if !f.called("ip link set enp0s4 address 52:54:00:22:22:22") {
		t.Error("mac not set on detected iface enp0s4")
	}
	if !f.called("dhclient -1 enp0s4") {
		t.Error("lease not renewed")
	}
}

func TestRegenerateEmptyItemsDefaultsToAll(t *testing.T) {
	h, f := newHandler()
	r := decode(t, h.handle([]byte(`{"op":"regenerate-identity","mac":"52:54:00:22:22:22","hostname":"c1"}`)))
	if !r.OK || len(r.Regenerated) != 4 {
		t.Fatalf("empty items should default to all four: %+v", r)
	}
	if !f.called("ssh-keygen -A") || !f.called("systemd-machine-id-setup") {
		t.Error("default-all did not run all ops")
	}
}

func TestInvalidMACRejected(t *testing.T) {
	h, f := newHandler()
	r := decode(t, h.handle([]byte(`{"op":"regenerate-identity","items":["machineId","macAddresses"],"mac":"not-a-mac"}`)))
	if r.OK {
		t.Fatalf("invalid mac must make ok=false: %+v", r)
	}
	if !strings.Contains(r.Error, "macAddresses") || !strings.Contains(r.Error, "invalid mac") {
		t.Errorf("error should name the bad mac: %q", r.Error)
	}
	// machineId still applied even though macAddresses failed (partial success surfaced)
	if !contains(r.Regenerated, "machineId") {
		t.Errorf("machineId should still be done: %v", r.Regenerated)
	}
	if f.called("ip link set enp0s4 address") {
		t.Error("must NOT set an invalid mac on the link")
	}
}

func TestInvalidHostnameRejected(t *testing.T) {
	h, _ := newHandler()
	r := decode(t, h.handle([]byte(`{"op":"regenerate-identity","items":["hostname"],"hostname":"bad host!"}`)))
	if r.OK || !strings.Contains(r.Error, "invalid hostname") {
		t.Fatalf("invalid hostname must be rejected: %+v", r)
	}
}

func TestMACSetOrderingDownAddressUp(t *testing.T) {
	h, f := newHandler()
	h.handle([]byte(`{"op":"regenerate-identity","items":["macAddresses"],"mac":"52:54:00:22:22:22"}`))
	var idxDown, idxAddr, idxUp = -1, -1, -1
	for i, c := range f.calls {
		switch {
		case c == "ip link set enp0s4 down":
			idxDown = i
		case c == "ip link set enp0s4 address 52:54:00:22:22:22":
			idxAddr = i
		case c == "ip link set enp0s4 up":
			idxUp = i
		}
	}
	if !(idxDown >= 0 && idxAddr > idxDown && idxUp > idxAddr) {
		t.Fatalf("mac change must be down->address->up; got down=%d addr=%d up=%d", idxDown, idxAddr, idxUp)
	}
}

func TestPrimaryIfaceFallbackToGlob(t *testing.T) {
	f := newFakeSys()
	// no default route -> fall back to the first non-virtual link with a device
	f.runErr[key("ip", "-o", "-4", "route", "show", "default")] = os.ErrNotExist
	f.globs["/sys/class/net/*"] = []string{"/sys/class/net/lo", "/sys/class/net/enp0s4"}
	f.files["/sys/class/net/enp0s4/device/uevent"] = "DRIVER=virtio_net"
	h := &handler{sys: f, version: "test"}
	ifc, err := h.primaryIface()
	if err != nil || ifc != "enp0s4" {
		t.Fatalf("fallback iface detect wrong: %q err=%v", ifc, err)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestRunOneShot_ErrorPaths covers the one-shot exec failure modes without needing
// root/chroot: an empty exec-root disables exec, and an empty argv is rejected —
// both return 127 (the shell "cannot run" convention).
func TestRunOneShot_ErrorPaths(t *testing.T) {
	// No exec-root -> buildExecCmd refuses (exec disabled) -> 127.
	if code := runOneShot("", "/work", []string{"/bin/true"}); code != 127 {
		t.Errorf("empty exec-root: code = %d, want 127", code)
	}
	// Empty argv -> buildExecCmd refuses -> 127.
	if code := runOneShot("/newroot", "/work", nil); code != 127 {
		t.Errorf("empty argv: code = %d, want 127", code)
	}
}

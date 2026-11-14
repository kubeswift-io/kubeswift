package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	passwd := "root:x:0:0:root:/root:/bin/sh\n# comment\nagent:x:1000:1000::/home/agent:/bin/sh\nnobody:x:65534:65534::/nonexistent:/sbin/nologin\nbroken:x:notanumber:1::/:/bin/sh\n"
	group := "root:x:0:\nagent:x:1000:\ndocker:x:999:agent,other\nwheel:x:10:root\nstaff:x:50:agent\n"
	for name, content := range map[string]string{"passwd": passwd, "group": group} {
		if err := os.WriteFile(filepath.Join(root, "etc", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveCreds(t *testing.T) {
	root := testRoot(t)
	for _, tc := range []struct {
		spec    string
		uid     uint32
		gid     uint32
		groups  []uint32
		home    string
		wantErr string
	}{
		{spec: "", uid: 0, gid: 0, groups: []uint32{0, 10}, home: "/root"},
		{spec: "agent", uid: 1000, gid: 1000, groups: []uint32{1000, 999, 50}, home: "/home/agent"},
		{spec: "1000", uid: 1000, gid: 1000, groups: []uint32{1000, 999, 50}, home: "/home/agent"},
		{spec: "agent:docker", uid: 1000, gid: 999, groups: []uint32{999, 50}, home: "/home/agent"},
		{spec: "1000:4242", uid: 1000, gid: 4242, groups: []uint32{4242, 999, 50}, home: "/home/agent"},
		{spec: "4242", uid: 4242, gid: 0, groups: []uint32{0}},
		{spec: "4242:7", uid: 4242, gid: 7, groups: []uint32{7}},
		{spec: "65534", uid: 65534, gid: 65534, groups: []uint32{65534}, home: "/nonexistent"},
		{spec: "ghost", wantErr: `user "ghost"`},
		{spec: "agent:ghosts", wantErr: `group "ghosts"`},
	} {
		c, err := resolveCreds(root, tc.spec)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: err=%v, want %q", tc.spec, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.spec, err)
			continue
		}
		if c.uid != tc.uid || c.gid != tc.gid || !slices.Equal(c.groups, tc.groups) || c.home != tc.home {
			t.Errorf("%q: got uid=%d gid=%d groups=%v home=%q, want %d %d %v %q", tc.spec, c.uid, c.gid, c.groups, c.home, tc.uid, tc.gid, tc.groups, tc.home)
		}
	}
}

// A distroless image has no /etc/passwd: a numeric user still works.
func TestResolveCredsNoPasswd(t *testing.T) {
	root := t.TempDir()
	c, err := resolveCreds(root, "65532")
	if err != nil || c.uid != 65532 || c.gid != 0 {
		t.Fatalf("got %+v err=%v", c, err)
	}
	if _, err := resolveCreds(root, "nonroot"); err == nil {
		t.Fatal("a user name resolved without /etc/passwd")
	}
}

func TestWorkloadCredsNonRoot(t *testing.T) {
	root := testRoot(t)
	if c, err := workloadCreds(root, identity{}); c != nil || err != nil {
		t.Fatalf("empty identity: %+v %v, want root with no creds", c, err)
	}
	if _, err := workloadCreds(root, identity{NonRoot: true}); err == nil {
		t.Fatal("runAsNonRoot with no user ran as root")
	}
	if _, err := workloadCreds(root, identity{User: "root", NonRoot: true}); err == nil {
		t.Fatal("runAsNonRoot with user root ran")
	}
	if c, err := workloadCreds(root, identity{User: "agent", NonRoot: true}); err != nil || c.uid != 1000 {
		t.Fatalf("agent: %+v %v", c, err)
	}
}

func TestWithHome(t *testing.T) {
	c := &creds{home: "/home/agent"}
	for _, tc := range []struct{ in, want []string }{
		{[]string{"PATH=/bin", "HOME=/"}, []string{"PATH=/bin", "HOME=/home/agent"}},
		{[]string{"PATH=/bin"}, []string{"PATH=/bin", "HOME=/home/agent"}},
		{[]string{"HOME=/", "HOME=/data"}, []string{"HOME=/", "HOME=/data"}},
	} {
		if got := withHome(tc.in, c); !slices.Equal(got, tc.want) {
			t.Errorf("withHome(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if got := withHome([]string{"HOME=/"}, nil); !slices.Equal(got, []string{"HOME=/"}) {
		t.Errorf("root: %v", got)
	}
}

// The checkout's workload sets the identity later execs run as; an exec that
// names its own identity keeps it.
func TestBuildExecCmdIdentity(t *testing.T) {
	root := testRoot(t)
	h := &handler{execRoot: root}
	cmd, plan, err := h.buildExecCmd(Request{Argv: []string{"/bin/true"}})
	if err != nil || plan.creds != nil || (cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil) {
		t.Fatalf("no identity: plan=%+v err=%v, want root", plan, err)
	}
	if _, _, err := h.buildExecCmd(Request{Argv: []string{"/bin/true"}, Workload: true, User: "agent", NoNewPrivs: true}); err != nil {
		t.Fatal(err)
	}
	cmd, plan, err = h.buildExecCmd(Request{Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if cr := cmd.SysProcAttr.Credential; cr == nil || cr.Uid != 1000 || cr.Gid != 1000 || !plan.nnp {
		t.Fatalf("exec after the workload: cred=%+v nnp=%v, want uid 1000 with no_new_privs", cr, plan.nnp)
	}
	if !slices.Contains(cmd.Env, "HOME=/home/agent") {
		t.Errorf("env %v has no HOME for the workload user", cmd.Env)
	}
	cmd, _, err = h.buildExecCmd(Request{Argv: []string{"/bin/true"}, User: "0"})
	if err != nil || cmd.SysProcAttr.Credential == nil || cmd.SysProcAttr.Credential.Uid != 0 {
		t.Fatalf("explicit root exec: %+v %v", cmd.SysProcAttr, err)
	}
	if _, _, err := h.buildExecCmd(Request{Argv: []string{"/bin/true"}, User: "ghost"}); err == nil {
		t.Fatal("an unknown user built a command")
	}
}

// The command started under startNoNewPrivs has no_new_privs; one started
// without does not inherit it from an earlier call.
func TestStartNoNewPrivs(t *testing.T) {
	read := func(nnp bool) string {
		cmd := exec.Command("/bin/grep", "NoNewPrivs", "/proc/self/status")
		var out strings.Builder
		cmd.Stdout = &out
		if err := startNoNewPrivs(nnp, cmd.Start); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(out.String()), " ")
	}
	if got := read(true); got != "NoNewPrivs: 1" {
		t.Fatalf("with no_new_privs: %q", got)
	}
	if got := read(false); got != "NoNewPrivs: 0" {
		t.Fatalf("after a no_new_privs start, a plain start has %q", got)
	}
}

func TestChownPaths(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := chownPaths(root, &creds{uid: 1234, gid: 5678}, []string{"/secret", ""}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != 1234 || st.Gid != 5678 {
		t.Fatalf("owner %d:%d, want 1234:5678", st.Uid, st.Gid)
	}
}

// The image's /etc/passwd is read as the workload would see it from its
// chroot: an absolute symlink resolves inside the root, a link that climbs
// with ".." stops at the root, and nothing outside the root is read.
func TestResolveCredsSymlinks(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "etc/static"), 0o755))
	must(os.WriteFile(filepath.Join(root, "etc/static/passwd"), []byte("agent:x:1000:1000::/home/agent:/bin/sh\n"), 0o644))
	must(os.WriteFile(filepath.Join(parent, "outside"), []byte("evil:x:0:0::/:/bin/sh\n"), 0o644))

	must(os.Symlink("/etc/static/passwd", filepath.Join(root, "etc/passwd")))
	if c, err := resolveCreds(root, "agent"); err != nil || c.uid != 1000 {
		t.Fatalf("absolute symlink: %+v %v", c, err)
	}

	must(os.Remove(filepath.Join(root, "etc/passwd")))
	must(os.Symlink("../../outside", filepath.Join(root, "etc/passwd")))
	if _, err := resolveCreds(root, "evil"); err == nil {
		t.Fatal("a symlink climbing out of the root read a passwd outside it")
	}

	must(os.Remove(filepath.Join(root, "etc/passwd")))
	must(os.Symlink("/etc/loop", filepath.Join(root, "etc/passwd")))
	must(os.Symlink("/etc/passwd", filepath.Join(root, "etc/loop")))
	if _, err := resolveCreds(root, "agent"); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("symlink loop: %v", err)
	}
}

// A FIFO as /etc/passwd is refused instead of blocking the agent.
func TestResolveCredsFIFO(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "etc/passwd"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := resolveCreds(root, "agent"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want not a regular file", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolveCreds blocked on a FIFO /etc/passwd")
	}
}

// A chown path through a symlinked directory stays inside the root, and the
// last element is never followed.
func TestResolveInRootChownPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var/run"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, tc := range []struct{ link, p, want string }{
		{"/var/run", "/run/secrets/token", "var/run/secrets/token"},
		{"../../../..", "/run/secrets/token", "secrets/token"},
		{"/etc/shadow", "/run", "run"}, // the last element itself: Lchown, not followed
	} {
		_ = os.Remove(filepath.Join(root, "run"))
		if err := os.Symlink(tc.link, filepath.Join(root, "run")); err != nil {
			t.Fatal(err)
		}
		if got, err := resolveInRoot(r, tc.p, false); err != nil || got != tc.want {
			t.Errorf("run -> %s: resolve %s = %q, %v; want %q", tc.link, tc.p, got, err, tc.want)
		}
	}
	if err := chownPaths(root, &creds{uid: 1, gid: 1}, []string{"/"}); err == nil {
		t.Error("chownPaths chowned the sandbox root")
	}
	if err := chownPaths(root, &creds{uid: 1 << 31, gid: 1}, []string{"/run"}); err == nil {
		t.Error("a uid above MaxInt32 reached Lchown")
	}
}

// A secret path whose directory the image made a symlink to a host path
// never chowns the host file.
func TestChownPathsSymlinkEscape(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	victim := filepath.Join(parent, "victim")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{parent, "../.."} {
		_ = os.Remove(filepath.Join(root, "sec"))
		if err := os.Symlink(link, filepath.Join(root, "sec")); err != nil {
			t.Fatal(err)
		}
		_ = chownPaths(root, &creds{uid: 4242, gid: 4242}, []string{"/sec/victim"})
		fi, err := os.Stat(victim)
		if err != nil {
			t.Fatal(err)
		}
		if st := fi.Sys().(*syscall.Stat_t); st.Uid == 4242 {
			t.Fatalf("sec -> %s: the host file outside the root was chowned", link)
		}
	}
}

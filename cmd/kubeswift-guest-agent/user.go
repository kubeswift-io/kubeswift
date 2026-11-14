package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// identity is who a sandbox workload runs as: the image USER or the
// sandbox's spec.securityContext, as the controller resolved it ("user",
// "uid", "user:group", "uid:gid"; empty = root), plus the two restrictions
// that go with it. Names are looked up in the sandbox root's /etc/passwd and
// /etc/group, as a container runtime does.
type identity struct {
	User       string
	NoNewPrivs bool
	NonRoot    bool
}

func (i identity) zero() bool { return i == identity{} }

// errIdentity marks an exec refused because its identity cannot be honoured.
var errIdentity = errors.New("cannot run as the workload user")

// creds is an identity resolved against the sandbox root.
type creds struct {
	uid, gid uint32
	groups   []uint32
	home     string
}

// defaultIdentity is what an exec without its own identity runs as: the
// workload's. The bridge sets it for a cold sandbox (--user), a checkout's
// workload request sets it for a warm slot.
type defaultIdentity struct {
	mu sync.Mutex
	id identity
}

func (d *defaultIdentity) get() identity {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.id
}

func (d *defaultIdentity) set(id identity) {
	d.mu.Lock()
	d.id = id
	d.mu.Unlock()
}

type passwdEntry struct {
	name     string
	uid, gid uint32
	home     string
}

type groupEntry struct {
	name    string
	gid     uint32
	members []string
}

// resolveCreds resolves spec against root's /etc/passwd and /etc/group. A
// numeric user need not exist in /etc/passwd (its group is then 0 unless the
// spec names one); a user or group name must.
func resolveCreds(root, spec string) (creds, error) {
	u, g, hasGroup := strings.Cut(spec, ":")
	if u == "" {
		u = "0"
	}
	users, err := readPasswd(filepath.Join(root, "etc/passwd"))
	if err != nil {
		return creds{}, err
	}
	var c creds
	var name string
	if n, err := strconv.ParseUint(u, 10, 32); err == nil {
		c.uid = uint32(n)
		for _, e := range users {
			if e.uid == c.uid {
				name, c.gid, c.home = e.name, e.gid, e.home
				break
			}
		}
	} else {
		i := slices.IndexFunc(users, func(e passwdEntry) bool { return e.name == u })
		if i < 0 {
			return creds{}, fmt.Errorf("user %q is not in the image's /etc/passwd", u)
		}
		e := users[i]
		name, c.uid, c.gid, c.home = e.name, e.uid, e.gid, e.home
	}
	groups, err := readGroup(filepath.Join(root, "etc/group"))
	if err != nil {
		return creds{}, err
	}
	if hasGroup && g != "" {
		if n, err := strconv.ParseUint(g, 10, 32); err == nil {
			c.gid = uint32(n)
		} else {
			i := slices.IndexFunc(groups, func(e groupEntry) bool { return e.name == g })
			if i < 0 {
				return creds{}, fmt.Errorf("group %q is not in the image's /etc/group", g)
			}
			c.gid = groups[i].gid
		}
	}
	// Supplementary groups: every group that lists the user as a member.
	c.groups = []uint32{c.gid}
	if name != "" {
		for _, e := range groups {
			if slices.Contains(e.members, name) && !slices.Contains(c.groups, e.gid) {
				c.groups = append(c.groups, e.gid)
			}
		}
	}
	return c, nil
}

// readPasswd parses an /etc/passwd; a missing file is an empty one.
func readPasswd(path string) ([]passwdEntry, error) {
	var out []passwdEntry
	err := eachLine(path, func(f []string) {
		if len(f) < 6 {
			return
		}
		uid, err1 := strconv.ParseUint(f[2], 10, 32)
		gid, err2 := strconv.ParseUint(f[3], 10, 32)
		if err1 != nil || err2 != nil {
			return
		}
		out = append(out, passwdEntry{name: f[0], uid: uint32(uid), gid: uint32(gid), home: f[5]})
	})
	return out, err
}

// readGroup parses an /etc/group; a missing file is an empty one.
func readGroup(path string) ([]groupEntry, error) {
	var out []groupEntry
	err := eachLine(path, func(f []string) {
		if len(f) < 3 {
			return
		}
		gid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			return
		}
		e := groupEntry{name: f[0], gid: uint32(gid)}
		if len(f) > 3 && f[3] != "" {
			e.members = strings.Split(f[3], ",")
		}
		out = append(out, e)
	})
	return out, err
}

func eachLine(path string, fn func([]string)) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fn(strings.Split(line, ":"))
	}
	return s.Err()
}

// workloadCreds resolves id for an exec in root. nil means run as the agent
// does (root), which is what an identity with no user and no restriction is.
func workloadCreds(root string, id identity) (*creds, error) {
	if id.User == "" && !id.NonRoot {
		return nil, nil
	}
	c, err := resolveCreds(root, id.User)
	if err != nil {
		return nil, err
	}
	if id.NonRoot && c.uid == 0 {
		return nil, fmt.Errorf("runAsNonRoot is set but user %q is uid 0", userOrRoot(id.User))
	}
	return &c, nil
}

func userOrRoot(u string) string {
	if u == "" {
		return "root"
	}
	return u
}

// withHome sets HOME to the user's home directory when the environment does
// not set it or leaves it at the agent's default "/".
func withHome(env []string, c *creds) []string {
	if c == nil || c.home == "" {
		return env
	}
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "HOME="); ok {
			if v != "/" && v != "" {
				return env
			}
			out := slices.Clone(env)
			out[i] = "HOME=" + c.home
			return out
		}
	}
	return append(slices.Clone(env), "HOME="+c.home)
}

// startNoNewPrivs runs start (which forks the command) on an OS thread that
// has no_new_privs set, so the command inherits it: setuid binaries and file
// capabilities then grant nothing. Go has no SysProcAttr field for it, and the
// flag is per thread, so the thread is locked and never unlocked; it exits
// with this goroutine instead of returning to the scheduler carrying the flag.
func startNoNewPrivs(nnp bool, start func() error) error {
	if !nnp {
		return start()
	}
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			errc <- fmt.Errorf("set no_new_privs: %w", err)
			return
		}
		errc <- start()
	}()
	return <-errc
}

// chownPaths gives the workload user the files and directories the platform
// put in place for it (secret files, output shares). Only the paths named,
// never recursively.
func chownPaths(root string, c *creds, paths []string) error {
	if c == nil {
		return nil
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Lchown(filepath.Join(root, p), int(c.uid), int(c.gid)); err != nil {
			return fmt.Errorf("give %s to uid %d: %w", p, c.uid, err)
		}
	}
	return nil
}

package main

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
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

// openRoot opens the sandbox root (the agent's own root when empty). The
// root holds an untrusted image, so every file it names is opened through the
// os.Root: a symlink in the image resolves inside it, never on the agent's
// side of the chroot.
func openRoot(root string) (*os.Root, error) {
	if root == "" {
		root = "/"
	}
	return os.OpenRoot(root)
}

// maxSymlinks bounds symlink resolution, as the kernel's ELOOP does.
const maxSymlinks = 40

// resolveInRoot resolves p as the workload sees it from inside its chroot:
// an absolute symlink target is taken from the sandbox root and ".." stops
// there. os.Root alone refuses an absolute symlink as escaping, which would
// break images that use them (/etc/passwd -> /etc/static/passwd). With
// followLast false the last element is not resolved (for Lchown). The result
// has no symlinks left and is still opened through r, so a link swapped after
// resolving can at worst fail the open, never leave the root.
func resolveInRoot(r *os.Root, p string, followLast bool) (string, error) {
	todo := strings.Split(strings.Trim(path.Clean("/"+p), "/"), "/")
	done := ""
	hops := 0
	for len(todo) > 0 {
		elem := todo[0]
		todo = todo[1:]
		if elem == "" || elem == "." {
			continue
		}
		if elem == ".." {
			done = strings.TrimPrefix(path.Dir("/"+done), "/")
			continue
		}
		next := path.Join(done, elem)
		if len(todo) == 0 && !followLast {
			return next, nil
		}
		fi, err := r.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			// Missing, or not a link: the caller's open reports it.
			done = next
			continue
		}
		if hops++; hops > maxSymlinks {
			return "", fmt.Errorf("/%s: too many levels of symbolic links", p)
		}
		target, err := r.Readlink(next)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(target, "/") {
			done = ""
		}
		todo = append(strings.Split(target, "/"), todo...)
	}
	if done == "" {
		return ".", nil
	}
	return done, nil
}

// resolveCreds resolves spec against root's /etc/passwd and /etc/group. A
// numeric user need not exist in /etc/passwd (its group is then 0 unless the
// spec names one); a user or group name must.
func resolveCreds(root, spec string) (creds, error) {
	u, g, hasGroup := strings.Cut(spec, ":")
	if u == "" {
		u = "0"
	}
	r, err := openRoot(root)
	if err != nil {
		return creds{}, err
	}
	defer r.Close()
	users, err := readPasswd(r, "etc/passwd")
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
	groups, err := readGroup(r, "etc/group")
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
func readPasswd(r *os.Root, name string) ([]passwdEntry, error) {
	var out []passwdEntry
	err := eachLine(r, name, func(f []string) {
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
func readGroup(r *os.Root, name string) ([]groupEntry, error) {
	var out []groupEntry
	err := eachLine(r, name, func(f []string) {
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

// eachLine calls fn with the fields of each line of name in r. Only a regular
// file is read: it is opened non-blocking, so an image whose /etc/passwd is a
// FIFO cannot hang the agent.
func eachLine(r *os.Root, name string, fn func([]string)) error {
	resolved, err := resolveInRoot(r, name, true)
	if err != nil {
		return err
	}
	f, err := r.OpenFile(resolved, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("/%s in the image is not a regular file", name)
	}
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
// never recursively, and through the sandbox root: a symlinked directory in
// the image cannot point the chown outside it.
func chownPaths(root string, c *creds, paths []string) error {
	if c == nil {
		return nil
	}
	// The ids reach Lchown as int; on a 32-bit agent a larger id would wrap.
	if c.uid > math.MaxInt32 || c.gid > math.MaxInt32 {
		return fmt.Errorf("uid %d / gid %d is out of range for chown", c.uid, c.gid)
	}
	uid, gid := int(c.uid), int(c.gid)
	r, err := openRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, p := range paths {
		if p == "" {
			continue
		}
		name, err := resolveInRoot(r, p, false)
		if err == nil && name == "." {
			err = errors.New("refusing to chown the sandbox root")
		}
		if err == nil {
			err = r.Lchown(name, uid, gid)
		}
		if err != nil {
			return fmt.Errorf("give %s to uid %d: %w", p, c.uid, err)
		}
	}
	return nil
}

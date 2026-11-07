package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The block-in-use op answers whether the guest still uses a disk, before
// the host unplugs it (a hot-plugged volume being detached). Cloud
// Hypervisor and the guest kernel both let a mounted disk be yanked; this is
// what lets the host refuse instead. Read-only: it looks at /sys and /proc
// and changes nothing.
//
// A disk is found by its virtio serial (/sys/block/<dev>/serial), the one the
// host gave it, and counts as in use when it or any of its partitions:
//   - backs a mount (major:minor in /proc/self/mountinfo),
//   - is swap (/proc/swaps),
//   - has holders: a device-mapper, md or other device stacked on it,
//   - is held open by a process (/proc/<pid>/fd).

// serialRE: a virtio-blk serial is at most 20 bytes; KubeSwift's are "ks"
// and hex digits.
var serialRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,20}$`)

// BlockUse is the block-in-use reply's payload, flattened into Response.
type BlockUse struct {
	Present bool   `json:"present"`
	InUse   bool   `json:"inUse"`
	Detail  string `json:"detail,omitempty"`
}

// blockInUse inspects the system under root ("" for the real one).
func blockInUse(root, serial string) (BlockUse, error) {
	if !serialRE.MatchString(serial) {
		return BlockUse{}, fmt.Errorf("invalid serial %q", serial)
	}
	disk, err := diskBySerial(root, serial)
	if err != nil || disk == "" {
		return BlockUse{}, err
	}
	use := BlockUse{Present: true}

	// The disk and its partitions, by name and major:minor.
	names := []string{disk}
	parts, _ := filepath.Glob(filepath.Join(root, "/sys/block", disk, disk+"*"))
	for _, p := range parts {
		if _, err := os.Stat(filepath.Join(p, "partition")); err == nil {
			names = append(names, filepath.Base(p))
		}
	}
	devs := map[string]string{} // major:minor -> name
	for _, n := range names {
		if b, err := os.ReadFile(filepath.Join(root, "/sys/class/block", n, "dev")); err == nil {
			devs[strings.TrimSpace(string(b))] = n
		}
	}

	var uses []string
	if mi, err := os.ReadFile(filepath.Join(root, "/proc/self/mountinfo")); err == nil {
		for _, line := range strings.Split(string(mi), "\n") {
			f := strings.Fields(line)
			if len(f) > 4 {
				if n, ok := devs[f[2]]; ok {
					uses = append(uses, fmt.Sprintf("/dev/%s mounted at %s", n, f[4]))
				}
			}
		}
	}
	if sw, err := os.ReadFile(filepath.Join(root, "/proc/swaps")); err == nil {
		for _, line := range strings.Split(string(sw), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) > 0 && isOneOf(strings.TrimPrefix(f[0], "/dev/"), names) {
				uses = append(uses, fmt.Sprintf("%s is swap", f[0]))
			}
		}
	}
	for _, n := range names {
		holders, _ := os.ReadDir(filepath.Join(root, "/sys/class/block", n, "holders"))
		for _, h := range holders {
			uses = append(uses, fmt.Sprintf("/dev/%s held by %s", n, h.Name()))
		}
	}
	uses = append(uses, openBy(root, names)...)

	if len(uses) > 0 {
		use.InUse = true
		sort.Strings(uses)
		use.Detail = strings.Join(uses, "; ")
	}
	return use, nil
}

// diskBySerial returns the name of the whole disk with that virtio serial, or
// "" when the guest has none.
func diskBySerial(root, serial string) (string, error) {
	files, err := filepath.Glob(filepath.Join(root, "/sys/block/*/serial"))
	if err != nil {
		return "", err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err == nil && strings.TrimSpace(string(b)) == serial {
			return filepath.Base(filepath.Dir(f)), nil
		}
	}
	return "", nil
}

// openBy lists the processes, other than this agent, holding one of the
// devices open.
func openBy(root string, names []string) []string {
	paths := map[string]bool{}
	for _, n := range names {
		paths["/dev/"+n] = true
	}
	self := strconv.Itoa(os.Getpid())
	fds, _ := filepath.Glob(filepath.Join(root, "/proc/[0-9]*/fd/*"))
	seen := map[string]bool{}
	var out []string
	for _, fd := range fds {
		target, err := os.Readlink(fd)
		if err != nil || !paths[target] {
			continue
		}
		pid := filepath.Base(filepath.Dir(filepath.Dir(fd)))
		if pid == self || seen[pid+target] {
			continue
		}
		seen[pid+target] = true
		comm, _ := os.ReadFile(filepath.Join(root, "/proc", pid, "comm"))
		out = append(out, fmt.Sprintf("%s open by pid %s (%s)", target, pid, strings.TrimSpace(string(comm))))
	}
	return out
}

func isOneOf(s string, set []string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Checkout-time read-only mounts (bridge feature warm-mounts).
//
// A warm-pool slot boots with an empty read-only virtio-fs share mounted at
// stageDir (kubeswift.stage). On a checkout the host projects each artifact
// into it as a directory named after the artifact; the exec request then
// lists {name, path}, and the agent binds stageDir/name read-only at path in
// the exec root before the workload runs. The workload sees only its own
// artifacts, at its own paths, never the share.

// DefaultStageDir is where the bridge mounts the staging share.
const DefaultStageDir = "/run/kubeswift-stage"

// ExecMount is one staged directory to bind read-only into the exec root.
type ExecMount struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// mountNameRE is an artifact name: a DNS label of at most 40 characters.
var mountNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// bindReadOnly binds source at target and makes the bind read-only, nosuid
// and nodev. A package var so tests run without root.
var bindReadOnly = func(source, target string) error {
	if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
		return err
	}
	flags := uintptr(syscall.MS_REMOUNT | syscall.MS_BIND | syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV)
	if err := syscall.Mount("", target, "", flags, ""); err != nil {
		_ = syscall.Unmount(target, syscall.MNT_DETACH)
		return err
	}
	return nil
}

// validMountPath is the bridge's rule for a mount path: clean, absolute, not
// the root, not under /proc, /sys or /dev.
func validMountPath(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
		return false
	}
	for _, reserved := range []string{"/proc", "/sys", "/dev"} {
		if p == reserved || strings.HasPrefix(p, reserved+"/") {
			return false
		}
	}
	return true
}

// mountExecMounts binds each staged artifact read-only at its path in root.
// Every entry is checked before anything is mounted, so a bad request mounts
// nothing.
func mountExecMounts(root, stageDir string, mounts []ExecMount) error {
	seen := map[string]bool{}
	for _, m := range mounts {
		if !mountNameRE.MatchString(m.Name) {
			return fmt.Errorf("exec: mount name %q is not a DNS label", m.Name)
		}
		if !validMountPath(m.Path) {
			return fmt.Errorf("exec: mount %s: path %q is not a clean absolute path outside /proc, /sys and /dev", m.Name, m.Path)
		}
		if seen[m.Path] {
			return fmt.Errorf("exec: mount path %s listed twice", m.Path)
		}
		seen[m.Path] = true
		fi, err := os.Stat(filepath.Join(stageDir, m.Name))
		if err != nil || !fi.IsDir() {
			return fmt.Errorf("exec: artifact %s is not staged", m.Name)
		}
	}
	for _, m := range mounts {
		target := filepath.Join(root, m.Path)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("exec: mount %s at %s: %w", m.Name, m.Path, err)
		}
		if err := bindReadOnly(filepath.Join(stageDir, m.Name), target); err != nil {
			return fmt.Errorf("exec: mount %s at %s: %w", m.Name, m.Path, err)
		}
	}
	return nil
}

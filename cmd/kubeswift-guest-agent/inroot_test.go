package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// hostileRoot is a sandbox root whose image tries to point platform writes
// outside it: /hostile is an absolute link to a directory outside the root
// (as "/hostile -> /" would be for the bridge), /up climbs out with "..", and
// /run/secrets/token is a link to a file outside the root.
func hostileRoot(t *testing.T) (root, outside string) {
	t.Helper()
	root, outside = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim"), []byte("bridge"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "run/secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"hostile":           outside,
		"up":                "../../../../../../..",
		"run/secrets/token": filepath.Join(outside, "victim"),
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

// untouched fails the test if anything was created or changed outside root.
func untouched(t *testing.T, outside string) {
	t.Helper()
	ents, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "victim" {
		t.Errorf("outside the root now holds %v", ents)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim")); string(b) != "bridge" {
		t.Errorf("file outside the root rewritten: %q", b)
	}
}

func TestWriteFileInRoot_StaysInRoot(t *testing.T) {
	root, outside := hostileRoot(t)
	for _, p := range []string{"/hostile/kstoken", "/up/kstoken2", "/run/secrets/token"} {
		if err := writeFileInRoot(root, p, strings.NewReader("s3cret"), 0o400); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	untouched(t, outside)
	// The absolute link resolves from the sandbox root, as the workload sees it.
	if b, err := os.ReadFile(filepath.Join(root, outside, "kstoken")); err != nil || string(b) != "s3cret" {
		t.Errorf("/hostile/kstoken: %q, %v", b, err)
	}
	// ".." stops at the sandbox root.
	if _, err := os.Stat(filepath.Join(root, "kstoken2")); err != nil {
		t.Errorf("/up/kstoken2 not at the root: %v", err)
	}
	// A link at the file itself is replaced by the file, not followed.
	fi, err := os.Lstat(filepath.Join(root, "run/secrets/token"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o400 {
		t.Errorf("/run/secrets/token: %v, %v", fi, err)
	}
}

func TestWriteFileInRoot_Refuses(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/fifo", "/dir", "/", "relative", "/a/../b"} {
		if err := writeFileInRoot(root, p, strings.NewReader("x"), 0o400); err == nil {
			t.Errorf("%q: written, want a refusal", p)
		}
	}
}

func TestMkdirInRoot_StaysInRoot(t *testing.T) {
	root, outside := hostileRoot(t)
	got, err := mkdirInRoot(root, "/hostile/opt/app", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, outside, "opt/app"); got != want {
		t.Errorf("mount point = %s, want %s", got, want)
	}
	if got, err := mkdirInRoot(root, "/up/srv", 0o755); err != nil || got != filepath.Join(root, "srv") {
		t.Errorf("/up/srv = %s, %v", got, err)
	}
	if err := os.Symlink("/", filepath.Join(root, "proc")); err != nil {
		t.Fatal(err)
	}
	if _, err := mkdirInRoot(root, "/proc", 0o755); err == nil {
		t.Error("/proc -> / became a mount point over the whole sandbox root")
	}
	untouched(t, outside)
	// The link to a file outside the root names a path inside it.
	if got, err := mkdirInRoot(root, "/run/secrets/token", 0o755); err != nil || got != filepath.Join(root, outside, "victim") {
		t.Errorf("/run/secrets/token = %s, %v", got, err)
	}
	untouched(t, outside)
}

// A warm checkout's secret files and artifact mounts go through the same
// resolution.
func TestWarmPathStaysInRoot(t *testing.T) {
	root, outside := hostileRoot(t)
	if err := writeExecFiles(root, []ExecFile{{Path: "/hostile/kstoken", Data: []byte("t")}}); err != nil {
		t.Fatal(err)
	}
	binds := recordBinds(t)
	st := stage(t, "app")
	if err := mountExecMounts(root, st, []ExecMount{{Name: "app", Path: "/hostile/app"}}); err != nil {
		t.Fatal(err)
	}
	untouched(t, outside)
	if want := filepath.Join(root, outside, "app"); len(*binds) != 1 || (*binds)[0][1] != want {
		t.Errorf("binds = %v, want target %s", *binds, want)
	}
}

func TestPathOneShot(t *testing.T) {
	root, outside := hostileRoot(t)
	if code := pathOneShot(root, true, "", []string{"/hostile/m"}); code != 0 {
		t.Errorf("--mkdir: code %d", code)
	}
	if code := pathOneShot(root, false, "0644", []string{"/etc/resolv.conf"}); code != 0 {
		t.Errorf("--write-file: code %d", code)
	}
	if fi, err := os.Stat(filepath.Join(root, "etc/resolv.conf")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("resolv.conf: %v, %v", fi, err)
	}
	for _, bad := range [][]string{nil, {"/a", "/b"}} {
		if code := pathOneShot(root, true, "", bad); code != 1 {
			t.Errorf("%v: code %d, want 1", bad, code)
		}
	}
	if code := pathOneShot(root, false, "0999", []string{"/x"}); code != 1 {
		t.Errorf("bad mode: code %d, want 1", code)
	}
	untouched(t, outside)
}

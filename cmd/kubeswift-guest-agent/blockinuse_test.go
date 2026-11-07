package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDisk builds /sys entries for vdc (serial ks1, 252:32) with one
// partition vdc1 (252:33) under root.
func fakeDisk(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(p, data string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sys/block/vda/serial", "root\n")
	write("sys/block/vdc/serial", "ks1\n")
	write("sys/block/vdc/vdc1/partition", "1\n")
	write("sys/class/block/vdc/dev", "252:32\n")
	write("sys/class/block/vdc1/dev", "252:33\n")
	write("proc/self/mountinfo", "22 1 252:1 / / rw - ext4 /dev/vda1 rw\n")
	write("proc/swaps", "Filename Type Size Used Priority\n")
	return root
}

func TestBlockInUse(t *testing.T) {
	root := fakeDisk(t)
	if u, err := blockInUse(root, "nope"); err != nil || u.Present {
		t.Fatalf("missing disk: %+v %v", u, err)
	}
	if u, err := blockInUse(root, "ks1"); err != nil || !u.Present || u.InUse {
		t.Fatalf("free disk: %+v %v", u, err)
	}

	mi := filepath.Join(root, "proc/self/mountinfo")
	_ = os.WriteFile(mi, []byte("22 1 252:1 / / rw - ext4 /dev/vda1 rw\n30 22 252:33 / /srv rw - ext4 /dev/vdc1 rw\n"), 0o644)
	if u, _ := blockInUse(root, "ks1"); !u.InUse || !strings.Contains(u.Detail, "/dev/vdc1 mounted at /srv") {
		t.Fatalf("mounted partition: %+v", u)
	}
	_ = os.WriteFile(mi, []byte(""), 0o644)

	_ = os.MkdirAll(filepath.Join(root, "sys/class/block/vdc/holders/dm-0"), 0o755)
	if u, _ := blockInUse(root, "ks1"); !u.InUse || !strings.Contains(u.Detail, "held by dm-0") {
		t.Fatalf("holder: %+v", u)
	}
	_ = os.RemoveAll(filepath.Join(root, "sys/class/block/vdc/holders"))

	_ = os.MkdirAll(filepath.Join(root, "proc/4242/fd"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "proc/4242/comm"), []byte("postgres\n"), 0o644)
	_ = os.Symlink("/dev/vdc", filepath.Join(root, "proc/4242/fd/7"))
	if u, _ := blockInUse(root, "ks1"); !u.InUse || !strings.Contains(u.Detail, "pid 4242 (postgres)") {
		t.Fatalf("open: %+v", u)
	}
	_ = os.RemoveAll(filepath.Join(root, "proc/4242"))

	_ = os.WriteFile(filepath.Join(root, "proc/swaps"), []byte("Filename Type Size Used Priority\n/dev/vdc1 partition 1 0 -2\n"), 0o644)
	if u, _ := blockInUse(root, "ks1"); !u.InUse || !strings.Contains(u.Detail, "is swap") {
		t.Fatalf("swap: %+v", u)
	}

	if _, err := blockInUse(root, "../../etc"); err == nil {
		t.Fatal("serial is validated")
	}
}

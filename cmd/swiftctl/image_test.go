package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestLooksLikeQCOW2(t *testing.T) {
	dir := t.TempDir()

	qcow := filepath.Join(dir, "d.qcow2")
	if err := os.WriteFile(qcow, append([]byte{0x51, 0x46, 0x49, 0xfb}, make([]byte, 60)...), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(dir, "d.raw")
	if err := os.WriteFile(raw, []byte("this is not a qcow2 header, just raw bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte{0x51, 0x46}, 0o644); err != nil { // shorter than the magic
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path string
		want bool
	}{
		{qcow, true},
		{raw, false},
		{short, false},
	} {
		got, err := looksLikeQCOW2(tc.path)
		if err != nil {
			t.Fatalf("looksLikeQCOW2(%s): %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("looksLikeQCOW2(%s) = %v, want %v", filepath.Base(tc.path), got, tc.want)
		}
	}
}

// A raw input is used in place (no conversion, cleanup is a no-op).
func TestEnsureRaw_RawPassthrough(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "golden.raw")
	if err := os.WriteFile(raw, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	got, cleanup, err := ensureRaw(&cobra.Command{}, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got != raw {
		t.Errorf("raw input must be used in place; got %q want %q", got, raw)
	}
	// cleanup must not remove the operator's own raw file.
	cleanup()
	if _, err := os.Stat(raw); err != nil {
		t.Errorf("cleanup must not delete a raw input file: %v", err)
	}
}

// ensureRaw on a qcow2 input: converts when qemu-img is present, else fails with
// actionable guidance. Environment-adaptive so it is deterministic either way.
func TestEnsureRaw_QCOW2(t *testing.T) {
	dir := t.TempDir()
	cmd := &cobra.Command{}

	if qemuImg, err := exec.LookPath("qemu-img"); err == nil {
		// Real qcow2 -> the converter yields an existing raw temp file.
		qcow := filepath.Join(dir, "real.qcow2")
		if out, err := exec.Command(qemuImg, "create", "-f", "qcow2", qcow, "1M").CombinedOutput(); err != nil {
			t.Fatalf("qemu-img create: %v (%s)", err, out)
		}
		got, cleanup, err := ensureRaw(cmd, qcow)
		if err != nil {
			t.Fatalf("ensureRaw on a real qcow2: %v", err)
		}
		if got == qcow {
			t.Fatal("a qcow2 input must be converted to a different raw path")
		}
		if fi, serr := os.Stat(got); serr != nil {
			t.Fatalf("converted raw missing: %v", serr)
		} else if fi.Size() != 1<<20 {
			t.Errorf("converted raw size = %d, want %d (qemu-img -O raw is the full virtual size)", fi.Size(), 1<<20)
		}
		cleanup() // default: removes the temp raw
		if _, serr := os.Stat(got); !os.IsNotExist(serr) {
			t.Errorf("cleanup must remove the temporary converted raw; stat err=%v", serr)
		}
		return
	}

	// qemu-img absent: a qcow2 input fails loudly with the manual command.
	qcow := filepath.Join(dir, "fake.qcow2")
	if err := os.WriteFile(qcow, append([]byte{0x51, 0x46, 0x49, 0xfb}, make([]byte, 60)...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := ensureRaw(cmd, qcow)
	if err == nil {
		t.Fatal("qcow2 input without qemu-img must error")
	}
	if !strings.Contains(err.Error(), "qemu-img") {
		t.Errorf("error should mention qemu-img and how to convert; got: %v", err)
	}
}

func TestHumanizeBytes(t *testing.T) {
	for _, tc := range []struct {
		b    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{1073741824, "1.00 GiB"},
	} {
		if got := humanizeBytes(tc.b); got != tc.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", tc.b, got, tc.want)
		}
	}
}

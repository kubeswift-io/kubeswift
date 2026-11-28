package nodefile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInstallReplacesTheInode(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "memory-ranges")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	f, err := Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !IsPart(filepath.Base(f.Name())) || filepath.Dir(f.Name()) != dir {
		t.Fatalf("part file %s is not a part file beside dst", f.Name())
	}
	if _, err := f.WriteString("new"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Install(f.Name(), dst); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 8)
	n, _ := held.Read(buf)
	if string(buf[:n]) != "old" {
		t.Fatalf("held file reads %q, want old", buf[:n])
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "new" {
		t.Fatalf("dst = %q, want new", b)
	}
	if st, _ := os.Stat(dst); st.Mode().Perm() != 0o644 {
		t.Fatalf("dst mode %v, want 0644", st.Mode().Perm())
	}
}

func TestRemoveStalePartsKeepsRecentOnes(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, PartPrefix+"old-1")
	recent := filepath.Join(dir, PartPrefix+"pull-2")
	keep := filepath.Join(dir, "memory-ranges")
	for _, p := range []string{old, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(recent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recent, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-StaleAfter - time.Minute)
	for _, p := range []string{old, keep, recent} {
		if err := os.Chtimes(p, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	// recent's file was just written: a download is still active in it.
	if err := RemoveStaleParts(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale part file was kept")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("an active part directory was removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("a real artifact was removed")
	}
	if err := RemoveStaleParts(filepath.Join(dir, "missing"), time.Now()); err != nil {
		t.Errorf("missing dir: %v", err)
	}
}

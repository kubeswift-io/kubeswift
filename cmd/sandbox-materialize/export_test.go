package main

import (
	"os"
	"path/filepath"
	"testing"
)

// clean-outputs removes exactly one sandbox's directory, named by a UID.
func TestCleanOutputs(t *testing.T) {
	root := t.TempDir()
	uid := "0a1b2c3d-0000-4000-8000-000000000001"
	other := "0a1b2c3d-0000-4000-8000-000000000002"
	for _, d := range []string{uid, other} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "out.tar"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"", "..", "../x", uid + "/..", "a/b"} {
		if code := cleanOutputs([]string{"--root=" + root, "--sandbox-uid=" + bad}); code != 2 {
			t.Errorf("%q: exit %d, want 2", bad, code)
		}
	}
	if code := cleanOutputs([]string{"--root=" + root, "--sandbox-uid=" + uid}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(root, uid)); !os.IsNotExist(err) {
		t.Errorf("%s kept: %v", uid, err)
	}
	if _, err := os.Stat(filepath.Join(root, other, "out.tar")); err != nil {
		t.Errorf("another sandbox's output was touched: %v", err)
	}
}

package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A key from the environment lands in a file cosign can read.
func TestWriteVerifyKey(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	path, err := writeVerifyKey("PEM")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "PEM" {
		t.Errorf("key file holds %q", b)
	}
}

// Verification cannot run against a plaintext registry; that fails the pull
// instead of quietly skipping the check. Without a key nothing is verified.
func TestVerified(t *testing.T) {
	a := runArgs{repository: "reg.example/snap", verifyKey: "/k.pub", insecure: true}
	if _, err := a.verified(context.Background(), nil, "t"); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("insecure + verify: err = %v", err)
	}
	a.verifyKey = ""
	if ref, err := a.verified(context.Background(), nil, "t"); err != nil || ref != "t" {
		t.Errorf("no key: ref %q err %v", ref, err)
	}
}

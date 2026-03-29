package runtimeintent

import (
	"encoding/json"
	"testing"
)

type mockResolvedGuest struct {
	hasSeed       bool
	hasKernel     bool
	format        string
	cpu           int
	memory        int
	lifecycle     string
	guestID       string
	kernelPath    string
	initramfsPath string
	kernelCmdline string
}

func (m *mockResolvedGuest) HasSeed() bool             { return m.hasSeed }
func (m *mockResolvedGuest) HasKernel() bool           { return m.hasKernel }
func (m *mockResolvedGuest) GetRootDiskFormat() string { return m.format }
func (m *mockResolvedGuest) GetCPU() int               { return m.cpu }
func (m *mockResolvedGuest) GetMemoryMiB() int         { return m.memory }
func (m *mockResolvedGuest) GetLifecycle() string      { return m.lifecycle }
func (m *mockResolvedGuest) GetGuestID() string        { return m.guestID }
func (m *mockResolvedGuest) GetKernelPath() string     { return m.kernelPath }
func (m *mockResolvedGuest) GetInitramfsPath() string  { return m.initramfsPath }
func (m *mockResolvedGuest) GetKernelCmdline() string  { return m.kernelCmdline }

func TestBuild(t *testing.T) {
	rg := &mockResolvedGuest{
		hasSeed:   true,
		format:    "raw",
		cpu:       2,
		memory:    2048,
		lifecycle: "start",
		guestID:   "test-guest",
	}
	intent := Build(rg)
	wantPath := DisksRootPath + "/" + RootDiskImageFile
	if intent.RootDisk.Path != wantPath {
		t.Errorf("rootDisk.path = %q, want %q", intent.RootDisk.Path, wantPath)
	}
	if intent.RootDisk.Format != "raw" {
		t.Errorf("rootDisk.format = %q, want raw", intent.RootDisk.Format)
	}
	if intent.SeedPath != SeedPath {
		t.Errorf("seedPath = %q, want %q", intent.SeedPath, SeedPath)
	}
	if intent.CPU != 2 || intent.Memory != 2048 {
		t.Errorf("cpu=%d memory=%d, want 2 2048", intent.CPU, intent.Memory)
	}
	if intent.GuestID != "test-guest" {
		t.Errorf("guestId = %q, want test-guest", intent.GuestID)
	}
	if !intent.Network {
		t.Error("network = false, want true when HasSeed")
	}
}

func TestBuildNoSeed(t *testing.T) {
	rg := &mockResolvedGuest{hasSeed: false, format: "raw", guestID: "x"}
	intent := Build(rg)
	if intent.SeedPath != "" {
		t.Errorf("seedPath = %q, want empty", intent.SeedPath)
	}
	if intent.Network {
		t.Error("network = true, want false when no seed")
	}
}

func TestSerializeParseRoundtrip(t *testing.T) {
	intent := &RuntimeIntent{
		RootDisk:  RootDiskSpec{Path: DisksRootPath + "/" + RootDiskImageFile, Format: "raw"},
		SeedPath:  SeedPath,
		CPU:       2,
		Memory:    2048,
		Lifecycle: "start",
		GuestID:   "test",
		Network:   true,
	}
	data, err := Serialize(intent)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	var parsed RuntimeIntent
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.RootDisk.Path != intent.RootDisk.Path {
		t.Errorf("parsed rootDisk.path = %q", parsed.RootDisk.Path)
	}
	if parsed.SeedPath != intent.SeedPath {
		t.Errorf("parsed seedPath = %q", parsed.SeedPath)
	}
}

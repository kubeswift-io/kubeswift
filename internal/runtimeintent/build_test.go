package runtimeintent

import (
	"encoding/json"
	"testing"
)

type mockResolvedGuest struct {
	hasSeed       bool
	hasKernel     bool
	hasNetwork    bool
	hasDataDisk   bool
	format        string
	cpu           int
	memory        int
	lifecycle     string
	guestID       string
	kernelPath    string
	initramfsPath string
	kernelCmdline string
	hypervisor    string
}

func (m *mockResolvedGuest) HasSeed() bool             { return m.hasSeed }
func (m *mockResolvedGuest) HasKernel() bool           { return m.hasKernel }
func (m *mockResolvedGuest) HasNetwork() bool          { return m.hasNetwork }
func (m *mockResolvedGuest) HasDataDisk() bool         { return m.hasDataDisk }
func (m *mockResolvedGuest) GetRootDiskFormat() string { return m.format }
func (m *mockResolvedGuest) GetCPU() int               { return m.cpu }
func (m *mockResolvedGuest) GetMemoryMiB() int         { return m.memory }
func (m *mockResolvedGuest) GetLifecycle() string      { return m.lifecycle }
func (m *mockResolvedGuest) GetGuestID() string        { return m.guestID }
func (m *mockResolvedGuest) GetKernelPath() string     { return m.kernelPath }
func (m *mockResolvedGuest) GetInitramfsPath() string  { return m.initramfsPath }
func (m *mockResolvedGuest) GetKernelCmdline() string  { return m.kernelCmdline }
func (m *mockResolvedGuest) GetHypervisor() string     { return m.hypervisor }
func (m *mockResolvedGuest) GetNICs() []NICIntent      { return nil }

func TestBuild(t *testing.T) {
	rg := &mockResolvedGuest{
		hasSeed:    true,
		hasNetwork: true,
		format:     "raw",
		cpu:        2,
		memory:     2048,
		lifecycle:  "start",
		guestID:    "test-guest",
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
		t.Error("network = false, want true when HasNetwork")
	}
}

func TestBuildNoSeed(t *testing.T) {
	rg := &mockResolvedGuest{hasSeed: false, hasNetwork: false, format: "raw", guestID: "x"}
	intent := Build(rg)
	if intent.SeedPath != "" {
		t.Errorf("seedPath = %q, want empty", intent.SeedPath)
	}
	if intent.Network {
		t.Error("network = true, want false when no network")
	}
}

func TestBuild_WithHypervisorQEMU(t *testing.T) {
	rg := &mockResolvedGuest{
		hasSeed:    true,
		hasNetwork: true,
		format:     "raw",
		cpu:        16,
		memory:     32768,
		lifecycle:  "start",
		guestID:    "default/gpu-test",
		hypervisor: "qemu",
	}
	intent := Build(rg)
	if intent.Hypervisor != "qemu" {
		t.Errorf("hypervisor = %q, want qemu", intent.Hypervisor)
	}
	if intent.CPU != 16 || intent.Memory != 32768 {
		t.Errorf("cpu=%d memory=%d, want 16 32768", intent.CPU, intent.Memory)
	}
}

func TestBuild_WithHypervisorDefault(t *testing.T) {
	rg := &mockResolvedGuest{
		hasSeed:    true,
		hasNetwork: true,
		format:     "raw",
		cpu:        2,
		memory:     2048,
		lifecycle:  "start",
		guestID:    "default/regular-guest",
		hypervisor: "",
	}
	intent := Build(rg)
	if intent.Hypervisor != "" {
		t.Errorf("hypervisor = %q, want empty string (default)", intent.Hypervisor)
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

func TestBuild_WithDataDisk_DiskBoot(t *testing.T) {
	rg := &mockResolvedGuest{
		hasSeed:     true,
		hasNetwork:  true,
		hasDataDisk: true,
		format:      "raw",
		cpu:         2,
		memory:      2048,
		lifecycle:   "start",
		guestID:     "default/data-test",
	}
	intent := Build(rg)
	if intent.DataDisk == nil {
		t.Fatal("dataDisk should be set when HasDataDisk is true")
	}
	wantPath := DisksDataPath + "/" + DataDiskImageFile
	if intent.DataDisk.Path != wantPath {
		t.Errorf("dataDisk.path = %q, want %q", intent.DataDisk.Path, wantPath)
	}
	if intent.DataDisk.Format != "raw" {
		t.Errorf("dataDisk.format = %q, want raw", intent.DataDisk.Format)
	}
}

func TestBuild_WithDataDisk_KernelBoot(t *testing.T) {
	rg := &mockResolvedGuest{
		hasKernel:     true,
		hasNetwork:    true,
		hasDataDisk:   true,
		cpu:           1,
		memory:        512,
		guestID:       "default/kernel-data",
		kernelPath:    "/var/lib/kubeswift/kernels/default-faas-minimal/bzImage",
		initramfsPath: "/var/lib/kubeswift/kernels/default-faas-minimal/rootfs.cpio.gz",
		kernelCmdline: "console=ttyS0",
	}
	intent := Build(rg)
	if intent.DataDisk == nil {
		t.Fatal("dataDisk should be set for kernel boot with HasDataDisk")
	}
	wantPath := DisksDataPath + "/" + DataDiskImageFile
	if intent.DataDisk.Path != wantPath {
		t.Errorf("dataDisk.path = %q, want %q", intent.DataDisk.Path, wantPath)
	}
	if intent.KernelBoot == nil {
		t.Fatal("kernelBoot should also be set")
	}
}

func TestBuild_WithoutDataDisk(t *testing.T) {
	rg := &mockResolvedGuest{
		hasSeed:    true,
		hasNetwork: true,
		format:     "raw",
		cpu:        2,
		memory:     2048,
		lifecycle:  "start",
		guestID:    "default/no-data",
	}
	intent := Build(rg)
	if intent.DataDisk != nil {
		t.Error("dataDisk should be nil when HasDataDisk is false")
	}
}

func TestSerializeParseRoundtrip_WithDataDisk(t *testing.T) {
	intent := &RuntimeIntent{
		RootDisk:  RootDiskSpec{Path: DisksRootPath + "/" + RootDiskImageFile, Format: "raw"},
		SeedPath:  SeedPath,
		CPU:       2,
		Memory:    2048,
		Lifecycle: "start",
		GuestID:   "test-dd",
		Network:   true,
		DataDisk:  &RootDiskSpec{Path: DisksDataPath + "/" + DataDiskImageFile, Format: "raw"},
	}
	data, err := Serialize(intent)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	var parsed RuntimeIntent
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.DataDisk == nil {
		t.Fatal("parsed dataDisk should not be nil")
	}
	if parsed.DataDisk.Path != intent.DataDisk.Path {
		t.Errorf("parsed dataDisk.path = %q, want %q", parsed.DataDisk.Path, intent.DataDisk.Path)
	}
}

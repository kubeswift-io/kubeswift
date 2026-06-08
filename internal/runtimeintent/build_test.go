package runtimeintent

import (
	"encoding/json"
	"testing"
)

type mockResolvedGuest struct {
	hasSeed        bool
	hasKernel      bool
	hasNetwork     bool
	hasDataDisk    bool
	format         string
	rootVolumeMode string // W9: "Filesystem" (default) or "Block"
	cpu            int
	memory         int
	lifecycle      string
	guestID        string
	kernelPath     string
	initramfsPath  string
	kernelCmdline  string
	hypervisor     string
	osType         string
}

func (m *mockResolvedGuest) HasSeed() bool                 { return m.hasSeed }
func (m *mockResolvedGuest) HasKernel() bool               { return m.hasKernel }
func (m *mockResolvedGuest) HasNetwork() bool              { return m.hasNetwork }
func (m *mockResolvedGuest) HasDataDisk() bool             { return m.hasDataDisk }
func (m *mockResolvedGuest) GetRootDiskFormat() string     { return m.format }
func (m *mockResolvedGuest) GetRootDiskVolumeMode() string { return m.rootVolumeMode }
func (m *mockResolvedGuest) GetCPU() int                   { return m.cpu }
func (m *mockResolvedGuest) GetMemoryMiB() int             { return m.memory }
func (m *mockResolvedGuest) GetLifecycle() string          { return m.lifecycle }
func (m *mockResolvedGuest) GetGuestID() string            { return m.guestID }
func (m *mockResolvedGuest) GetKernelPath() string         { return m.kernelPath }
func (m *mockResolvedGuest) GetInitramfsPath() string      { return m.initramfsPath }
func (m *mockResolvedGuest) GetKernelCmdline() string      { return m.kernelCmdline }
func (m *mockResolvedGuest) GetHypervisor() string         { return m.hypervisor }
func (m *mockResolvedGuest) GetOSType() string {
	if m.osType == "" {
		return "linux"
	}
	return m.osType
}
func (m *mockResolvedGuest) GetNICs() []NICIntent { return nil }

// TestBuild_DiskBootBlockMode is the W9 contract test for the
// runtimeintent producer side: a guest with Block-mode root storage
// produces a RuntimeIntent.RootDisk.Path equal to the Block device
// path constant, NOT the filesystem image.raw path. swiftletd hands
// this string to Cloud Hypervisor's --disk path=<value> opaquely; the
// kubelet attaches the Block PVC at this device path via VolumeDevices
// in the launcher pod (controller-side, see pod.go::rootDiskMount).
func TestBuild_DiskBootBlockMode(t *testing.T) {
	rg := &mockResolvedGuest{
		hasSeed:        true,
		hasNetwork:     true,
		format:         "raw",
		rootVolumeMode: "Block",
		cpu:            2,
		memory:         2048,
		lifecycle:      "start",
		guestID:        "block-guest",
	}
	intent := Build(rg)
	if intent.RootDisk.Path != DiskRootDevicePath {
		t.Errorf("Block-mode RootDisk.Path = %q, want %q", intent.RootDisk.Path, DiskRootDevicePath)
	}
	if intent.RootDisk.Path == DisksRootPath+"/"+RootDiskImageFile {
		t.Errorf("Block-mode RootDisk.Path must NOT be the filesystem path")
	}
}

// TestBuild_DiskBootFilesystemModeUnchanged is the regression gate. The
// Filesystem path is byte-identical to pre-W9: RootDisk.Path resolves
// to <DisksRootPath>/<RootDiskImageFile>. Empty rootVolumeMode (the
// default before any caller sets it) MUST resolve to Filesystem too.
func TestBuild_DiskBootFilesystemModeUnchanged(t *testing.T) {
	cases := []struct {
		name string
		mode string
	}{
		{"explicit_Filesystem", "Filesystem"},
		{"empty_defaults_Filesystem", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rg := &mockResolvedGuest{
				hasSeed:        true,
				format:         "raw",
				rootVolumeMode: tc.mode,
				cpu:            2,
				memory:         2048,
				lifecycle:      "start",
				guestID:        "fs-guest",
			}
			intent := Build(rg)
			want := DisksRootPath + "/" + RootDiskImageFile
			if intent.RootDisk.Path != want {
				t.Errorf("Filesystem-mode RootDisk.Path = %q, want %q", intent.RootDisk.Path, want)
			}
		})
	}
}

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

func TestBuild_OSType(t *testing.T) {
	// windows -> intent.OSType=windows (swiftletd adds kvm_hyperv on --cpus).
	win := Build(&mockResolvedGuest{
		hasSeed: true, hasNetwork: true, format: "raw", cpu: 2, memory: 4096,
		lifecycle: "start", guestID: "default/win", osType: "windows",
	})
	if win.OSType != "windows" {
		t.Errorf("OSType = %q, want windows", win.OSType)
	}
	// default (unset) -> linux (no behaviour change for existing guests).
	lin := Build(&mockResolvedGuest{
		hasSeed: true, hasNetwork: true, format: "raw", cpu: 2, memory: 2048,
		lifecycle: "start", guestID: "default/lin",
	})
	if lin.OSType != "linux" {
		t.Errorf("OSType = %q, want linux", lin.OSType)
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

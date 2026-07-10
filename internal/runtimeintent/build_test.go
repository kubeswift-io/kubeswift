package runtimeintent

import (
	"encoding/json"
	"testing"
)

type mockResolvedGuest struct {
	hasSeed             bool
	hasKernel           bool
	hasNetwork          bool
	dataDisks           []DataDiskSpec
	format              string
	rootVolumeMode      string // W9: "Filesystem" (default) or "Block"
	cpu                 int
	memory              int
	lifecycle           string
	guestID             string
	kernelPath          string
	initramfsPath       string
	kernelCmdline       string
	hypervisor          string
	osType              string
	filesystems         []FilesystemIntent
	vhostUserDevices    []VhostUserDeviceIntent
	coreScheduling      string
	vsockCID            uint32
	primaryUDNInterface string
}

func (m *mockResolvedGuest) HasSeed() bool                 { return m.hasSeed }
func (m *mockResolvedGuest) HasKernel() bool               { return m.hasKernel }
func (m *mockResolvedGuest) HasNetwork() bool              { return m.hasNetwork }
func (m *mockResolvedGuest) GetDataDisks() []DataDiskSpec  { return m.dataDisks }
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
func (m *mockResolvedGuest) GetNICs() []NICIntent               { return nil }
func (m *mockResolvedGuest) GetPrimaryUDNInterface() string     { return m.primaryUDNInterface }
func (m *mockResolvedGuest) GetExposedPorts() []PortIntent      { return nil }
func (m *mockResolvedGuest) GetFilesystems() []FilesystemIntent { return m.filesystems }
func (m *mockResolvedGuest) GetVhostUserDevices() []VhostUserDeviceIntent {
	return m.vhostUserDevices
}
func (m *mockResolvedGuest) GetCoreScheduling() string { return m.coreScheduling }
func (m *mockResolvedGuest) GetVsockCID() uint32       { return m.vsockCID }

func TestBuild_VsockSetWhenCIDNonZero(t *testing.T) {
	// disk boot with an agent CID -> intent carries Vsock
	rg := &mockResolvedGuest{hasSeed: true, guestID: "default/src", cpu: 2, memory: 2048, vsockCID: 42}
	got := Build(rg)
	if got.Vsock == nil || got.Vsock.CID != 42 {
		t.Fatalf("expected Vsock CID 42, got %+v", got.Vsock)
	}
	// kernel boot honors it too
	rg.hasSeed = false
	rg.hasKernel = true
	if k := Build(rg); k.Vsock == nil || k.Vsock.CID != 42 {
		t.Fatalf("kernel-boot Vsock not set: %+v", k.Vsock)
	}
	// CID 0 (agent disabled) -> nil
	rg2 := &mockResolvedGuest{hasSeed: true, guestID: "default/src", cpu: 2, memory: 2048, vsockCID: 0}
	if got := Build(rg2); got.Vsock != nil {
		t.Fatalf("expected no Vsock when CID 0, got %+v", got.Vsock)
	}
}

// TestBuild_DiskBootBlockMode is the W9 contract test for the
// runtimeintent producer side: a guest with Block-mode root storage
// produces a RuntimeIntent.RootDisk.Path equal to the Block device
// path constant, NOT the filesystem image.raw path. swiftletd hands
// this string to Cloud Hypervisor's --disk path=<value> opaquely; the
// kubelet attaches the Block PVC at this device path via VolumeDevices
// in the launcher pod (controller-side, see pod.go::rootDiskMount).
// TestBuild_PrimaryUDNInterface is the Model A wiring gate: the top-level
// PrimaryUDNInterface flows from the resolver getter into the intent on BOTH boot
// paths (a default guest's intent has no nics, so this MUST be top-level). Empty
// when not Model A.
func TestBuild_PrimaryUDNInterface(t *testing.T) {
	// disk boot
	rg := &mockResolvedGuest{hasSeed: true, hasNetwork: true, format: "raw", cpu: 2, memory: 2048, lifecycle: "start", guestID: "default/udn", primaryUDNInterface: "ovn-udn1"}
	if got := Build(rg).PrimaryUDNInterface; got != "ovn-udn1" {
		t.Errorf("disk-boot PrimaryUDNInterface = %q, want ovn-udn1", got)
	}
	// kernel boot honors it too
	rg.hasSeed = false
	rg.hasKernel = true
	if got := Build(rg).PrimaryUDNInterface; got != "ovn-udn1" {
		t.Errorf("kernel-boot PrimaryUDNInterface = %q, want ovn-udn1", got)
	}
	// not Model A -> empty
	rg2 := &mockResolvedGuest{hasSeed: true, hasNetwork: true, format: "raw", cpu: 2, memory: 2048, lifecycle: "start", guestID: "default/plain"}
	if got := Build(rg2).PrimaryUDNInterface; got != "" {
		t.Errorf("non-Model-A PrimaryUDNInterface = %q, want empty", got)
	}
}

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

func TestBuild_Filesystems(t *testing.T) {
	// Filesystems flow through to the intent unchanged on the disk-boot path.
	fs := []FilesystemIntent{
		{Name: "data", Tag: "data", SourcePath: VirtiofsBasePath + "/data"},
		{Name: "ro", Tag: "shared", SourcePath: VirtiofsBasePath + "/ro", ReadOnly: true},
	}
	disk := Build(&mockResolvedGuest{
		hasSeed: true, hasNetwork: true, format: "raw", cpu: 2, memory: 2048,
		lifecycle: "start", guestID: "default/fs", filesystems: fs,
	})
	if len(disk.Filesystems) != 2 {
		t.Fatalf("disk-boot Filesystems len = %d, want 2", len(disk.Filesystems))
	}
	if disk.Filesystems[1].Tag != "shared" || !disk.Filesystems[1].ReadOnly {
		t.Errorf("Filesystems[1] = %+v, want tag=shared readOnly=true", disk.Filesystems[1])
	}
	// And on the kernel-boot path.
	kern := Build(&mockResolvedGuest{
		hasKernel: true, hasNetwork: true, cpu: 1, memory: 1024,
		lifecycle: "start", guestID: "default/fs-kern", filesystems: fs,
	})
	if len(kern.Filesystems) != 2 {
		t.Fatalf("kernel-boot Filesystems len = %d, want 2", len(kern.Filesystems))
	}
	// None set -> nil (no virtiofs).
	none := Build(&mockResolvedGuest{
		hasSeed: true, hasNetwork: true, format: "raw", cpu: 2, memory: 2048,
		lifecycle: "start", guestID: "default/no-fs",
	})
	if none.Filesystems != nil {
		t.Errorf("Filesystems = %v, want nil when none set", none.Filesystems)
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

// TestSerializeParseRoundtrip_WithSandboxRootfs pins the mode-3 wire contract:
// a kernel-boot intent carrying sandboxRootfs.path round-trips, and the key is
// omitted (omitempty) for every non-sandbox intent so swiftletd's #[serde(default)]
// leaves it None.
func TestSerializeParseRoundtrip_WithSandboxRootfs(t *testing.T) {
	const rootfs = "/var/lib/kubeswift/sandbox-rootfs/sha256-abc.ext4"
	intent := &RuntimeIntent{
		KernelBoot: &KernelBootSpec{
			KernelPath:    "/var/lib/kubeswift/kernels/default-sandbox/bzImage",
			InitramfsPath: "/var/lib/kubeswift/kernels/default-sandbox/rootfs.cpio.gz",
			Cmdline:       "console=ttyS0 kubeswift.rootfs=block",
		},
		SandboxRootfs: &SandboxRootfsSpec{Path: rootfs},
		CPU:           2,
		Memory:        2048,
		Lifecycle:     "start",
		GuestID:       "default/sbx",
	}
	data, err := Serialize(intent)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	// Wire contract: swiftletd reads sandboxRootfs.path.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal map: %v", err)
	}
	if _, ok := m["sandboxRootfs"]; !ok {
		t.Fatalf("sandboxRootfs key missing from serialized intent: %s", data)
	}
	var parsed RuntimeIntent
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed.SandboxRootfs == nil || parsed.SandboxRootfs.Path != rootfs {
		t.Errorf("parsed sandboxRootfs = %+v, want path %q", parsed.SandboxRootfs, rootfs)
	}

	// omitempty: a non-sandbox intent must NOT carry the key.
	plain, err := Serialize(&RuntimeIntent{GuestID: "x", Lifecycle: "start"})
	if err != nil {
		t.Fatalf("Serialize plain: %v", err)
	}
	var pm map[string]json.RawMessage
	if err := json.Unmarshal(plain, &pm); err != nil {
		t.Fatalf("Unmarshal plain: %v", err)
	}
	if _, ok := pm["sandboxRootfs"]; ok {
		t.Errorf("sandboxRootfs must be omitted when nil: %s", plain)
	}
}

func TestBuild_WithDataDisk_DiskBoot(t *testing.T) {
	wantPath := DisksDataPath + "/" + DataDiskImageFile
	rg := &mockResolvedGuest{
		hasSeed:    true,
		hasNetwork: true,
		dataDisks:  []DataDiskSpec{{Name: "data", Path: wantPath, Format: "raw"}},
		format:     "raw",
		cpu:        2,
		memory:     2048,
		lifecycle:  "start",
		guestID:    "default/data-test",
	}
	intent := Build(rg)
	if len(intent.DataDisks) != 1 {
		t.Fatalf("want 1 data disk, got %d", len(intent.DataDisks))
	}
	if intent.DataDisks[0].Path != wantPath {
		t.Errorf("dataDisks[0].path = %q, want %q", intent.DataDisks[0].Path, wantPath)
	}
	if intent.DataDisks[0].Format != "raw" {
		t.Errorf("dataDisks[0].format = %q, want raw", intent.DataDisks[0].Format)
	}
}

func TestBuild_WithDataDisk_KernelBoot(t *testing.T) {
	wantPath := DisksDataPath + "/" + DataDiskImageFile
	rg := &mockResolvedGuest{
		hasKernel:     true,
		hasNetwork:    true,
		dataDisks:     []DataDiskSpec{{Name: "data", Path: wantPath, Format: "raw"}},
		cpu:           1,
		memory:        512,
		guestID:       "default/kernel-data",
		kernelPath:    "/var/lib/kubeswift/kernels/default-faas-minimal/bzImage",
		initramfsPath: "/var/lib/kubeswift/kernels/default-faas-minimal/rootfs.cpio.gz",
		kernelCmdline: "console=ttyS0",
	}
	intent := Build(rg)
	if len(intent.DataDisks) != 1 {
		t.Fatalf("want 1 data disk for kernel boot, got %d", len(intent.DataDisks))
	}
	if intent.DataDisks[0].Path != wantPath {
		t.Errorf("dataDisks[0].path = %q, want %q", intent.DataDisks[0].Path, wantPath)
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
	if len(intent.DataDisks) != 0 {
		t.Errorf("dataDisks should be empty when no data disk; got %d", len(intent.DataDisks))
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
		DataDisks: []DataDiskSpec{{Name: "data", Path: DisksDataPath + "/" + DataDiskImageFile, Format: "raw"}},
	}
	data, err := Serialize(intent)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	var parsed RuntimeIntent
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(parsed.DataDisks) != 1 {
		t.Fatalf("parsed dataDisks should have 1 entry, got %d", len(parsed.DataDisks))
	}
	if parsed.DataDisks[0].Path != intent.DataDisks[0].Path {
		t.Errorf("parsed dataDisks[0].path = %q, want %q", parsed.DataDisks[0].Path, intent.DataDisks[0].Path)
	}
}

func TestBuild_VhostUserDevices(t *testing.T) {
	devs := []VhostUserDeviceIntent{
		{Name: "d0", Type: "blk", Socket: "/run/spdk/0"},
		{Name: "g0", Type: "generic", Socket: "/run/x/g", VirtioID: "fs", QueueSizes: []int32{1024}},
	}
	disk := Build(&mockResolvedGuest{
		hasSeed: true, format: "raw", cpu: 2, memory: 2048, lifecycle: "start",
		guestID: "default/vu", vhostUserDevices: devs,
	})
	if len(disk.VhostUserDevices) != 2 {
		t.Fatalf("disk-boot len = %d, want 2", len(disk.VhostUserDevices))
	}
	kern := Build(&mockResolvedGuest{
		hasKernel: true, cpu: 1, memory: 1024, lifecycle: "start",
		guestID: "default/vu-k", vhostUserDevices: devs,
	})
	if len(kern.VhostUserDevices) != 2 {
		t.Fatalf("kernel-boot len = %d, want 2", len(kern.VhostUserDevices))
	}
	none := Build(&mockResolvedGuest{hasSeed: true, format: "raw", guestID: "x"})
	if none.VhostUserDevices != nil {
		t.Errorf("want nil when none set")
	}
}

func TestBuild_CoreScheduling(t *testing.T) {
	disk := Build(&mockResolvedGuest{
		hasSeed: true, format: "raw", cpu: 2, memory: 2048, lifecycle: "start",
		guestID: "default/cs", coreScheduling: "vm",
	})
	if disk.CoreScheduling != "vm" {
		t.Errorf("disk-boot CoreScheduling = %q, want vm", disk.CoreScheduling)
	}
	kern := Build(&mockResolvedGuest{
		hasKernel: true, cpu: 1, memory: 1024, lifecycle: "start",
		guestID: "default/cs-k", coreScheduling: "vcpu",
	})
	if kern.CoreScheduling != "vcpu" {
		t.Errorf("kernel-boot CoreScheduling = %q, want vcpu", kern.CoreScheduling)
	}
	none := Build(&mockResolvedGuest{hasSeed: true, format: "raw", guestID: "x"})
	if none.CoreScheduling != "" {
		t.Errorf("CoreScheduling = %q, want empty", none.CoreScheduling)
	}
}

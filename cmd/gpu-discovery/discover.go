package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/klog/v2"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
)

// --- GPU Discovery ---

// GPU PCI class codes (from the PCI specification).
// 0300 = VGA compatible controller (consumer GPUs, some data center GPUs)
// 0302 = 3D controller (most data center GPUs: T4, A100, H100, etc.)
// 0380 = Display controller (some AMD and Intel data center GPUs)
var gpuPCIClasses = map[string]bool{
	"0300": true,
	"0302": true,
	"0380": true,
}

// Known GPU vendor IDs mapped to human-readable names.
var gpuVendorNames = map[string]string{
	"10de": "NVIDIA",
	"1002": "AMD",
	"8086": "Intel",
}

// VendorName returns the human-readable vendor name for a PCI vendor ID.
func VendorName(vendorID string) string {
	if name, ok := gpuVendorNames[strings.ToLower(vendorID)]; ok {
		return name
	}
	return fmt.Sprintf("Unknown (%s)", vendorID)
}

// lspciGPUPattern matches GPU lines by PCI class code, vendor-agnostic.
// Captures: (1) PCI address, (2) class code, (3) description, (4) vendor:device ID.
// Example lines:
//
//	0000:17:00.0 3D controller [0302]: NVIDIA Corporation H200 SXM [10de:2336] (rev a1)
//	0000:03:00.0 VGA compatible controller [0300]: Advanced Micro Devices, Inc. [AMD/ATI] Navi 31 [1002:73bf] (rev c8)
//	0000:56:00.0 Display controller [0380]: Intel Corporation Data Center GPU Flex 170 [8086:56c0]
var lspciGPUPattern = regexp.MustCompile(
	`^([0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F])\s+.+?\[([0-9a-fA-F]{4})\]:\s+(.+?)\s+\[([0-9a-fA-F]{4}):([0-9a-fA-F]{4})\]`)

func discoverGPUs() ([]gpuv1alpha1.GPUDevice, error) {
	output, err := runCommand("lspci", "-Dnn")
	if err != nil {
		return nil, fmt.Errorf("lspci -Dnn: %w", err)
	}
	return parseGPUsFromLspci(output)
}

// parseGPUsFromLspci parses lspci -Dnn output and returns GPU devices.
// Detects GPUs from any vendor by PCI class code (0300, 0302, 0380).
func parseGPUsFromLspci(output string) ([]gpuv1alpha1.GPUDevice, error) {
	var gpus []gpuv1alpha1.GPUDevice
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		matches := lspciGPUPattern.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		pciAddr := matches[1]
		classCode := matches[2]
		description := strings.TrimSpace(matches[3])
		vendorID := matches[4]
		deviceID := matches[5]

		// Filter by GPU PCI class codes.
		if !gpuPCIClasses[classCode] {
			continue
		}

		vendor := VendorName(vendorID)
		fullDeviceID := vendorID + ":" + deviceID

		// Extract model name from the description.
		// lspci descriptions look like "NVIDIA Corporation H200 SXM" or
		// "Advanced Micro Devices, Inc. [AMD/ATI] Navi 31 [Radeon RX 7900 XTX]".
		// We use the full description as the model — it's already human-readable.
		model := description

		numaNode := readNUMANode(pciAddr)
		iommuGroup := readIOMMUGroup(pciAddr)
		driver := readDriver(pciAddr)
		barSizes := discoverBARSizes(pciAddr)

		gpus = append(gpus, gpuv1alpha1.GPUDevice{
			PCIAddress: pciAddr,
			Vendor:     vendor,
			Model:      model,
			DeviceID:   fullDeviceID,
			NUMANode:   numaNode,
			IOMMUGroup: iommuGroup,
			Driver:     driver,
			BARSizes:   barSizes,
		})
	}

	// Sort by PCI address for deterministic ordering.
	sort.Slice(gpus, func(i, j int) bool {
		return gpus[i].PCIAddress < gpus[j].PCIAddress
	})

	// Assign sequential index.
	for i := range gpus {
		gpus[i].Index = i
	}

	return gpus, nil
}

// HasNVIDIAGPUs returns true if any discovered GPU is from NVIDIA.
func HasNVIDIAGPUs(gpus []gpuv1alpha1.GPUDevice) bool {
	for _, g := range gpus {
		if g.Vendor == "NVIDIA" {
			return true
		}
	}
	return false
}

func readNUMANode(pciAddr string) int {
	data, err := os.ReadFile(filepath.Join("/sys/bus/pci/devices", pciAddr, "numa_node"))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func readIOMMUGroup(pciAddr string) int {
	link, err := os.Readlink(filepath.Join("/sys/bus/pci/devices", pciAddr, "iommu_group"))
	if err != nil {
		return -1
	}
	base := filepath.Base(link)
	n, err := strconv.Atoi(base)
	if err != nil {
		return -1
	}
	return n
}

func readDriver(pciAddr string) string {
	link, err := os.Readlink(filepath.Join("/sys/bus/pci/devices", pciAddr, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

// --- BAR Size Discovery ---

// barPattern matches lspci -vvv lines like:
// "	Region 0: Memory at ... [size=64M]" or "[size=256G]"
var barPattern = regexp.MustCompile(
	`Region\s+(\d+):\s+Memory\s+.*\[size=(\d+)([KMGT]?)\]`)

func discoverBARSizes(pciAddr string) []gpuv1alpha1.BARSize {
	output, err := runCommand("lspci", "-vvv", "-s", pciAddr)
	if err != nil {
		return nil
	}
	return parseBARSizes(output)
}

// parseBARSizes parses lspci -vvv output for BAR regions and sizes.
func parseBARSizes(output string) []gpuv1alpha1.BARSize {
	var bars []gpuv1alpha1.BARSize
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		matches := barPattern.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		region, _ := strconv.Atoi(matches[1])
		size, _ := strconv.ParseInt(matches[2], 10, 64)
		suffix := matches[3]

		// Convert to MiB.
		var sizeMi int64
		switch suffix {
		case "K":
			// Less than 1 MiB, record as 0 (not useful for our purposes).
			sizeMi = 0
		case "M":
			sizeMi = size
		case "G":
			sizeMi = size * 1024
		case "T":
			sizeMi = size * 1024 * 1024
		default:
			// Bytes — convert.
			sizeMi = size / (1024 * 1024)
		}
		if sizeMi > 0 {
			bars = append(bars, gpuv1alpha1.BARSize{Region: region, SizeMi: sizeMi})
		}
	}
	return bars
}

// --- NUMA Topology Discovery ---

func discoverHostTopology() (gpuv1alpha1.HostTopology, error) {
	cpu, err := discoverCPUTopology()
	if err != nil {
		return gpuv1alpha1.HostTopology{}, err
	}

	numaNodes, err := discoverNUMANodes()
	if err != nil {
		return gpuv1alpha1.HostTopology{}, err
	}

	iommu := isIOMMUEnabled()
	hugepages := discoverHugepages()

	return gpuv1alpha1.HostTopology{
		CPUTopology:  cpu,
		NUMANodes:    numaNodes,
		IOMMUEnabled: iommu,
		Hugepages1Gi: hugepages,
	}, nil
}

// parseLscpuOutput parses lscpu output into a CPUTopologyInfo.
func parseLscpuOutput(output string) gpuv1alpha1.CPUTopologyInfo {
	info := gpuv1alpha1.CPUTopologyInfo{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "Socket(s)":
			info.Sockets, _ = strconv.Atoi(val)
		case "Core(s) per socket":
			info.CoresPerSocket, _ = strconv.Atoi(val)
		case "Thread(s) per core":
			info.ThreadsPerCore, _ = strconv.Atoi(val)
		case "CPU(s)":
			info.TotalCPUs, _ = strconv.Atoi(val)
		}
	}
	return info
}

func discoverCPUTopology() (gpuv1alpha1.CPUTopologyInfo, error) {
	output, err := runCommand("lscpu")
	if err != nil {
		return gpuv1alpha1.CPUTopologyInfo{}, fmt.Errorf("lscpu: %w", err)
	}
	return parseLscpuOutput(output), nil
}

func discoverNUMANodes() ([]gpuv1alpha1.NUMANodeInfo, error) {
	entries, err := os.ReadDir("/sys/devices/system/node")
	if err != nil {
		return nil, fmt.Errorf("read NUMA nodes: %w", err)
	}

	var nodes []gpuv1alpha1.NUMANodeInfo
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "node") {
			continue
		}
		idStr := strings.TrimPrefix(entry.Name(), "node")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		nodePath := filepath.Join("/sys/devices/system/node", entry.Name())

		cpus := readFileContent(filepath.Join(nodePath, "cpulist"))
		memMi := parseNUMAMemory(filepath.Join(nodePath, "meminfo"), id)

		nodes = append(nodes, gpuv1alpha1.NUMANodeInfo{
			ID:       id,
			CPUs:     cpus,
			MemoryMi: memMi,
		})
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes, nil
}

// parseNUMAMemoryContent parses /sys/devices/system/node/nodeN/meminfo content
// for MemTotal, returning the value in MiB.
func parseNUMAMemoryContent(content string, numaID int) int64 {
	prefix := fmt.Sprintf("Node %d MemTotal:", numaID)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, prefix) {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				kb, _ := strconv.ParseInt(fields[3], 10, 64)
				return kb / 1024
			}
		}
	}
	return 0
}

func parseNUMAMemory(path string, numaID int) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return parseNUMAMemoryContent(string(data), numaID)
}

func isIOMMUEnabled() bool {
	entries, err := os.ReadDir("/sys/kernel/iommu_groups")
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// isVfioPciLoaded reports whether the vfio-pci driver is registered on this
// node (the driver directory exists in sysfs), which is required for GPU
// passthrough: gpu-init binds devices to vfio-pci, and that fails if the
// module is not loaded. This is a READ-ONLY check — the gpu-discovery
// DaemonSet runs with drop-ALL capabilities and a read-only /sys, so it
// reports readiness but cannot load the module; loading vfio-pci is a host
// responsibility (e.g. /etc/modules-load.d/vfio.conf).
func isVfioPciLoaded() bool {
	info, err := os.Stat("/sys/bus/pci/drivers/vfio-pci")
	return err == nil && info.IsDir()
}

func discoverHugepages() gpuv1alpha1.HugepageInfo {
	total := readIntFile("/sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages")
	free := readIntFile("/sys/kernel/mm/hugepages/hugepages-1048576kB/free_hugepages")
	return gpuv1alpha1.HugepageInfo{Total: total, Free: free}
}

// --- NVSwitch Discovery ---

// Known NVSwitch PCI device IDs.
var nvSwitchDeviceIDs = map[string]bool{
	"10de:22a3": true,
	"10de:22a4": true,
}

func discoverNVSwitches() ([]gpuv1alpha1.NVSwitchDevice, error) {
	output, err := runCommand("lspci", "-Dnn")
	if err != nil {
		return nil, err
	}
	return parseNVSwitchesFromLspci(output)
}

// parseNVSwitchesFromLspci parses lspci -Dnn output for NVSwitch devices.
func parseNVSwitchesFromLspci(output string) ([]gpuv1alpha1.NVSwitchDevice, error) {
	// Pattern: "0000:0a:00.0 Bridge [0680]: NVIDIA Corporation ... [10de:22a4]"
	pattern := regexp.MustCompile(
		`^([0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]).*\[([0-9a-fA-F]{4}:[0-9a-fA-F]{4})\]`)

	var switches []gpuv1alpha1.NVSwitchDevice
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		matches := pattern.FindStringSubmatch(scanner.Text())
		if matches == nil {
			continue
		}
		devID := matches[2]
		if !nvSwitchDeviceIDs[devID] {
			continue
		}
		pciAddr := matches[1]
		switches = append(switches, gpuv1alpha1.NVSwitchDevice{
			PCIAddress: pciAddr,
			DeviceID:   devID,
			NUMANode:   readNUMANode(pciAddr),
		})
	}
	return switches, nil
}

// --- Fabric Manager Discovery ---

func discoverFabricManager(moduleToIndex map[int]int) (*gpuv1alpha1.FabricManagerStatus, error) {
	fmpmPath, err := exec.LookPath("fmpm")
	if err != nil {
		return nil, fmt.Errorf("fmpm not in PATH")
	}

	fm := &gpuv1alpha1.FabricManagerStatus{
		Installed: true,
	}

	// Get version.
	verOutput, err := runCommand(fmpmPath, "-v")
	if err != nil {
		fm.Version = "unknown"
	} else {
		fm.Version = strings.TrimSpace(verOutput)
	}

	// Check running state.
	statusOutput, err := runCommand("systemctl", "is-active", "nvidia-fabricmanager")
	fm.Running = err == nil && strings.TrimSpace(statusOutput) == "active"

	// Parse partitions.
	partOutput, err := runCommand(fmpmPath, "-q")
	if err != nil {
		klog.V(2).InfoS("fmpm -q failed, skipping partition info", "err", err)
	} else {
		fm.Partitions = parseFMPartitions(partOutput, moduleToIndex)
		if len(fm.Partitions) > 0 && len(moduleToIndex) == 0 {
			// No silent failure (Design Principle #6): FM partition membership is
			// keyed on GPU physical/Module IDs, which do NOT follow lspci order.
			// Without the nvidia-smi Module-ID join the GPUIndices fall back to
			// identity and are very likely WRONG on real HGX baseboards.
			klog.ErrorS(nil, "Fabric Manager partitions discovered but GPU Module IDs are unavailable "+
				"(nvidia-smi missing from the discovery environment); partition GPUIndices fall back to "+
				"an identity mapping and are very likely INCORRECT on HGX baseboards. Provide nvidia-smi "+
				"to the gpu-discovery pod (host-mount or NVIDIA container runtime).")
		}
	}

	return fm, nil
}

// parseFMPartitions parses a Fabric Manager partition listing and translates the
// per-partition GPU membership into the device-Index space the allocator uses
// (FMPartitionStatus.GPUIndices, consumed by selectPartitionGPUs).
//
// SEMANTICS (NVIDIA HGX Shared NVSwitch GPU Passthrough guide, WP-12736-002):
// Fabric Manager expresses partition membership in GPU *physical IDs* — the
// baseboard slot / Module ID surfaced by `nvidia-smi -q` as "GPU Module Id".
// Those IDs do NOT follow lspci/BDF order, so they cannot be used as device
// indices directly. moduleToIndex (from discoverGPUModuleIDs) carries the
// (Module ID -> device Index) join; translatePartitionGPUIndices applies it.
//
// FORMAT: the line grammar below ("partition N: gpus=... active=...") is a
// placeholder — the fmpm CLI text format is FM/driver-version dependent and the
// canonical acquisition is the Fabric Manager shared-library API
// (fmGetSupportedFabricPartitions). This is the hardware-gated adapter seam; the
// LOAD-BEARING correctness (physical-ID -> Index translation) lives in
// translatePartitionGPUIndices and is unit-tested independent of the source
// format. The parsed `gpus=` values are physical/Module IDs, NOT indices.
var fmPartitionPattern = regexp.MustCompile(
	`partition\s+(\d+):\s+gpus=([\d,]+)\s+active=(\w+)`)

func parseFMPartitions(output string, moduleToIndex map[int]int) []gpuv1alpha1.FMPartitionStatus {
	var partitions []gpuv1alpha1.FMPartitionStatus
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		matches := fmPartitionPattern.FindStringSubmatch(scanner.Text())
		if matches == nil {
			continue
		}
		id, _ := strconv.Atoi(matches[1])
		physicalIDs := parseIntList(matches[2])
		active := matches[3] == "true"

		partitions = append(partitions, gpuv1alpha1.FMPartitionStatus{
			ID:         id,
			GPUIndices: translatePartitionGPUIndices(physicalIDs, moduleToIndex),
			Active:     active,
		})
	}
	return partitions
}

// translatePartitionGPUIndices maps a partition's GPU physical / Module IDs into
// device-Index space via moduleToIndex (see parseFMPartitions for why this is
// required).
//
//   - Empty map (nvidia-smi unavailable): the raw physical IDs pass through
//     unchanged (identity) so PCIe-only nodes and unit tests still function; the
//     caller logs a prominent warning because identity is likely wrong on HGX.
//   - Non-empty map, physical ID present: translated to its device Index.
//   - Non-empty map, physical ID absent: DROPPED (the partition ends up
//     under-count and is therefore not selectable — a safe failure, never a
//     wrong-NVLink one) with a warning.
func translatePartitionGPUIndices(physicalIDs []int, moduleToIndex map[int]int) []int {
	if len(moduleToIndex) == 0 {
		return physicalIDs
	}
	out := make([]int, 0, len(physicalIDs))
	for _, pid := range physicalIDs {
		idx, ok := moduleToIndex[pid]
		if !ok {
			klog.V(1).InfoS("FM partition references a GPU Module ID with no discovered device; dropping it from the partition",
				"moduleID", pid)
			continue
		}
		out = append(out, idx)
	}
	return out
}

// --- GPU Module ID Discovery (nvidia-smi) ---

// nvidiaSmiGPUHeaderPattern matches a per-GPU section header in `nvidia-smi -q`,
// e.g. "GPU 00000000:0F:00.0". nvidia-smi uses an 8-hex-digit domain and
// upper-case hex; lspci discovery uses a 4-hex-digit lower-case domain —
// normalizeBDF reconciles the two.
var nvidiaSmiGPUHeaderPattern = regexp.MustCompile(
	`^GPU\s+([0-9a-fA-F]{4,8}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F])`)

// moduleIDPattern matches the "GPU Module Id" field in `nvidia-smi -q`. The
// exact field name is FM/driver-version dependent (seen as "GPU Module Id" and
// "Module Id"); match case-insensitively on the "module id" tail.
var moduleIDPattern = regexp.MustCompile(`(?i)module id\s*:\s*(\d+)`)

// discoverGPUModuleIDs runs `nvidia-smi -q` and returns a (GPU Module Id ->
// device Index) map for the discovered GPUs — the join Fabric Manager partition
// translation needs. Returns an empty map (never an error) when nvidia-smi is
// unavailable, so FM partition translation degrades to identity with a
// caller-side warning.
//
// DEPLOYMENT: nvidia-smi must be present in the discovery environment. The
// shipped gpu-discovery DaemonSet is drop-ALL / read-only-/sys and does NOT ship
// it — HGX deployments must provide nvidia-smi to the discovery pod (host-mount
// or NVIDIA container runtime). Correct on PCIe-tier nodes without it (no FM, no
// partitions to translate).
func discoverGPUModuleIDs(gpus []gpuv1alpha1.GPUDevice) map[int]int {
	smiPath, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return map[int]int{}
	}
	out, err := runCommand(smiPath, "-q")
	if err != nil {
		klog.V(2).InfoS("nvidia-smi -q failed; FM partition membership will degrade to identity", "err", err)
		return map[int]int{}
	}
	return buildModuleToIndex(parseModuleIDs(out), gpus)
}

// parseModuleIDs parses `nvidia-smi -q` output into a (normalized BDF -> GPU
// Module Id) map. Each "GPU <BDF>" section header starts a new device; the first
// "Module Id" line within that section records its module id.
func parseModuleIDs(output string) map[string]int {
	result := map[string]int{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	current := ""
	for scanner.Scan() {
		line := scanner.Text()
		if m := nvidiaSmiGPUHeaderPattern.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			current = normalizeBDF(m[1])
			continue
		}
		if current == "" {
			continue
		}
		if m := moduleIDPattern.FindStringSubmatch(line); m != nil {
			if _, seen := result[current]; !seen {
				id, _ := strconv.Atoi(m[1])
				result[current] = id
			}
		}
	}
	return result
}

// buildModuleToIndex joins the (normalized BDF -> Module Id) map from nvidia-smi
// with the (BDF -> Index) order from lspci discovery to produce the
// (Module Id -> device Index) map the FM-partition translation consumes.
func buildModuleToIndex(bdfToModule map[string]int, gpus []gpuv1alpha1.GPUDevice) map[int]int {
	out := map[int]int{}
	for _, g := range gpus {
		if mod, ok := bdfToModule[normalizeBDF(g.PCIAddress)]; ok {
			out[mod] = g.Index
		}
	}
	return out
}

// normalizeBDF canonicalizes a PCI address to lower-case with a 4-hex-digit
// domain, so nvidia-smi's "00000000:0F:00.0" and lspci's "0000:0f:00.0" compare
// equal. A domain-less "BB:DD.F" is treated as domain 0000.
func normalizeBDF(bdf string) string {
	bdf = strings.ToLower(strings.TrimSpace(bdf))
	if strings.Count(bdf, ":") == 1 {
		bdf = "0000:" + bdf
	}
	parts := strings.SplitN(bdf, ":", 2)
	if len(parts) != 2 {
		return bdf
	}
	dom := parts[0]
	switch {
	case len(dom) > 4:
		dom = dom[len(dom)-4:]
	case len(dom) < 4:
		dom = strings.Repeat("0", 4-len(dom)) + dom
	}
	return dom + ":" + parts[1]
}

// --- Utility functions ---

func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w", name, args, err)
	}
	return string(out), nil
}

func readFileContent(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readIntFile(path string) int {
	s := readFileContent(path)
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func parseIntList(s string) []int {
	parts := strings.Split(s, ",")
	var result []int
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil {
			result = append(result, n)
		}
	}
	return result
}

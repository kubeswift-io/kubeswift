package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// gpuDevice is one discovered GPU function on the node.
type gpuDevice struct {
	// PCIAddress is the full BDF, e.g. "0000:01:00.0".
	PCIAddress string
	// VendorDevice is "<vendor>:<device>", e.g. "10de:1b80".
	VendorDevice string
	// NUMANode is the NUMA affinity (-1/absent normalizes to 0).
	NUMANode int64
	// IOMMUGroup is the IOMMU group number, or -1 when unknown.
	IOMMUGroup int64
}

// discoverGPUs scans sysfs for display-class PCI devices (class 0x0300xx
// VGA-compatible / 0x0302xx 3D controller) — the same classes the
// gpu-discovery DaemonSet matches via lspci, but pure-sysfs so the driver
// image needs no pciutils. sysfsRoot is parameterized for tests ("/sys" in
// production).
func discoverGPUs(sysfsRoot string) ([]gpuDevice, error) {
	devRoot := filepath.Join(sysfsRoot, "bus", "pci", "devices")
	entries, err := os.ReadDir(devRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", devRoot, err)
	}
	var gpus []gpuDevice
	for _, e := range entries {
		bdf := e.Name()
		dir := filepath.Join(devRoot, bdf)
		class := readSysfsString(filepath.Join(dir, "class"))
		// class file is "0x030000"-style; match 0x0300xx and 0x0302xx.
		if !strings.HasPrefix(class, "0x0300") && !strings.HasPrefix(class, "0x0302") {
			continue
		}
		vendor := strings.TrimPrefix(readSysfsString(filepath.Join(dir, "vendor")), "0x")
		device := strings.TrimPrefix(readSysfsString(filepath.Join(dir, "device")), "0x")

		numa := readSysfsInt(filepath.Join(dir, "numa_node"))
		if numa < 0 {
			numa = 0
		}
		iommu := int64(-1)
		if link, err := os.Readlink(filepath.Join(dir, "iommu_group")); err == nil {
			if n, err := strconv.ParseInt(filepath.Base(link), 10, 64); err == nil {
				iommu = n
			}
		}
		gpus = append(gpus, gpuDevice{
			PCIAddress:   bdf,
			VendorDevice: vendor + ":" + device,
			NUMANode:     numa,
			IOMMUGroup:   iommu,
		})
	}
	return gpus, nil
}

// vfioReady reports whether the vfio-pci driver is registered on this node
// (the same check gpu-discovery publishes on SwiftGPUNode.status.vfioReady).
func vfioReady(sysfsRoot string) bool {
	_, err := os.Stat(filepath.Join(sysfsRoot, "bus", "pci", "drivers", "vfio-pci"))
	return err == nil
}

func readSysfsString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readSysfsInt(path string) int64 {
	s := readSysfsString(path)
	if s == "" {
		return -1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

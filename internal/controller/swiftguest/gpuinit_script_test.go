package swiftguest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfs builds the slice of /sys that gpu-init.sh reads: devices with a
// class, an IOMMU group and optionally a bound driver, plus the group listing.
type fakeDev struct{ bdf, class, driver string }

func fakeSysfs(t *testing.T, group string, devs ...fakeDev) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	pci := filepath.Join(root, "bus", "pci")
	must(os.MkdirAll(filepath.Join(root, "kernel", "iommu_groups", group, "devices"), 0o755))
	must(os.MkdirAll(pci, 0o755))
	must(os.WriteFile(filepath.Join(pci, "drivers_probe"), nil, 0o644))
	for _, d := range devs {
		dir := filepath.Join(pci, "devices", d.bdf)
		must(os.MkdirAll(dir, 0o755))
		must(os.WriteFile(filepath.Join(dir, "class"), []byte(d.class+"\n"), 0o644))
		must(os.WriteFile(filepath.Join(dir, "driver_override"), nil, 0o644))
		must(os.Symlink("../../../../kernel/iommu_groups/"+group, filepath.Join(dir, "iommu_group")))
		must(os.WriteFile(filepath.Join(root, "kernel", "iommu_groups", group, "devices", d.bdf), nil, 0o644))
		if d.driver != "" {
			drv := filepath.Join(pci, "drivers", d.driver)
			must(os.MkdirAll(drv, 0o755))
			if _, err := os.Stat(filepath.Join(drv, "unbind")); os.IsNotExist(err) {
				must(os.WriteFile(filepath.Join(drv, "unbind"), nil, 0o644))
			}
			must(os.Symlink("../../drivers/"+d.driver, filepath.Join(dir, "driver")))
		}
	}
	return root
}

func runGPUInit(t *testing.T, sys, addrs string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", "../../../rust/swiftletd/scripts/gpu-init.sh")
	cmd.Env = append(os.Environ(), "HOST_SYS_PATH="+sys, "GPU_PCI_ADDRESSES="+addrs, "GPU_PARTITION_ID=-1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// On a board without ACS the GPU's IOMMU group can hold another card's NVMe
// controller. gpu-init used to bind every non-bridge device in the group to
// vfio-pci, taking the node's disk away. It must refuse, and unbind nothing.
func TestGPUInit_RefusesAGroupWithAHostDevice(t *testing.T) {
	sys := fakeSysfs(t, "7",
		fakeDev{"0000:01:00.0", "0x030000", "nvidia"},
		fakeDev{"0000:01:00.1", "0x040300", "snd_hda_intel"},
		fakeDev{"0000:02:00.0", "0x010802", "nvme"},
	)
	out, err := runGPUInit(t, sys, "0000:01:00.0")
	if err == nil || !strings.Contains(out, "0000:02:00.0") || !strings.Contains(out, "not part of this GPU") {
		t.Fatalf("want a refusal naming the NVMe controller, got err=%v\n%s", err, out)
	}
	for _, drv := range []string{"nvme", "nvidia", "snd_hda_intel"} {
		if b, _ := os.ReadFile(filepath.Join(sys, "bus", "pci", "drivers", drv, "unbind")); len(b) != 0 {
			t.Errorf("%s was unbound (%q) although the group was refused", drv, b)
		}
	}
}

// The GPU's own functions (same bus:device) are part of the card and are
// bound with it.
func TestGPUInit_AcceptsTheGPUsOwnFunctions(t *testing.T) {
	sys := fakeSysfs(t, "7",
		fakeDev{"0000:01:00.0", "0x030000", "vfio-pci"},
		fakeDev{"0000:01:00.1", "0x040300", "vfio-pci"},
	)
	if out, err := runGPUInit(t, sys, "0000:01:00.0"); err != nil || !strings.Contains(out, "gpu-init: complete") {
		t.Fatalf("a GPU with only its own functions in the group was refused: err=%v\n%s", err, out)
	}
}

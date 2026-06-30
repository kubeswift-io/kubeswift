package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeswift-io/kubeswift/internal/gpualloc"
)

// writeFakeGPU creates a sysfs-shaped GPU under root.
func writeFakeGPU(t *testing.T, root, bdf, class, vendor, device, numa string, iommuGroup string) {
	t.Helper()
	dir := filepath.Join(root, "bus", "pci", "devices", bdf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"class": class, "vendor": vendor, "device": device, "numa_node": numa} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if iommuGroup != "" {
		gdir := filepath.Join(root, "kernel", "iommu_groups", iommuGroup)
		if err := os.MkdirAll(gdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(gdir, filepath.Join(dir, "iommu_group")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverGPUs(t *testing.T) {
	root := t.TempDir()
	writeFakeGPU(t, root, "0000:01:00.0", "0x030000", "0x10de", "0x1b80", "0", "13") // GTX 1080-ish VGA
	writeFakeGPU(t, root, "0000:02:00.0", "0x030200", "0x10de", "0x20b5", "1", "14") // 3D controller
	writeFakeGPU(t, root, "0000:03:00.0", "0x020000", "0x8086", "0x1521", "0", "")   // a NIC — must be skipped

	gpus, err := discoverGPUs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want 2 (the NIC must be skipped): %+v", len(gpus), gpus)
	}
	if gpus[0].PCIAddress != "0000:01:00.0" || gpus[0].VendorDevice != "10de:1b80" ||
		gpus[0].NUMANode != 0 || gpus[0].IOMMUGroup != 13 {
		t.Errorf("gpu[0] = %+v", gpus[0])
	}
	if gpus[1].PCIAddress != "0000:02:00.0" || gpus[1].NUMANode != 1 || gpus[1].IOMMUGroup != 14 {
		t.Errorf("gpu[1] = %+v", gpus[1])
	}

	if vfioReady(root) {
		t.Error("vfioReady must be false without the driver dir")
	}
	if err := os.MkdirAll(filepath.Join(root, "bus", "pci", "drivers", "vfio-pci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !vfioReady(root) {
		t.Error("vfioReady must be true once the driver dir exists")
	}
}

func TestWriteClaimCDISpec(t *testing.T) {
	dir := t.TempDir()
	id, err := writeClaimCDISpec(dir, "uid-123", []string{"0000:01:00.0", "0000:02:00.0"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "kubeswift.io/gpu=claim-uid-123" {
		t.Errorf("CDI ID = %q", id)
	}
	raw, err := os.ReadFile(claimCDISpecPath(dir, "uid-123"))
	if err != nil {
		t.Fatal(err)
	}
	var spec cdiSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	if spec.CDIVersion != "0.6.0" || spec.Kind != "kubeswift.io/gpu" {
		t.Errorf("spec header = %+v", spec)
	}
	env := strings.Join(spec.Devices[0].ContainerEdits.Env, ";")
	if !strings.Contains(env, "GPU_PCI_ADDRESSES=0000:01:00.0,0000:02:00.0") ||
		!strings.Contains(env, "GPU_PARTITION_ID=-1") {
		t.Errorf("containerEdits env = %q", env)
	}

	// Unprepare removes it; a second remove is an idempotent no-op.
	if err := removeClaimCDISpec(dir, "uid-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(claimCDISpecPath(dir, "uid-123")); !os.IsNotExist(err) {
		t.Error("spec file must be gone")
	}
	if err := removeClaimCDISpec(dir, "uid-123"); err != nil {
		t.Errorf("second remove must be a no-op: %v", err)
	}
}

func TestPrepareClaim(t *testing.T) {
	dir := t.TempDir()
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default", UID: "uid-9"},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Request: "gpu", Driver: driverName, Pool: "boba",
							Device: gpualloc.EncodeDeviceName("0000:01:00.0")},
						{Request: "x", Driver: "other.example.com", Pool: "p", Device: "d"},
					},
				},
			},
		},
	}
	d := &draDriver{nodeName: "boba", cdiDir: dir, kube: fake.NewSimpleClientset(claim)}

	res := d.prepareClaim(context.Background(), claim)
	if res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}
	// Only OUR device is returned; the foreign driver's result is ignored.
	if len(res.Devices) != 1 {
		t.Fatalf("devices = %+v, want exactly ours", res.Devices)
	}
	dev := res.Devices[0]
	if dev.DeviceName != gpualloc.EncodeDeviceName("0000:01:00.0") || dev.PoolName != "boba" {
		t.Errorf("device = %+v", dev)
	}
	if len(dev.CDIDeviceIDs) != 1 || dev.CDIDeviceIDs[0] != "kubeswift.io/gpu=claim-uid-9" {
		t.Errorf("CDIDeviceIDs = %v", dev.CDIDeviceIDs)
	}
	if _, err := os.Stat(claimCDISpecPath(dir, "uid-9")); err != nil {
		t.Errorf("CDI spec must exist after prepare: %v", err)
	}

	// The §A3 tier-1 device status was written through the fake clientset.
	fresh, _ := d.kube.ResourceV1().ResourceClaims("default").Get(context.Background(), "c", metav1.GetOptions{})
	if len(fresh.Status.Devices) != 1 {
		t.Fatalf("status.devices = %+v, want the prepared entry", fresh.Status.Devices)
	}
	var data map[string]string
	if err := json.Unmarshal(fresh.Status.Devices[0].Data.Raw, &data); err != nil || data["pciAddress"] != "0000:01:00.0" {
		t.Errorf("status Data = %s (err %v)", fresh.Status.Devices[0].Data.Raw, err)
	}
}

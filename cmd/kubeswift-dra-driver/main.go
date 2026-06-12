// kubeswift-dra-driver is KubeSwift's REFERENCE DRA driver for VM GPU
// passthrough (design doc docs/design/dra-gpu-integration.md §A4). It runs as
// a DaemonSet on kubeswift.io/gpu-node=true nodes and:
//
//  1. publishes the node's GPUs as a ResourceSlice (driver gpu.kubeswift.io;
//     device names encode the PCI BDF: gpu-0000-01-00-0 — the §A3 contract),
//  2. implements the kubelet DRA plugin: at pod admission it writes a
//     per-claim CDI spec whose containerEdits inject GPU_PCI_ADDRESSES /
//     GPU_PARTITION_ID into the claim's containers (the device hand-off that
//     gpu-init and swiftletd consume; §A2),
//  3. best-effort publishes status.devices[].data {"pciAddress"} on the claim.
//
// The SCHEDULER allocates (DRA structured parameters) — there is no allocation
// controller. vfio-pci binding stays with the launcher pod's gpu-init (proven,
// idempotent); this driver only advertises and hands identity over.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/projectbeskar/kubeswift/internal/gpualloc"
)

func main() {
	var (
		nodeName  = flag.String("node-name", os.Getenv("NODE_NAME"), "node this driver instance runs on (downward API)")
		cdiDir    = flag.String("cdi-dir", "/var/run/cdi", "directory to write per-claim CDI spec files")
		sysfsRoot = flag.String("sysfs-root", "/sys", "sysfs root for GPU discovery")
		interval  = flag.Duration("rediscover-interval", 60*time.Second, "republish interval for the ResourceSlice inventory")
	)
	klog.InitFlags(nil)
	flag.Parse()
	logger := klog.Background()

	if *nodeName == "" {
		logger.Error(nil, "--node-name (or NODE_NAME) is required")
		os.Exit(1)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error(err, "in-cluster config")
		os.Exit(1)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "build clientset")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The kubeletplugin helper binds its gRPC socket under
	// /var/lib/kubelet/plugins/<driver>/ but does NOT create the directory —
	// without it the bind fails "no such file or directory" (cluster-e2e
	// finding, 2026-06-12).
	pluginDir := filepath.Join("/var/lib/kubelet/plugins", driverName)
	if err := os.MkdirAll(pluginDir, 0o750); err != nil {
		logger.Error(err, "create plugin data dir", "dir", pluginDir)
		os.Exit(1)
	}

	driver := &draDriver{nodeName: *nodeName, cdiDir: *cdiDir, kube: kube}
	helper, err := kubeletplugin.Start(ctx, driver,
		kubeletplugin.DriverName(driverName),
		kubeletplugin.NodeName(*nodeName),
		kubeletplugin.KubeClient(kube),
	)
	if err != nil {
		logger.Error(err, "start kubelet plugin")
		os.Exit(1)
	}
	defer helper.Stop()
	logger.Info("kubelet plugin registered", "driver", driverName, "node", *nodeName)

	// Publish the GPU inventory now and on every interval (devices rarely
	// change, but vfio-pci readiness can — e.g. after the operator loads the
	// module). The helper's resourceslice controller diffs and only writes
	// real changes.
	publish := func() {
		gpus, err := discoverGPUs(*sysfsRoot)
		if err != nil {
			logger.Error(err, "GPU discovery failed; keeping previous inventory")
			return
		}
		ready := vfioReady(*sysfsRoot)
		devices := make([]resourceapi.Device, 0, len(gpus))
		for _, g := range gpus {
			devices = append(devices, resourceapi.Device{
				// The §A3 naming contract: the device name encodes the BDF so
				// the allocation result alone identifies the device, with or
				// without the device-status feature.
				Name: gpualloc.EncodeDeviceName(g.PCIAddress),
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"pciAddress":   {StringValue: &g.PCIAddress},
					"vendorDevice": {StringValue: &g.VendorDevice},
					"numaNode":     {IntValue: &g.NUMANode},
					"iommuGroup":   {IntValue: &g.IOMMUGroup},
					"vfioReady":    {BoolValue: &ready},
				},
			})
		}
		err = helper.PublishResources(ctx, resourceslice.DriverResources{
			Pools: map[string]resourceslice.Pool{
				*nodeName: {Slices: []resourceslice.Slice{{Devices: devices}}},
			},
		})
		if err != nil {
			logger.Error(err, "publish ResourceSlice")
			return
		}
		logger.V(1).Info("published inventory", "gpus", len(devices), "vfioReady", ready)
	}
	publish()

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-ticker.C:
			publish()
		}
	}
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

func main() {
	interval := flag.Duration("interval", 60*time.Second, "Discovery loop interval")
	klog.InitFlags(nil)
	flag.Parse()

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		klog.Fatal("NODE_NAME environment variable is required")
	}

	cfg := ctrl.GetConfigOrDie()
	k8s, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		klog.Fatalf("unable to create kubernetes client: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	klog.InfoS("gpu-discovery starting", "node", nodeName, "interval", interval.String())

	// Run discovery immediately on startup, then on interval.
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	runDiscovery(ctx, k8s, nodeName)
	for {
		select {
		case <-ctx.Done():
			klog.InfoS("shutting down")
			return
		case <-ticker.C:
			runDiscovery(ctx, k8s, nodeName)
		}
	}
}

func runDiscovery(ctx context.Context, k8s client.Client, nodeName string) {
	klog.InfoS("starting discovery cycle", "node", nodeName)

	// Discover hardware.
	discovered, err := discoverHardware()
	if err != nil {
		klog.ErrorS(err, "hardware discovery failed")
		patchPhase(ctx, k8s, nodeName, "Error")
		return
	}

	// Read existing SwiftGPUNode to preserve controller-owned fields.
	var existing gpuv1alpha1.SwiftGPUNode
	err = k8s.Get(ctx, client.ObjectKey{Name: nodeName}, &existing)
	if client.IgnoreNotFound(err) != nil {
		klog.ErrorS(err, "failed to get SwiftGPUNode")
		return
	}
	exists := err == nil

	// Build merged status.
	merged := mergeStatus(discovered, &existing.Status)

	if !exists {
		// Create the SwiftGPUNode resource.
		node := &gpuv1alpha1.SwiftGPUNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Labels: map[string]string{
					"kubeswift.io/gpu-node": "true",
				},
			},
		}
		if err := k8s.Create(ctx, node); err != nil {
			klog.ErrorS(err, "failed to create SwiftGPUNode")
			return
		}
		// Re-read to get resourceVersion for status patch.
		if err := k8s.Get(ctx, client.ObjectKey{Name: nodeName}, &existing); err != nil {
			klog.ErrorS(err, "failed to re-read SwiftGPUNode after create")
			return
		}
	}

	// Patch status subresource.
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Status = merged
	if err := k8s.Status().Patch(ctx, &existing, patch); err != nil {
		klog.ErrorS(err, "failed to patch SwiftGPUNode status")
		return
	}

	klog.InfoS("discovery cycle complete",
		"node", nodeName,
		"gpuCount", merged.GPUCount,
		"freeGPUs", merged.FreeGPUs,
		"phase", merged.Phase,
	)
}

func patchPhase(ctx context.Context, k8s client.Client, nodeName, phase string) {
	var node gpuv1alpha1.SwiftGPUNode
	if err := k8s.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.Phase = phase
	_ = k8s.Status().Patch(ctx, &node, patch)
}

// mergeStatus builds the new SwiftGPUNode status from discovered hardware,
// preserving controller-owned fields from the existing status.
func mergeStatus(discovered *SwiftGPUNodeStatus, existing *gpuv1alpha1.SwiftGPUNodeStatus) gpuv1alpha1.SwiftGPUNodeStatus {
	now := metav1.Now()

	status := gpuv1alpha1.SwiftGPUNodeStatus{
		Phase:         "Ready",
		LastDiscovery: &now,
		Host:          discovered.Host,
		NVSwitches:    discovered.NVSwitches,
	}

	// Build a map of existing GPU allocations keyed by PCI address.
	existingGPUs := map[string]*gpuv1alpha1.GPUDevice{}
	for i := range existing.GPUs {
		existingGPUs[existing.GPUs[i].PCIAddress] = &existing.GPUs[i]
	}

	// Build a map of existing FM partition allocations keyed by ID.
	existingPartitions := map[int]string{}
	if existing.FabricManager != nil {
		for _, p := range existing.FabricManager.Partitions {
			existingPartitions[p.ID] = p.AllocatedTo
		}
	}

	// Merge GPU list: discovery-owned fields from discovered, controller-owned from existing.
	gpus := make([]gpuv1alpha1.GPUDevice, len(discovered.GPUs))
	removedAllocated := []string{}
	for i, dg := range discovered.GPUs {
		gpus[i] = gpuv1alpha1.GPUDevice{
			Index:      dg.Index,
			PCIAddress: dg.PCIAddress,
			Vendor:     dg.Vendor,
			Model:      dg.Model,
			DeviceID:   dg.DeviceID,
			NUMANode:   dg.NUMANode,
			IOMMUGroup: dg.IOMMUGroup,
			Driver:     dg.Driver,
			BARSizes:   dg.BARSizes,
		}
		// Preserve controller-owned allocation fields.
		if eg, ok := existingGPUs[dg.PCIAddress]; ok {
			gpus[i].Allocated = eg.Allocated
			gpus[i].AllocatedTo = eg.AllocatedTo
		}
	}

	// Check for GPUs that were in existing but not in discovered.
	discoveredAddrs := map[string]bool{}
	for _, dg := range discovered.GPUs {
		discoveredAddrs[dg.PCIAddress] = true
	}
	for _, eg := range existing.GPUs {
		if !discoveredAddrs[eg.PCIAddress] && eg.Allocated {
			// An allocated GPU disappeared — flag it.
			removedAllocated = append(removedAllocated, eg.PCIAddress)
		}
	}

	if len(removedAllocated) > 0 {
		klog.ErrorS(nil, "allocated GPUs no longer detected by discovery — possible hardware removal",
			"pciAddresses", removedAllocated)
		status.Phase = "Error"
	}

	status.GPUs = gpus
	status.GPUCount = len(gpus)
	status.FreeGPUs = countFreeGPUs(gpus)
	if len(gpus) > 0 {
		status.GPUModel = gpus[0].Model
		status.GPUVendor = gpus[0].Vendor
	}

	// Merge Fabric Manager status.
	if discovered.FabricManager != nil {
		fm := *discovered.FabricManager
		for i := range fm.Partitions {
			if allocTo, ok := existingPartitions[fm.Partitions[i].ID]; ok {
				fm.Partitions[i].AllocatedTo = allocTo
			}
		}
		status.FabricManager = &fm
	}

	return status
}

func countFreeGPUs(gpus []gpuv1alpha1.GPUDevice) int {
	n := 0
	for _, g := range gpus {
		if !g.Allocated {
			n++
		}
	}
	return n
}

// SwiftGPUNodeStatus is the intermediate discovery result before merging
// with controller-owned fields.
type SwiftGPUNodeStatus struct {
	Host          gpuv1alpha1.HostTopology
	GPUs          []gpuv1alpha1.GPUDevice
	NVSwitches    []gpuv1alpha1.NVSwitchDevice
	FabricManager *gpuv1alpha1.FabricManagerStatus
}

// discoverHardware reads GPU inventory, NUMA topology, and Fabric Manager state
// from the host via sysfs and system commands.
func discoverHardware() (*SwiftGPUNodeStatus, error) {
	gpus, err := discoverGPUs()
	if err != nil {
		return nil, fmt.Errorf("GPU discovery: %w", err)
	}

	host, err := discoverHostTopology()
	if err != nil {
		return nil, fmt.Errorf("host topology discovery: %w", err)
	}

	// NVSwitch and Fabric Manager are NVIDIA-specific. Only discover them
	// when NVIDIA GPUs are present on the node.
	var nvSwitches []gpuv1alpha1.NVSwitchDevice
	var fm *gpuv1alpha1.FabricManagerStatus
	if HasNVIDIAGPUs(gpus) {
		nvSwitches, err = discoverNVSwitches()
		if err != nil {
			klog.V(2).InfoS("NVSwitch discovery skipped", "reason", err)
			nvSwitches = nil
		}

		fm, err = discoverFabricManager()
		if err != nil {
			klog.V(2).InfoS("Fabric Manager discovery skipped", "reason", err)
			fm = nil
		}
	} else {
		klog.V(2).InfoS("NVSwitch/Fabric Manager discovery skipped (no NVIDIA GPUs)")
	}

	return &SwiftGPUNodeStatus{
		Host:          host,
		GPUs:          gpus,
		NVSwitches:    nvSwitches,
		FabricManager: fm,
	}, nil
}

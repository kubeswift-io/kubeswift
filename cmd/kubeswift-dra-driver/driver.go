package main

import (
	"context"
	"encoding/json"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/projectbeskar/kubeswift/internal/gpualloc"
)

// driverName is the DRA driver name devices are published under and
// DeviceClasses select on (device.driver == driverName).
const driverName = "gpu.kubeswift.io"

// draDriver implements kubeletplugin.DRAPlugin for the KubeSwift reference
// GPU driver. The SCHEDULER allocates (structured parameters) — this plugin
// only (1) publishes the node's GPU inventory as ResourceSlices and (2) at
// pod-admission time writes the per-claim CDI spec that injects
// GPU_PCI_ADDRESSES into the claim's containers (design doc §A2/§A4).
type draDriver struct {
	nodeName string
	cdiDir   string
	kube     kubernetes.Interface
}

// PrepareResourceClaims implements kubeletplugin.DRAPlugin. For each claim it
// decodes the allocated BDFs from the device names (the §A3 naming contract),
// writes the per-claim CDI spec, and returns the CDI device ID for every
// allocated device result so kubelet wires the env into the claim's containers.
func (d *draDriver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		results[claim.UID] = d.prepareClaim(ctx, claim)
	}
	return results, nil
}

func (d *draDriver) prepareClaim(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	if claim.Status.Allocation == nil {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("claim %s/%s has no allocation", claim.Namespace, claim.Name)}
	}

	var bdfs []string
	var devices []kubeletplugin.Device
	for _, res := range claim.Status.Allocation.Devices.Results {
		if res.Driver != driverName {
			continue // another driver's request in the same claim
		}
		bdf, ok := gpualloc.DecodeDeviceName(res.Device)
		if !ok {
			return kubeletplugin.PrepareResult{Err: fmt.Errorf(
				"device %q does not use the %s naming scheme (gpu-<bdf>)", res.Device, driverName)}
		}
		bdfs = append(bdfs, bdf)
		devices = append(devices, kubeletplugin.Device{
			Requests:   []string{res.Request},
			PoolName:   res.Pool,
			DeviceName: res.Device,
		})
	}
	if len(bdfs) == 0 {
		return kubeletplugin.PrepareResult{} // nothing of ours in this claim
	}

	cdiID, err := writeClaimCDISpec(d.cdiDir, string(claim.UID), bdfs)
	if err != nil {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("write CDI spec: %w", err)}
	}
	// One CDI device per claim carries the combined env; attach its ID to the
	// FIRST device result only — kubelet unions CDI IDs across results into
	// the container config, and listing the same ID on every result would
	// duplicate it.
	devices[0].CDIDeviceIDs = []string{cdiID}

	// Best-effort: publish the BDFs in status.devices[].data (§A3 tier 1 —
	// what gpualloc.extractDeviceBDFs prefers). Requires the
	// DRAResourceClaimDeviceStatus feature; the name-encoding fallback covers
	// clusters without it, so failure here is logged, not fatal.
	if err := d.publishDeviceStatus(ctx, claim, bdfs); err != nil {
		klog.V(1).InfoS("device-status write skipped (name-encoding fallback covers read-back)",
			"claim", klog.KObj(claim), "err", err)
	}

	klog.InfoS("prepared claim", "claim", klog.KObj(claim), "bdfs", bdfs, "cdiID", cdiID)
	return kubeletplugin.PrepareResult{Devices: devices}
}

// UnprepareResourceClaims implements kubeletplugin.DRAPlugin: remove the
// per-claim CDI spec (idempotent — the claim may never have been prepared by
// this instance, e.g. across a driver restart).
func (d *draDriver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	results := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		results[claim.UID] = removeClaimCDISpec(d.cdiDir, string(claim.UID))
	}
	return results, nil
}

// HandleError implements kubeletplugin.DRAPlugin: background errors (e.g.
// ResourceSlice publishing). Retryable errors are logged and the helper keeps
// retrying; nothing here is fatal for a single-pool node-local inventory.
func (d *draDriver) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
}

// publishDeviceStatus writes AllocatedDeviceStatus.Data {"pciAddress": ...}
// for each of our allocated devices on the claim.
func (d *draDriver) publishDeviceStatus(ctx context.Context, claim *resourceapi.ResourceClaim, bdfs []string) error {
	fresh, err := d.kube.ResourceV1().ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if fresh.Status.Allocation == nil {
		return fmt.Errorf("claim lost its allocation")
	}
	i := 0
	for _, res := range fresh.Status.Allocation.Devices.Results {
		if res.Driver != driverName || i >= len(bdfs) {
			continue
		}
		data, _ := json.Marshal(map[string]string{"pciAddress": bdfs[i]})
		setAllocatedDeviceStatus(fresh, resourceapi.AllocatedDeviceStatus{
			Driver: res.Driver,
			Pool:   res.Pool,
			Device: res.Device,
			Data:   &runtime.RawExtension{Raw: data},
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "Prepared",
				LastTransitionTime: metav1.Now(),
				Message:            "vfio passthrough device prepared by the KubeSwift reference driver",
			}},
		})
		i++
	}
	_, err = d.kube.ResourceV1().ResourceClaims(claim.Namespace).UpdateStatus(ctx, fresh, metav1.UpdateOptions{})
	if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
		// Feature gate off or RBAC narrowed — non-fatal by design (§A3 tier 2).
		return err
	}
	return err
}

// setAllocatedDeviceStatus upserts one device entry on the claim status.
func setAllocatedDeviceStatus(claim *resourceapi.ResourceClaim, dev resourceapi.AllocatedDeviceStatus) {
	for i := range claim.Status.Devices {
		e := &claim.Status.Devices[i]
		if e.Driver == dev.Driver && e.Pool == dev.Pool && e.Device == dev.Device {
			claim.Status.Devices[i] = dev
			return
		}
	}
	claim.Status.Devices = append(claim.Status.Devices, dev)
}

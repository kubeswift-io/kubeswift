package swiftguest

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
)

// isGPUAllocated returns true when the GPUAllocated condition on the guest is True.
func isGPUAllocated(guest *swiftv1alpha1.SwiftGuest) bool {
	for _, c := range guest.Status.Conditions {
		if c.Type == swiftv1alpha1.ConditionGPUAllocated {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// buildGPUIntent constructs a GPUIntent for the RuntimeIntent by combining the
// guest's GPU allocation status, SwiftGPUProfile config, and SwiftGPUNode topology.
func (r *SwiftGuestReconciler) buildGPUIntent(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*runtimeintent.GPUIntent, error) {
	var profile gpuv1alpha1.SwiftGPUProfile
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: guest.Namespace,
		Name:      guest.Spec.GPUProfileRef.Name,
	}, &profile); err != nil {
		return nil, fmt.Errorf("load SwiftGPUProfile: %w", err)
	}

	var gpuNode gpuv1alpha1.SwiftGPUNode
	if err := r.Get(ctx, client.ObjectKey{Name: guest.Status.GPU.NodeName}, &gpuNode); err != nil {
		return nil, fmt.Errorf("load SwiftGPUNode: %w", err)
	}

	isQEMU := profile.Spec.Tier == "hgx-shared" || profile.Spec.Tier == "hgx-full"

	// Resolve PCIe topology flags from the profile.
	rootPortPerDevice := false
	noMmap := false
	gpuDirectClique := 0
	if profile.Spec.PCIeTopology != nil {
		rootPortPerDevice = profile.Spec.PCIeTopology.RootPortPerDevice
		noMmap = profile.Spec.PCIeTopology.NoMmap
		gpuDirectClique = profile.Spec.PCIeTopology.GPUDirectClique
	}

	// Build per-device intent, looking up NUMA affinity from the node inventory.
	devices := make([]runtimeintent.VFIODeviceIntent, 0, len(guest.Status.GPU.Devices))
	for _, pciAddr := range guest.Status.GPU.Devices {
		numaNode := 0
		for _, g := range gpuNode.Status.GPUs {
			if g.PCIAddress == pciAddr {
				numaNode = g.NUMANode
				break
			}
		}
		devices = append(devices, runtimeintent.VFIODeviceIntent{
			HostPath:        fmt.Sprintf("/sys/bus/pci/devices/%s/", pciAddr),
			PCIAddress:      pciAddr,
			PCIeRootPort:    rootPortPerDevice,
			GPUDirectClique: gpuDirectClique,
			NoMmap:          noMmap,
			NUMANode:        numaNode,
		})
	}

	// Firmware selection.
	firmware := "hypervisor-fw"
	if isQEMU {
		firmware = "ovmf"
	}

	// Hugepage size conversion: Kubernetes format → QEMU format.
	hugepages := ""
	switch profile.Spec.Hugepages {
	case "1Gi":
		hugepages = "1G"
	case "2Mi":
		hugepages = "2M"
	}

	// NUMA intent and vCPU pinning (QEMU only, when NUMATopology is configured).
	var numaIntent *runtimeintent.NUMAIntent
	var vcpuPins []runtimeintent.VCPUPin
	if isQEMU && profile.Spec.NUMATopology != nil {
		numaIntent, vcpuPins = buildNUMAAndPinning(
			profile.Spec.NUMATopology,
			guest.Status.GPU.NUMANodes,
			gpuNode.Status.Host.NUMANodes,
			profile.Spec.VCPUPinning,
		)
	}

	// Validate that the FM partition (if any) is still allocated to this guest.
	// This guards against stale status.gpu data activating a partition that was
	// reassigned to another tenant after this guest's allocation.
	fmPartitionID := guest.Status.GPU.PartitionID
	if fmPartitionID >= 0 {
		allocatedTo := guest.Namespace + "/" + guest.Name
		if !isFMPartitionOwnedBy(gpuNode.Status.FabricManager, fmPartitionID, allocatedTo) {
			return nil, fmt.Errorf("FM partition %d is not allocated to %s on node %s — possible stale allocation",
				fmPartitionID, allocatedTo, gpuNode.Name)
		}
	}

	return &runtimeintent.GPUIntent{
		Devices:                  devices,
		Firmware:                 firmware,
		NUMA:                     numaIntent,
		VCPUPinning:              vcpuPins,
		Hugepages:                hugepages,
		FabricManagerPartitionID: fmPartitionID,
	}, nil
}

// isFMPartitionOwnedBy checks that the given FM partition on the node is still
// allocated to the expected guest (namespace/name). Returns false if the FM
// status is nil, the partition doesn't exist, or it's allocated to someone else.
func isFMPartitionOwnedBy(fm *gpuv1alpha1.FabricManagerStatus, partitionID int, allocatedTo string) bool {
	if fm == nil {
		return false
	}
	for _, p := range fm.Partitions {
		if p.ID == partitionID {
			return p.AllocatedTo == allocatedTo
		}
	}
	return false
}

// buildNUMAAndPinning computes the virtual NUMA topology and optional vCPU→pCPU
// pinning map from profile spec and physical node topology.
func buildNUMAAndPinning(
	numaSpec *gpuv1alpha1.NUMATopologySpec,
	allocatedNUMANodes []int,
	hostNUMANodes []gpuv1alpha1.NUMANodeInfo,
	vcpuPinning bool,
) (*runtimeintent.NUMAIntent, []runtimeintent.VCPUPin) {
	coresPerNode := numaSpec.CoresPerSocket * numaSpec.ThreadsPerCore

	// Build sequential virtual NUMA nodes.
	nodes := make([]runtimeintent.NUMANodeIntent, numaSpec.Sockets)
	for i := 0; i < numaSpec.Sockets; i++ {
		start := i * coresPerNode
		end := start + coresPerNode - 1
		cpuRange := fmt.Sprintf("%d-%d", start, end)
		if coresPerNode == 1 {
			cpuRange = strconv.Itoa(start)
		}
		nodes[i] = runtimeintent.NUMANodeIntent{
			ID:       i,
			CPUs:     cpuRange,
			MemoryMi: numaSpec.MemoryPerSocketMi,
		}
	}

	numaIntent := &runtimeintent.NUMAIntent{Nodes: nodes}

	if !vcpuPinning || len(allocatedNUMANodes) == 0 {
		return numaIntent, nil
	}

	// Build a lookup for physical NUMA node CPU ranges.
	physNUMAByCPUs := map[int]string{} // physNUMAID → CPU range string
	for _, n := range hostNUMANodes {
		physNUMAByCPUs[n.ID] = n.CPUs
	}

	// Map virtual NUMA node i → physical NUMA node allocatedNUMANodes[i].
	var pins []runtimeintent.VCPUPin
	for vNUMA := 0; vNUMA < numaSpec.Sockets; vNUMA++ {
		physNUMAID := -1
		if vNUMA < len(allocatedNUMANodes) {
			physNUMAID = allocatedNUMANodes[vNUMA]
		} else if len(allocatedNUMANodes) > 0 {
			// More virtual sockets than physical NUMA nodes: share the last one.
			physNUMAID = allocatedNUMANodes[len(allocatedNUMANodes)-1]
		}
		if physNUMAID < 0 {
			continue
		}

		cpuStr, ok := physNUMAByCPUs[physNUMAID]
		if !ok {
			continue
		}
		physCPUs, err := expandCPURange(cpuStr)
		if err != nil || len(physCPUs) == 0 {
			continue
		}

		vCPUStart := vNUMA * coresPerNode
		for j := 0; j < coresPerNode && j < len(physCPUs); j++ {
			pins = append(pins, runtimeintent.VCPUPin{
				VCPU:    vCPUStart + j,
				HostCPU: physCPUs[j],
			})
		}
	}

	return numaIntent, pins
}

// expandCPURange converts a Linux CPU range string (e.g. "0-47,96-143") into
// a sorted slice of individual CPU numbers.
func expandCPURange(s string) ([]int, error) {
	var result []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			lo, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
			if err != nil {
				return nil, fmt.Errorf("invalid CPU range %q: %w", part, err)
			}
			hi, err := strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid CPU range %q: %w", part, err)
			}
			for i := lo; i <= hi; i++ {
				result = append(result, i)
			}
		} else {
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid CPU number %q: %w", part, err)
			}
			result = append(result, n)
		}
	}
	return result, nil
}

// buildPod dispatches to the appropriate pod builder based on whether the guest
// uses GPU passthrough, kernel boot, or standard disk boot.
func (r *SwiftGuestReconciler) buildPod(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
	seedConfigMapName, intentConfigMapName string,
) (*corev1.Pod, error) {
	if guest.Spec.GPUProfileRef != nil && guest.Status.GPU != nil {
		// Load profile to get hugepages size for the pod spec.
		var profile gpuv1alpha1.SwiftGPUProfile
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: guest.Namespace,
			Name:      guest.Spec.GPUProfileRef.Name,
		}, &profile); err != nil {
			return nil, fmt.Errorf("load SwiftGPUProfile for pod: %w", err)
		}
		return BuildGPUDiskBootPod(guest, rg, seedConfigMapName, intentConfigMapName, profile.Spec.Hugepages), nil
	}
	return BuildPod(guest, rg, seedConfigMapName, intentConfigMapName), nil
}

// BuildGPUDiskBootPod constructs a launcher pod for a GPU-backed SwiftGuest.
// guest.Status.GPU must be populated (GPUAllocated=True) before calling this.
// hugepages is the Kubernetes quantity string ("1Gi", "2Mi", or "") from the profile.
func BuildGPUDiskBootPod(
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
	seedConfigMapName, intentConfigMapName string,
	hugepages string,
) *corev1.Pod {
	cpu := rg.Resources.CPU
	if cpu < 1 {
		cpu = 1
	}
	mem := rg.Resources.Memory
	if mem < 128 {
		mem = 128
	}

	// Base volumes: same as disk boot.
	volumes := []corev1.Volume{
		{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{
			Name: "root-disk",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: rg.PreparedImage.PVCName,
				},
			},
		},
		{
			Name: "runtime-intent",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: intentConfigMapName},
				},
			},
		},
		{
			Name: "dev-kvm",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/dev/kvm",
					Type: ptr.To(corev1.HostPathType("CharDevice")),
				},
			},
		},
		// /dev/vfio provides VFIO character devices for IOMMU group access.
		// Ideally this would be scoped to specific IOMMU group devices (e.g.
		// /dev/vfio/15), but the VFIO group device files are created by the kernel
		// during gpu-init's driver bind — they may not exist when the pod spec is
		// generated. Mounting the directory is the pragmatic choice; IOMMU hardware
		// isolation prevents cross-group DMA regardless.
		{
			Name: "dev-vfio",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/dev/vfio",
					Type: ptr.To(corev1.HostPathDirectory),
				},
			},
		},
		// /sys/bus/pci is needed by gpu-init to bind devices to vfio-pci via sysfs
		// (driver_override, drivers_probe, driver/unbind). Without this volume, sysfs
		// writes fail even with SYS_ADMIN because the container's mount namespace
		// does not include the host sysfs.
		{
			Name: "sysfs-pci",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/sys/bus/pci",
					Type: ptr.To(corev1.HostPathDirectory),
				},
			},
		},
	}
	if rg.HasSeed() {
		AddSeedVolume(&volumes, seedConfigMapName)
	}

	// Hugepage volume for GPU workloads.
	if hugepages != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "hugepages",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMedium("HugePages-" + hugepages),
				},
			},
		})
	}

	// Standard launcher mounts.
	var mounts []corev1.VolumeMount
	AddVolumeMounts(&mounts, rg.HasSeed())
	mounts = append(mounts, corev1.VolumeMount{Name: "dev-vfio", MountPath: "/dev/vfio"})
	if hugepages != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: "hugepages", MountPath: "/dev/hugepages"})
	}

	// gpu-init runs before network-init: binds devices to vfio-pci and activates
	// the Fabric Manager partition (if applicable) before swiftletd starts.
	gpuAddresses := ""
	partitionID := -1
	if guest.Status.GPU != nil {
		gpuAddresses = strings.Join(guest.Status.GPU.Devices, ",")
		partitionID = guest.Status.GPU.PartitionID
	}

	gpuInitContainer := corev1.Container{
		Name:            "gpu-init",
		Image:           LauncherImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/usr/local/bin/gpu-init.sh"},
		Env: []corev1.EnvVar{
			{Name: "GPU_PCI_ADDRESSES", Value: gpuAddresses},
			{Name: "GPU_PARTITION_ID", Value: strconv.Itoa(partitionID)},
		},
		SecurityContext: gpuInitSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "dev-vfio", MountPath: "/dev/vfio"},
			{Name: "sysfs-pci", MountPath: "/sys/bus/pci"},
		},
	}

	var initContainers []corev1.Container
	initContainers = append(initContainers, gpuInitContainer)
	if rg.HasNetwork() {
		initContainers = append(initContainers, networkInitContainer())
		volumes = append(volumes, corev1.Volume{
			Name: "dev-net-tun",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/dev/net/tun",
					Type: ptr.To(corev1.HostPathType("CharDevice")),
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "dev-net-tun", MountPath: "/dev/net/tun"})
	}

	// Resource requests for launcher container.
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpu), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(mem)*1024*1024, resource.BinarySI),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpu), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(mem)*1024*1024, resource.BinarySI),
		},
	}

	nodeName := ""
	if guest.Status.GPU != nil {
		nodeName = guest.Status.GPU.NodeName
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      guest.Name,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guest.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: initContainers,
			// Pin to the specific node where GPUs were allocated.
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": nodeName,
			},
			Containers: []corev1.Container{
				{
					Name:            "launcher",
					Image:           LauncherImage(),
					ImagePullPolicy: corev1.PullIfNotPresent,
					SecurityContext: launcherSecurityContext(true),
					Env: []corev1.EnvVar{
						{
							Name: "POD_NAME",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
							},
						},
						{
							Name: "POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
							},
						},
					},
					Resources:    resources,
					VolumeMounts: mounts,
				},
			},
			Volumes: volumes,
		},
	}
}

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

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
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

// buildDRAGPUIntent constructs the GPUIntent for a DRA-backed guest
// (spec.gpuResourceClaim). Unlike the native path there is no SwiftGPUProfile
// and possibly no SwiftGPUNode, and the controller CANNOT know the devices when
// it writes the intent — the scheduler + DRA driver allocate at pod-schedule
// time and the reference driver's CDI containerEdits inject GPU_PCI_ADDRESSES
// into the containers. deviceSource: "env" tells swiftletd to synthesize the
// device list from that env var (clique -1, NUMA 0 in v1).
func buildDRAGPUIntent(rc *swiftv1alpha1.GPUResourceClaimSpec) *runtimeintent.GPUIntent {
	firmware := "cloudhv"
	if rc.Tier == "hgx-shared" || rc.Tier == "hgx-full" {
		firmware = "ovmf"
	}
	hugepages := ""
	switch rc.Hugepages {
	case "1Gi":
		hugepages = "1G"
	case "2Mi":
		hugepages = "2M"
	}
	return &runtimeintent.GPUIntent{
		Devices:                  nil,
		DeviceSource:             "env",
		Firmware:                 firmware,
		Hugepages:                hugepages,
		FabricManagerPartitionID: -1,
	}
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

	// x_nv_gpudirect_clique is NVIDIA-specific. For non-NVIDIA GPUs, set to -1
	// so swiftletd omits the flag from the hypervisor command line.
	isNVIDIA := gpuNode.Status.GPUVendor == "NVIDIA"
	if !isNVIDIA {
		gpuDirectClique = -1
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
	firmware := "cloudhv"
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
//
// rootDiskClone is non-nil for disk-boot guests after EnsureRootDiskClone
// has succeeded. It controls whether a clone-grow-init init container is
// added on the snapshot clone strategy.
// buildPod resolves the launcher pod and, when Phase 3c mTLS is enabled,
// injects the idle source-side stunnel client sidecar for migration-
// eligible guests. The base-pod construction lives in buildBasePod; this
// thin wrapper keeps the sidecar concern (and the r-scoped mTLS flag) out
// of the three boot-path branches.
func (r *SwiftGuestReconciler) buildPod(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
	seedConfigMapName, intentConfigMapName string,
	rootDiskClone *RootDiskCloneResult,
) (*corev1.Pod, error) {
	pod, err := r.buildBasePod(ctx, guest, rg, seedConfigMapName, intentConfigMapName, rootDiskClone)
	if err != nil {
		return nil, err
	}
	if r.MigrationMTLSEnabled && migrationEligible(guest) {
		applyMigrationSourceSidecar(pod, guest)
	}
	return pod, nil
}

func (r *SwiftGuestReconciler) buildBasePod(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
	seedConfigMapName, intentConfigMapName string,
	rootDiskClone *RootDiskCloneResult,
) (*corev1.Pod, error) {
	// Tier B (local-backend) restore: the SwiftRestore controller has
	// stamped the SwiftGuest with snapshot.kubeswift.io/active-restore.
	// The restore-mode launcher mounts the snapshot directory hostPath,
	// optionally stages+patches it via the snapshot-stager init
	// container, and runs CH in --restore mode. See restore.go.
	//
	// The webhook rejects gpuProfileRef + Tier B at admission time
	// (Phase 0 spike Constraint #1: VFIO + memory snapshot fails on
	// restore with `bar 0 already used`), so the restore branch sits
	// before the GPU branch and the two are mutually exclusive in
	// practice.
	// Precedence consistency assertion: when both spec.NodeName and a GPU
	// allocation are present, they MUST agree (status.GPU.NodeName is the
	// binding source for which GPUs gpu-init binds; spec.NodeName pins the
	// pod). The VFIO release-and-reallocate offline migration cutover commits
	// BOTH together (ReleaseFromNode(source) + stamp status.GPU=target, THEN
	// patch spec.NodeName=target), so by the time the dst pod is built they
	// agree. A disagreement here is therefore a real bug or an out-of-band
	// edit — refuse to build, surfaced as Resolved=False via the controller's
	// ResolutionError mapping.
	//
	// LOAD-BEARING (W26-class): do NOT weaken this to "trust spec.NodeName
	// alone" — status.GPU.NodeName must stay the binding source, or a
	// half-committed cutover could bind the wrong node's GPUs.
	if guest.Spec.NodeName != "" &&
		guest.Status.GPU != nil &&
		guest.Status.GPU.NodeName != "" &&
		guest.Status.GPU.NodeName != guest.Spec.NodeName {
		return nil, fmt.Errorf("spec.nodeName=%q disagrees with status.gpu.nodeName=%q; the GPU migration cutover must commit both together",
			guest.Spec.NodeName, guest.Status.GPU.NodeName)
	}

	if params, ok := RestoreParamsFromAnnotations(guest.Annotations); ok {
		pod := BuildRestorePod(guest, rg, seedConfigMapName, intentConfigMapName, rootDiskClone, params)
		applyNodeName(pod, guest)
		return pod, nil
	}
	if guest.Spec.GPUProfileRef != nil && guest.Status.GPU != nil {
		// Load profile to get hugepages size for the pod spec.
		var profile gpuv1alpha1.SwiftGPUProfile
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: guest.Namespace,
			Name:      guest.Spec.GPUProfileRef.Name,
		}, &profile); err != nil {
			return nil, fmt.Errorf("load SwiftGPUProfile for pod: %w", err)
		}
		// GPU pods pin via kubernetes.io/hostname=<status.GPU.NodeName>
		// nodeSelector. We do not also set pod.Spec.NodeName because the
		// existing GPU dispatch path handled it through the selector;
		// adding direct binding here would risk regression on the GPU
		// e2e validated on Hetzner. The precedence check above ensures
		// spec.NodeName (if set) matches GPU.NodeName, so the effective
		// pinned node is the same either way.
		return BuildGPUDiskBootPod(guest, rg, seedConfigMapName, intentConfigMapName, profile.Spec.Hugepages, rootDiskClone), nil
	}
	if guest.Spec.GPUResourceClaim != nil {
		// DRA backend: the pod is built BEFORE allocation (no status.GPU, no
		// SwiftGPUProfile) — claim-bearing and unpinned; the scheduler + DRA
		// driver pick node+device and the driver's CDI containerEdits inject
		// the device identity. applyDRAClaim (inside the builder) handles the
		// DRA mutations. Hugepages come from the claim spec.
		return BuildGPUDiskBootPod(guest, rg, seedConfigMapName, intentConfigMapName, guest.Spec.GPUResourceClaim.Hugepages, rootDiskClone), nil
	}
	return BuildPod(guest, rg, seedConfigMapName, intentConfigMapName, rootDiskClone), nil
}

// BuildGPUDiskBootPod constructs a launcher pod for a GPU-backed SwiftGuest.
// guest.Status.GPU must be populated (GPUAllocated=True) before calling this.
// hugepages is the Kubernetes quantity string ("1Gi", "2Mi", or "") from the profile.
//
// rootDiskClone follows the same contract as BuildPod: when non-nil with
// NeedsGrowInit=true, a clone-grow-init init container runs before gpu-init.
func BuildGPUDiskBootPod(
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
	seedConfigMapName, intentConfigMapName string,
	hugepages string,
	rootDiskClone *RootDiskCloneResult,
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
		// Uses DirectoryOrCreate because /dev/vfio does not exist on the host
		// until the first device is bound to vfio-pci — which gpu-init does.
		// IOMMU hardware isolation prevents cross-group DMA regardless of
		// mounting the full directory.
		{
			Name: "dev-vfio",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/dev/vfio",
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		},
		// /sys is needed by gpu-init to bind devices to vfio-pci via sysfs
		// (driver_override, drivers_probe, driver/unbind). We mount the full host
		// sysfs (not just /sys/bus/pci) because device symlinks under /sys/bus/pci/devices/
		// resolve to paths under /sys/devices/ which must also be accessible.
		// Mounted at /host/sys to avoid being shadowed by the container's own /sys.
		{
			Name: "host-sys",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/sys",
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

	volumes = append(volumes, dataDiskVolumes(rg)...)

	// Standard launcher mounts.
	var mounts []corev1.VolumeMount
	var volumeDevices []corev1.VolumeDevice
	AddVolumeMounts(&mounts, &volumeDevices, rg, rg.HasSeed())
	mounts = append(mounts, corev1.VolumeMount{Name: "dev-vfio", MountPath: "/dev/vfio"})
	if hugepages != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: "hugepages", MountPath: "/dev/hugepages"})
	}
	mounts = append(mounts, dataDiskMounts(rg)...)
	volumeDevices = append(volumeDevices, dataDiskDevices(rg)...)
	// /var/lib/kubeswift/snapshots/ — writable hostPath. Tier B
	// captures on GPU guests are rejected by the webhook (Phase 0
	// Constraint #1: VFIO + memory snapshot fails on restore), but
	// the mount is present for symmetry and to support potential
	// future disk-only Tier B captures of GPU guests.
	AddSnapshotsHostPathMount(&volumes, &mounts)

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
		Command:         []string{"/bin/bash", "/usr/local/bin/gpu-init.sh"},
		Env: []corev1.EnvVar{
			{Name: "GPU_PCI_ADDRESSES", Value: gpuAddresses},
			{Name: "GPU_PARTITION_ID", Value: strconv.Itoa(partitionID)},
		},
		SecurityContext: gpuInitSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "dev-vfio", MountPath: "/dev/vfio"},
			{Name: "host-sys", MountPath: "/host/sys"},
		},
	}

	var initContainers []corev1.Container
	if rootDiskClone != nil && rootDiskClone.NeedsGrowInit {
		initContainers = append(initContainers, cloneGrowInitContainer(rg, rootDiskClone.TargetSizeBytes))
	}
	initContainers = append(initContainers, gpuInitContainer)
	if rg.HasNetwork() {
		initContainers = append(initContainers, networkInitContainer())
	}

	// Resource requests for launcher container.
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpu), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(mem+LauncherMemoryOverheadMiB)*1024*1024, resource.BinarySI),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpu), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(mem+LauncherMemoryOverheadMiB)*1024*1024, resource.BinarySI),
		},
	}
	AddSRIOVResourceLimits(&resources, guest)

	nodeName := ""
	if guest.Status.GPU != nil {
		nodeName = guest.Status.GPU.NodeName
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        guest.Name,
			Namespace:   guest.Namespace,
			Annotations: podAnnotations(guest),
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
					Resources:     resources,
					VolumeMounts:  mounts,
					VolumeDevices: volumeDevices,
				},
			},
			Volumes: volumes,
		},
	}

	if guest.Spec.GPUResourceClaim != nil {
		applyDRAClaim(pod, guest.Spec.GPUResourceClaim)
	}
	return pod
}

// draClaimPodName is the pod-local name under which the guest's GPU
// ResourceClaim is referenced (pod.spec.resourceClaims[].name and the
// containers' resources.claims[].name).
const draClaimPodName = "gpu"

// applyDRAClaim mutates a GPU launcher pod for the DRA backend (DRA Workstream
// A; design doc §A2/§A6):
//
//   - UNPIN it: the scheduler + DRA driver pick the node at schedule time, so
//     the native kubernetes.io/hostname pin (built from status.GPU.NodeName,
//     which is empty pre-schedule) must go.
//   - Carry the ResourceClaim: pod.spec.resourceClaims + resources.claims on
//     the gpu-init and launcher containers — referencing the claim is what
//     makes kubelet apply the driver's CDI containerEdits to them.
//   - Drop the GPU_PCI_ADDRESSES / GPU_PARTITION_ID envs from gpu-init: the
//     controller cannot know the devices at build time; the reference driver's
//     CDI spec injects both envs at container create. Setting them here (empty)
//     would shadow the CDI-injected values.
func applyDRAClaim(pod *corev1.Pod, rc *swiftv1alpha1.GPUResourceClaimSpec) {
	pod.Spec.NodeSelector = nil

	claim := corev1.PodResourceClaim{Name: draClaimPodName}
	if rc.ResourceClaimTemplateName != "" {
		claim.ResourceClaimTemplateName = &rc.ResourceClaimTemplateName
	} else {
		claim.ResourceClaimName = &rc.ResourceClaimName
	}
	pod.Spec.ResourceClaims = []corev1.PodResourceClaim{claim}

	ref := corev1.ResourceClaim{Name: draClaimPodName, Request: rc.RequestName}
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.Name != "gpu-init" {
			continue
		}
		var env []corev1.EnvVar
		for _, e := range c.Env {
			if e.Name == "GPU_PCI_ADDRESSES" || e.Name == "GPU_PARTITION_ID" {
				continue
			}
			env = append(env, e)
		}
		c.Env = env
		c.Resources.Claims = append(c.Resources.Claims, ref)
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "launcher" {
			pod.Spec.Containers[i].Resources.Claims = append(pod.Spec.Containers[i].Resources.Claims, ref)
		}
	}
}

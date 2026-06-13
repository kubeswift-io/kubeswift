package swiftguest

import (
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
)

// IntentConfigMapSuffix is the suffix for the runtime intent ConfigMap name.
const IntentConfigMapSuffix = "-runtime-intent"

// LauncherMemoryOverheadMiB is the extra memory added to the container limit
// beyond the guest memory. Accounts for the hypervisor process (CH or QEMU),
// swiftletd, dnsmasq, and kernel cgroup overhead. Without this, the container
// is OOMKilled because the guest RAM is allocated inside the container's cgroup.
const LauncherMemoryOverheadMiB = 512

// BuildPod creates a pod spec for the SwiftGuest.
//
// rootDiskClone is non-nil for disk-boot guests after EnsureRootDiskClone
// has succeeded. When rootDiskClone.NeedsGrowInit is true (snapshot clone
// strategy), an extra clone-grow-init init container is added before
// network-init to run qemu-img resize + sgdisk -e on the per-guest clone.
// nil is the legacy/copy path and is treated as "no grow-init needed".
func BuildPod(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest, seedConfigMapName, intentConfigMapName string, rootDiskClone *RootDiskCloneResult) *corev1.Pod {
	var pod *corev1.Pod
	if rg.HasKernel() {
		pod = buildKernelBootPod(guest, rg, intentConfigMapName)
	} else {
		pod = buildDiskBootPod(guest, rg, seedConfigMapName, intentConfigMapName, rootDiskClone)
	}
	applyTopologyConstraints(pod, guest)
	applyNodeName(pod, guest)
	applyDataDiskRefs(pod, guest)
	applyFilesystems(pod, guest)
	applyVhostUserSocketVolumes(pod, guest)
	applyExposedPorts(pod, guest)
	return pod
}

// applyExposedPorts declares launcher containerPorts for each spec.network.ports
// entry and sets a readiness probe on the first TCP port. The kubelet probes the
// pod IP:port, which the in-pod DNAT (network-init.sh) forwards to the VM, so the
// launcher pod is Ready — and thus the per-guest Service endpoint is Ready — only
// once the in-guest service is actually listening. nat binding only; no-op
// otherwise. See docs/design/service-exposure.md.
func applyExposedPorts(pod *corev1.Pod, guest *swiftv1alpha1.SwiftGuest) {
	if guest.Spec.Network == nil || len(guest.Spec.Network.Ports) == 0 {
		return
	}
	if guest.Spec.Network.Binding == "bridge" {
		return
	}
	if len(pod.Spec.Containers) == 0 {
		return
	}
	c := &pod.Spec.Containers[0]
	var firstTCP *swiftv1alpha1.GuestPort
	for i := range guest.Spec.Network.Ports {
		p := &guest.Spec.Network.Ports[i]
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		c.Ports = append(c.Ports, corev1.ContainerPort{
			Name:          p.Name,
			ContainerPort: p.Port,
			Protocol:      proto,
		})
		if firstTCP == nil && proto == corev1.ProtocolTCP {
			firstTCP = p
		}
	}
	// Honest endpoint readiness: probe the first TCP port. It succeeds only once
	// the in-guest service is up (the DNAT forwards to the VM). Readiness-only —
	// a failing probe never restarts the launcher (no liveness change).
	if firstTCP != nil && c.ReadinessProbe == nil {
		c.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(firstTCP.Port)},
			},
			InitialDelaySeconds: 15,
			PeriodSeconds:       10,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		}
	}
}

// applyVhostUserSocketVolumes mounts the node directory holding each
// operator-provided vhost-user backend socket into the launcher at the SAME
// path, so Cloud Hypervisor (running in the launcher container) can connect to
// the listener. This covers BOTH vhost-user-net interfaces (type: vhost-user)
// and vhost-user devices (spec.vhostUserDevices — blk and generic). KubeSwift
// does not run the backend — the directory is a hostPath the operator's
// datapath (DPDK/OVS/SPDK) populates, the same posture as SR-IOV expecting
// pre-provisioned VFs.
//
// Directories are deduped across BOTH sources in a single pass: a vhost-user
// NIC socket and a vhost-user-blk socket under the same directory must mount
// that directory exactly once (two mounts at the same path would be rejected
// at pod admission).
func applyVhostUserSocketVolumes(pod *corev1.Pod, guest *swiftv1alpha1.SwiftGuest) {
	if len(pod.Spec.Containers) == 0 {
		return
	}
	sockets := make([]string, 0, len(guest.Spec.Interfaces)+len(guest.Spec.VhostUserDevices))
	for _, iface := range guest.Spec.Interfaces {
		if iface.Type == swiftv1alpha1.InterfaceTypeVhostUser && iface.Socket != "" {
			sockets = append(sockets, iface.Socket)
		}
	}
	for _, d := range guest.Spec.VhostUserDevices {
		if d.Socket != "" {
			sockets = append(sockets, d.Socket)
		}
	}

	hostPathType := corev1.HostPathDirectoryOrCreate
	seen := map[string]struct{}{}
	for _, sock := range sockets {
		dir := filepath.Dir(sock)
		if _, ok := seen[dir]; ok {
			continue
		}
		volName := fmt.Sprintf("vhost-sock-%d", len(seen))
		seen[dir] = struct{}{}
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: dir, Type: &hostPathType},
			},
		})
		pod.Spec.Containers[0].VolumeMounts = append(
			pod.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: volName, MountPath: dir},
		)
	}
}

// applyFilesystems adds the source volume + mount for each virtiofs share in
// spec.filesystems. The source (node-local hostPath or a PVC) is mounted into
// the launcher container at runtimeintent.VirtiofsBasePath/<name>, which
// swiftletd hands virtiofsd as --shared-dir. ReadOnly on the share becomes a
// read-only mount — that is the enforcement (virtiofsd cannot widen a
// read-only mount). The virtiofsd socket is NOT a volume; swiftletd binds it
// in the runtime dir (an existing emptyDir already mounted in the container).
func applyFilesystems(pod *corev1.Pod, guest *swiftv1alpha1.SwiftGuest) {
	if len(pod.Spec.Containers) == 0 {
		return
	}
	hostPathType := corev1.HostPathDirectoryOrCreate
	for i, fs := range guest.Spec.Filesystems {
		volName := fmt.Sprintf("virtiofs-%d", i)
		mountPath := runtimeintent.VirtiofsBasePath + "/" + fs.Name

		var src corev1.VolumeSource
		switch {
		case fs.Source.HostPath != nil:
			src = corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: *fs.Source.HostPath,
					Type: &hostPathType,
				},
			}
		case fs.Source.PVCRef != nil:
			src = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: fs.Source.PVCRef.Name,
					ReadOnly:  fs.ReadOnly,
				},
			}
		default:
			// Webhook rejects this; skip defensively rather than panic.
			continue
		}

		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name:         volName,
			VolumeSource: src,
		})
		pod.Spec.Containers[0].VolumeMounts = append(
			pod.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: volName, MountPath: mountPath, ReadOnly: fs.ReadOnly},
		)
	}
}

// applyTopologyConstraints copies topology spread constraints from the SwiftGuest
// spec to the launcher pod. Typically set by SwiftGuestPool controller.
func applyTopologyConstraints(pod *corev1.Pod, guest *swiftv1alpha1.SwiftGuest) {
	if len(guest.Spec.TopologySpreadConstraints) > 0 {
		pod.Spec.TopologySpreadConstraints = guest.Spec.TopologySpreadConstraints
	}
}

// applyNodeName pins the launcher pod to a specific node when
// guest.Spec.NodeName is set. Direct binding via pod.Spec.NodeName
// (not via a kubernetes.io/hostname nodeSelector) is deliberate — it
// gives fast kubelet-time rejection on bad fits, which the
// SwiftMigration controller relies on for clean failure detection.
//
// pod.Spec.NodeName is set on Create only — it is immutable post-binding
// in Kubernetes. The SwiftMigration StopAndCopy phase (commit 8) relies
// on this contract: the controller delete-then-recreates the pod to
// move the guest, never patches an in-flight pod's nodeName.
//
// For kernel-boot pods, the existing kubeswift.io/kernel-node=true
// nodeSelector is preserved as defense-in-depth: the SwiftMigration
// validation webhook ensures the chosen NodeName is a kernel node, but
// leaving the selector in place catches any webhook bypass (the pod
// will fail to admit on a non-kernel node — which is the correct
// failure mode rather than silently running on a node that lacks the
// /var/lib/kubeswift/kernels/ hostPath).
//
// For GPU-backed pods, BuildGPUDiskBootPod's existing kubernetes.io/
// hostname=<status.GPU.NodeName> nodeSelector path is unchanged. The
// dispatcher (buildPod in gpu.go) enforces NodeName == GPU.NodeName
// when both are set; if they disagree the dispatcher returns an
// error and applyNodeName never runs.
func applyNodeName(pod *corev1.Pod, guest *swiftv1alpha1.SwiftGuest) {
	if guest.Spec.NodeName != "" {
		pod.Spec.NodeName = guest.Spec.NodeName
	}
}

// applyDataDiskRefs adds PVC volumes and mounts for dataDiskRefs with pvcRef.
// ImageRef-backed dataDiskRefs are resolved by the resolver, not here.
func applyDataDiskRefs(pod *corev1.Pod, guest *swiftv1alpha1.SwiftGuest) {
	for i, ref := range guest.Spec.DataDiskRefs {
		if ref.PVCRef == nil {
			continue
		}
		volName := fmt.Sprintf("data-disk-pvc-%d", i)
		mountPath := fmt.Sprintf("/var/lib/kubeswift/disks/pvc-%s", ref.Name)

		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: ref.PVCRef.Name,
				},
			},
		})
		if len(pod.Spec.Containers) > 0 {
			pod.Spec.Containers[0].VolumeMounts = append(
				pod.Spec.Containers[0].VolumeMounts,
				corev1.VolumeMount{Name: volName, MountPath: mountPath},
			)
		}
	}
}

// podAnnotations returns the base annotations for a launcher pod,
// including the Multus networks annotation if secondary NICs are configured.
func podAnnotations(guest *swiftv1alpha1.SwiftGuest) map[string]string {
	multus := BuildMultusAnnotation(guest)
	if multus == "" {
		return nil
	}
	return map[string]string{
		MultusAnnotationKey: multus,
	}
}

func buildKernelBootPod(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest, intentConfigMapName string) *corev1.Pod {
	volumes := []corev1.Volume{
		{
			Name: "run",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
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
		{
			Name: "kernel-artifacts",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: rg.KernelBoot.LocalPath,
					Type: ptr.To(corev1.HostPathDirectory),
				},
			},
		},
	}

	if rg.HasDataDisk() {
		volumes = append(volumes, corev1.Volume{
			Name: "data-disk",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: rg.GetDataDiskPVCName(),
				},
			},
		})
	}

	mounts := []corev1.VolumeMount{
		{Name: "run", MountPath: RunDirPath},
		{Name: "runtime-intent", MountPath: IntentPath},
		{Name: "dev-kvm", MountPath: "/dev/kvm"},
		{Name: "kernel-artifacts", MountPath: rg.KernelBoot.LocalPath},
	}
	if rg.HasDataDisk() {
		mounts = append(mounts, corev1.VolumeMount{Name: "data-disk", MountPath: DisksDataPath})
	}

	// /var/lib/kubeswift/snapshots/ — writable hostPath so the launcher's
	// CH process can write Tier B snapshot directories when the
	// SwiftSnapshot controller triggers a capture action.
	AddSnapshotsHostPathMount(&volumes, &mounts)

	cpu := rg.Resources.CPU
	if cpu < 1 {
		cpu = 1
	}
	mem := rg.Resources.Memory
	if mem < 128 {
		mem = 128
	}

	// SR-IOV: add /dev/vfio volume+mount if any interface is type=sriov.
	addSRIOVVolumesIfNeeded(&volumes, &mounts, guest)

	var initContainers []corev1.Container
	if rg.HasNetwork() {
		initContainers = append(initContainers, networkInitContainer())
	}

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
			NodeSelector: map[string]string{
				"kubeswift.io/kernel-node": "true",
			},
			Containers: []corev1.Container{
				{
					Name:            "launcher",
					Image:           LauncherImage(),
					ImagePullPolicy: corev1.PullIfNotPresent,
					SecurityContext: launcherSecurityContext(false),
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
	return pod
}

func buildDiskBootPod(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest, seedConfigMapName, intentConfigMapName string, rootDiskClone *RootDiskCloneResult) *corev1.Pod {
	volumes := []corev1.Volume{
		{
			Name: "run",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
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
	}
	if rg.HasSeed() {
		AddSeedVolume(&volumes, seedConfigMapName)
	}
	if rg.HasDataDisk() {
		volumes = append(volumes, corev1.Volume{
			Name: "data-disk",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: rg.GetDataDiskPVCName(),
				},
			},
		})
	}

	var mounts []corev1.VolumeMount
	var volumeDevices []corev1.VolumeDevice
	AddVolumeMounts(&mounts, &volumeDevices, rg, rg.HasSeed())
	if rg.HasDataDisk() {
		mounts = append(mounts, corev1.VolumeMount{Name: "data-disk", MountPath: DisksDataPath})
	}

	// /var/lib/kubeswift/snapshots/ — writable hostPath so the launcher's
	// CH process can write Tier B snapshot directories when the
	// SwiftSnapshot controller triggers a capture action.
	AddSnapshotsHostPathMount(&volumes, &mounts)

	// SR-IOV: add /dev/vfio volume+mount if any interface is type=sriov.
	addSRIOVVolumesIfNeeded(&volumes, &mounts, guest)

	cpu := rg.Resources.CPU
	if cpu < 1 {
		cpu = 1
	}
	mem := rg.Resources.Memory
	if mem < 128 {
		mem = 128
	}

	var initContainers []corev1.Container
	if rootDiskClone != nil && rootDiskClone.NeedsGrowInit {
		initContainers = append(initContainers, cloneGrowInitContainer(rg, rootDiskClone.TargetSizeBytes))
	}
	if rg.HasNetwork() {
		initContainers = append(initContainers, networkInitContainer())
	}

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
			Containers: []corev1.Container{
				{
					Name:            "launcher",
					Image:           LauncherImage(),
					ImagePullPolicy: corev1.PullIfNotPresent,
					SecurityContext: launcherSecurityContext(false),
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
	return pod
}

// VolumeMountPaths returns the canonical mount paths for the guest pod.
// Used when building pod spec to ensure alignment with swiftletd.
// intentPath is the directory; runtime-intent.json is the file inside it.
func VolumeMountPaths() (imagePath, seedPath, intentDir string) {
	return DisksRootPath, SeedPath, IntentPath
}

// rootDiskMount returns the launcher-pod root-disk mount surface for
// the resolved storage spec (W9 — runtime path for volumeMode: Block).
// Filesystem mode returns a non-nil VolumeMount; Block mode returns a
// non-nil VolumeDevice. Exactly one is non-nil. Callers append to the
// appropriate list.
//
// The two are mutually exclusive on the same volume name by Kubernetes
// contract — kubelet rejects with "volume X has volumeMode Block, but
// is specified in volumeMounts" (the W9 surface point that triggered
// this whole follow-up). Centralising the branch here means the four
// call sites (launcher container in pod.go and gpu.go;
// cloneGrowInitContainer; restore-receive launcher in restore.go)
// share one source of truth.
//
// The same helper serves the launcher container, clone-grow-init, and
// restore-receive launcher because all three use the same volume name
// ("root-disk") and the same path constants — the architect (W9 Q1
// review) confirmed a single helper covers the four call sites without
// per-site differentiation.
func rootDiskMount(rg *resolved.ResolvedGuest) (*corev1.VolumeMount, *corev1.VolumeDevice) {
	if rg.Storage.VolumeMode == "Block" {
		return nil, &corev1.VolumeDevice{Name: "root-disk", DevicePath: DiskRootDevicePath}
	}
	return &corev1.VolumeMount{Name: "root-disk", MountPath: DisksRootPath}, nil
}

// AddVolumeMounts adds the standard volume mounts (and, for Block-mode
// guests, the root-disk volume device) to a container.
//
// Caller must ensure volumes exist for image, seed (if present),
// intent, and dev-kvm. The W9 signature change adds the devices slice
// and rg parameter so the helper can place the root-disk surface on
// the correct list. Pre-W9 callers passed (mounts, hasSeed); W9
// callers pass (mounts, devices, rg, hasSeed).
func AddVolumeMounts(mounts *[]corev1.VolumeMount, devices *[]corev1.VolumeDevice, rg *resolved.ResolvedGuest, hasSeed bool) {
	_, seedPath, intentDir := VolumeMountPaths()
	*mounts = append(*mounts,
		corev1.VolumeMount{Name: "run", MountPath: RunDirPath},
	)
	if mount, device := rootDiskMount(rg); mount != nil {
		*mounts = append(*mounts, *mount)
	} else if device != nil && devices != nil {
		*devices = append(*devices, *device)
	}
	*mounts = append(*mounts,
		corev1.VolumeMount{Name: "runtime-intent", MountPath: intentDir},
		corev1.VolumeMount{Name: "dev-kvm", MountPath: "/dev/kvm"},
	)
	if hasSeed {
		*mounts = append(*mounts, corev1.VolumeMount{Name: "seed", MountPath: seedPath})
	}
}

// AddSeedVolume adds the seed ConfigMap volume to the pod. Use when ResolvedGuest has Seed.
// ConfigMap name should be guestName + SeedConfigMapSuffix.
func AddSeedVolume(volumes *[]corev1.Volume, configMapName string) {
	*volumes = append(*volumes, corev1.Volume{
		Name: "seed",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			},
		},
	})
}

// snapshotsHostPathVolumeName is the volume name for the writable
// SnapshotsHostPath mount on every launcher pod.
const snapshotsHostPathVolumeName = "kubeswift-snapshots"

// AddSnapshotsHostPathMount adds the writable hostPath volume + mount
// for /var/lib/kubeswift/snapshots/ to the launcher pod, so Cloud
// Hypervisor's vm.snapshot endpoint can write Tier B snapshot
// directories without pod recreation when SwiftSnapshot drives a
// capture action. DirectoryOrCreate handles first-snapshot bootstrap
// (the parent dir doesn't necessarily exist before the first capture).
//
// Idempotent: if the volume name already exists on volumes, neither
// the volume nor the mount is duplicated.
func AddSnapshotsHostPathMount(volumes *[]corev1.Volume, mounts *[]corev1.VolumeMount) {
	for _, v := range *volumes {
		if v.Name == snapshotsHostPathVolumeName {
			return
		}
	}
	*volumes = append(*volumes, corev1.Volume{
		Name: snapshotsHostPathVolumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: SnapshotsHostPath,
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		},
	})
	*mounts = append(*mounts, corev1.VolumeMount{
		Name:      snapshotsHostPathVolumeName,
		MountPath: SnapshotsHostPath,
	})
}

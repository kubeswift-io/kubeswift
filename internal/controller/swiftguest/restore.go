// Tier B (local-backend) restore pod construction.
//
// When the SwiftGuest carries the snapshot.kubeswift.io/active-restore
// annotation, the SwiftRestore controller has marked it as a target
// for a restore-receive launch (CH `--restore source_url=...`). The
// reconciler calls [BuildRestorePod] in place of the normal
// [BuildPod] so the launcher pod has the right shape:
//
//   - nodeSelector pinned to the on-node hostPath of the snapshot
//     directory (Tier B snapshots are inherently node-local).
//   - The snapshot directory is mounted hostPath, **read-only**, at
//     [RestoreSourcePath]. The on-disk snapshot is never modified.
//   - For an in-place restore (target name == source name with no
//     identity regeneration), runtime-intent.restore.snapshotPath
//     points directly at the read-only mount — no init container,
//     no staging copy, no patcher invocation. Fast path.
//   - For a clone (different target name, or any identity regen),
//     a writable pod-local emptyDir at [RestoreStagingPath] is
//     populated by the [SnapshotStagerInitContainerName] init
//     container. The init container copies snapshot files,
//     applies the requested config.json patches (cmdline marker
//     and per-clone MAC rewrites), and writes a sentinel last so
//     a partially-completed retry is wiped and redone. See
//     cmd/snapshot-stager/main.go.
//   - The seed ConfigMap is omitted entirely. The original VM's
//     cloud-init state is baked into the snapshot's memory; a
//     fresh seed.iso would not be consumed by the restored boot.
//   - The runtime-intent ConfigMap carries `restore: {snapshotPath: ...}`,
//     which routes swiftletd to [`run_ch_restore`] (rust/swiftletd
//     /src/launch.rs) instead of the normal CH/QEMU spawn path.
//
// The disk PVC continues to mount at the canonical
// /var/lib/kubeswift/disks/root/image.raw — CH on `--restore` reads
// that path from config.json verbatim, so the launcher pod must
// expose the disk file at the same in-pod path the source pod did.
// In-place restore reuses the source guest's PVC. Clone restore
// gets a fresh PVC populated from the source SwiftImage by the
// existing EnsureRootDiskClone Copy Job; this means clone restores
// boot with a snapshot's memory state on top of a SwiftImage-fresh
// disk. The cloud-init bootcmd in
// config/samples/seed-profiles/clone-identity-regen.yaml regenerates
// the in-guest identity (machine-id, SSH host keys, hostname); the
// disk freshness is documented in docs/snapshots/local-snapshots.md.

package swiftguest

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

// Annotation set used to drive Tier B restore pod construction.
//
// All three are written by the SwiftRestore controller onto the
// target SwiftGuest. The first names the active SwiftRestore so its
// lifecycle gates restore-mode pod construction (a SwiftRestore that
// has reached Ready leaves the annotation in place but the next pod
// recreation falls through to normal BuildPod — memory state is
// gone after a launcher pod restart anyway).
const (
	// AnnotationActiveRestore names the SwiftRestore driving this pod
	// (used by the controller to gate restore-mode construction on
	// the SwiftRestore's phase).
	AnnotationActiveRestore = "snapshot.kubeswift.io/active-restore"
	// AnnotationRestoreSnapshotPath is the on-node hostPath of the
	// snapshot directory. Mirrors snap.Spec.Backend.Local.HostPath.
	AnnotationRestoreSnapshotPath = "snapshot.kubeswift.io/restore-snapshot-path"
	// AnnotationRestoreNodeName is the node where the snapshot lives
	// (snap.Status.NodeName). Used as the launcher pod's nodeSelector.
	AnnotationRestoreNodeName = "snapshot.kubeswift.io/restore-node-name"
	// AnnotationRestoreMode is "in-place" (mount RO, no staging) or
	// "clone" (stage + patch). Empty defaults to in-place.
	AnnotationRestoreMode = "snapshot.kubeswift.io/restore-mode"
	// AnnotationRestoreMACRewrites is a comma-separated MAC list,
	// indexed by config.net[]. Only set for clone mode when MAC
	// regeneration is requested. Empty entries leave the source MAC
	// in place (the patcher's documented contract).
	AnnotationRestoreMACRewrites = "snapshot.kubeswift.io/restore-mac-rewrites"
	// AnnotationRestoreAppendCmdlineMarker, when "true", asks the
	// stager to append `kubeswift.clone=true` to config.payload.cmdline
	// so the in-guest cloud-init bootcmd regenerates machine-id, SSH
	// host keys, and hostname on first wake (see
	// docs/snapshots/identity-regeneration.md).
	AnnotationRestoreAppendCmdlineMarker = "snapshot.kubeswift.io/restore-append-cmdline-marker"
	// AnnotationRestoreRuntimeDirFromPrefix is the source pod's
	// runtime_dir prefix that the stager must rewrite in
	// disks[].path and serial.socket. Must end in "/". Empty disables
	// the rewrite (in-place restores reuse the source's runtime_dir
	// name and don't need the patch).
	AnnotationRestoreRuntimeDirFromPrefix = "snapshot.kubeswift.io/restore-runtime-dir-from"
	// AnnotationRestoreRuntimeDirToPrefix is the clone pod's runtime_dir
	// prefix that source-runtime_dir paths must be rewritten to. Must
	// end in "/".
	AnnotationRestoreRuntimeDirToPrefix = "snapshot.kubeswift.io/restore-runtime-dir-to"
	// AnnotationRestoreNullifyHostMAC, when "true", asks the stager to
	// set net[].host_mac to null in config.json. Required for clones
	// because CH on restore would otherwise force the clone tap's MAC
	// to the source's saved value (cloud-hypervisor v51.1
	// net_util/src/open_tap.rs sets the tap MAC when host_mac is Some).
	AnnotationRestoreNullifyHostMAC = "snapshot.kubeswift.io/restore-nullify-host-mac"
)

// Restore mode values for AnnotationRestoreMode.
const (
	RestoreModeInPlace = "in-place"
	RestoreModeClone   = "clone"
)

// In-pod paths for restore mounts. Kept here (not in constants.go)
// because they're specific to the Tier B path and have no other
// callers.
const (
	// RestoreSourcePath is where the read-only on-node snapshot dir
	// is mounted. swiftletd reads this directly when the restore mode
	// is in-place (no staging required).
	RestoreSourcePath = "/var/lib/kubeswift/restore/source"
	// RestoreStagingPath is where the snapshot-stager writes the
	// patched copy for clone restores. swiftletd reads this when the
	// restore mode is clone.
	RestoreStagingPath = "/var/lib/kubeswift/restore/staging"

	snapshotSourceVolume   = "snapshot-source"
	snapshotStagingVolume  = "snapshot-staging"
	snapshotStagerImageEnv = "" // reuses launcher image — keeps stager binary in lockstep with swiftletd

	// SnapshotStagerInitContainerName is the init container that
	// stages a clone restore. Tests assert on this name.
	SnapshotStagerInitContainerName = "snapshot-stager"
)

// RestoreParams collects the inputs for [BuildRestorePod] in one
// place so the call site reads naturally. Populated by the SwiftGuest
// reconciler from the AnnotationRestore* annotations.
type RestoreParams struct {
	// SnapshotPath is the on-node absolute path of the snapshot
	// directory (e.g. /var/lib/kubeswift/snapshots/default-snap1).
	SnapshotPath string
	// NodeName is the Kubernetes node where the snapshot lives.
	NodeName string
	// Mode is RestoreModeInPlace or RestoreModeClone.
	Mode string
	// MACRewrites is a comma-separated MAC list (clone mode only).
	// Same format as the snapshot-stager's --rewrite-macs flag.
	MACRewrites string
	// AppendCmdlineMarker asks the stager to mark the kernel cmdline
	// for the in-guest identity-regeneration bootcmd.
	AppendCmdlineMarker bool
	// RuntimeDirFromPrefix is the source pod's runtime_dir prefix
	// (must end in '/') that the stager substitutes in disks[].path
	// and serial.socket. Empty disables the rewrite.
	RuntimeDirFromPrefix string
	// RuntimeDirToPrefix is the clone pod's runtime_dir prefix
	// (must end in '/') that source paths are rewritten to.
	RuntimeDirToPrefix string
	// NullifyHostMAC asks the stager to set net[].host_mac to null in
	// config.json. Required for clones (see configjson.PatchOptions).
	NullifyHostMAC bool
}

// IsClone reports whether the restore must stage and patch the
// snapshot. Anything other than the explicit in-place mode is
// treated as a clone (defensive — an unknown future value is the
// safe-but-slower path).
func (p RestoreParams) IsClone() bool {
	return p.Mode != RestoreModeInPlace
}

// InPodSnapshotPath returns the path swiftletd should pass to CH on
// `--restore source_url=file://<path>/`. For in-place that's the
// read-only mount; for clone that's the writable staged copy.
func (p RestoreParams) InPodSnapshotPath() string {
	if p.IsClone() {
		return RestoreStagingPath
	}
	return RestoreSourcePath
}

// BuildRestorePod constructs the launcher pod for a Tier B restore.
//
// rootDiskClone may be nil — in-place restores reuse the source
// guest's PVC and don't go through EnsureRootDiskClone (it's a no-op
// when the per-guest PVC already exists, but Phase 2 commit 10b
// callers don't need to do it). For clones, rootDiskClone names the
// freshly-cloned per-guest PVC that EnsureRootDiskClone produced.
//
// seedConfigMapName names the seed ConfigMap to mount when the source
// guest had a seed profile. The snapshot's config.json references the
// original seed.iso disk path; CH refuses to restore when that file is
// missing, so the launcher rebuilds seed.iso (deterministically — the
// bytes are the same as the original since the seed dir contents are
// the same). For in-place restore the runtime_dir name matches the
// source so the rebuilt path matches what config.json expects. For
// clones the runtime_dir differs; clone restore relies on the
// snapshot-stager + a future config.json disk-path patch (not in
// commit 10b). Empty seedConfigMapName skips the seed mount.
func BuildRestorePod(
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
	seedConfigMapName, intentConfigMapName string,
	rootDiskClone *RootDiskCloneResult,
	params RestoreParams,
) *corev1.Pod {
	pvcName := ""
	if rootDiskClone != nil {
		pvcName = rootDiskClone.PVCName
	} else {
		pvcName = RootDiskPVCPrefix + guest.Name
	}

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
					ClaimName: pvcName,
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
		// Snapshot source mount — always present, always read-only.
		// The snapshot dir is the on-node hostPath that the SwiftSnapshot
		// controller (Tier B local) wrote.
		{
			Name: snapshotSourceVolume,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: params.SnapshotPath,
					Type: ptr.To(corev1.HostPathType("Directory")),
				},
			},
		},
	}

	if params.IsClone() {
		// Pod-local writable staging copy. emptyDir survives init
		// container restarts, which is intentional — the stager's
		// sentinel file makes the second run idempotent.
		volumes = append(volumes, corev1.Volume{
			Name: snapshotStagingVolume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}
	// Seed ConfigMap mount — required for restore-receive even though
	// cloud-init has already run (the seed dir is baked into the
	// snapshot's memory state). The snapshot's config.json still
	// references the seed.iso disk path; CH on --restore re-opens
	// every disk listed and refuses with "no such file or directory"
	// when the file is missing. swiftletd reconstructs seed.iso
	// deterministically from the seed ConfigMap before spawn_ch_restore.
	if rg.HasSeed() && seedConfigMapName != "" {
		AddSeedVolume(&volumes, seedConfigMapName)
	}

	// W9 — root-disk surface depends on resolved storage volumeMode. The
	// restore-receive launcher pod has the same kubelet contract as the
	// regular launcher: VolumeMounts and VolumeDevices cannot share a
	// volume name. Channel through rootDiskMount so the same source-of-
	// truth covers all five call sites.
	mounts := []corev1.VolumeMount{
		{Name: "run", MountPath: RunDirPath},
	}
	var volumeDevices []corev1.VolumeDevice
	if mount, device := rootDiskMount(rg); mount != nil {
		mounts = append(mounts, *mount)
	} else if device != nil {
		volumeDevices = append(volumeDevices, *device)
	}
	mounts = append(mounts,
		corev1.VolumeMount{Name: "runtime-intent", MountPath: IntentPath},
		corev1.VolumeMount{Name: "dev-kvm", MountPath: "/dev/kvm"},
	)
	if rg.HasSeed() && seedConfigMapName != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: "seed", MountPath: SeedPath})
	}
	if params.IsClone() {
		// In clone mode the launcher reads the staged+patched copy.
		// The RO source mount stays only on the init container —
		// the launcher doesn't need it. Strict-by-default: we don't
		// expose the source dir to the launcher process.
		mounts = append(mounts, corev1.VolumeMount{
			Name: snapshotStagingVolume, MountPath: RestoreStagingPath,
		})
	} else {
		// In-place: launcher reads the read-only source directly.
		mounts = append(mounts, corev1.VolumeMount{
			Name: snapshotSourceVolume, MountPath: RestoreSourcePath, ReadOnly: true,
		})
	}

	cpu := rg.Resources.CPU
	if cpu < 1 {
		cpu = 1
	}
	mem := rg.Resources.Memory
	if mem < 128 {
		mem = 128
	}

	var initContainers []corev1.Container
	if rg.HasNetwork() {
		initContainers = append(initContainers, networkInitContainer())
	}
	if params.IsClone() {
		initContainers = append(initContainers, snapshotStagerInitContainer(params))
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        guest.Name,
			Namespace:   guest.Namespace,
			Annotations: podAnnotations(guest),
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guest.Name,
				"swift.kubeswift.io/role":  "restore-receive",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			NodeSelector:   map[string]string{"kubernetes.io/hostname": params.NodeName},
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

// snapshotStagerInitContainer returns the init container that copies
// the snapshot source into the staging emptyDir, applies config.json
// patches, and writes a sentinel last. Sentinel-guarded for restart
// safety — see cmd/snapshot-stager/main.go.
func snapshotStagerInitContainer(params RestoreParams) corev1.Container {
	args := []string{
		"--src", RestoreSourcePath,
		"--dst", RestoreStagingPath,
	}
	if params.AppendCmdlineMarker {
		args = append(args, "--append-cmdline-marker=true")
	}
	if strings.TrimSpace(params.MACRewrites) != "" {
		args = append(args, "--rewrite-macs", params.MACRewrites)
	}
	if params.RuntimeDirFromPrefix != "" && params.RuntimeDirToPrefix != "" {
		args = append(args,
			"--rewrite-runtime-dir-from", params.RuntimeDirFromPrefix,
			"--rewrite-runtime-dir-to", params.RuntimeDirToPrefix,
		)
	}
	if params.NullifyHostMAC {
		args = append(args, "--nullify-host-mac=true")
	}
	return corev1.Container{
		Name:            SnapshotStagerInitContainerName,
		Image:           LauncherImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/usr/local/bin/snapshot-stager"},
		Args:            args,
		// No privileged context — the stager only reads from a RO
		// hostPath mount and writes to a pod-local emptyDir; no need
		// for SYS_ADMIN, NET_ADMIN, etc.
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: snapshotSourceVolume, MountPath: RestoreSourcePath, ReadOnly: true},
			{Name: snapshotStagingVolume, MountPath: RestoreStagingPath},
		},
	}
}

// RestoreParamsFromAnnotations extracts a RestoreParams from a
// SwiftGuest's annotations. Returns the params and `present` true
// when AnnotationActiveRestore is set; the caller decides whether
// the SwiftRestore is in a non-terminal phase before treating the
// guest as restore-mode.
func RestoreParamsFromAnnotations(annotations map[string]string) (RestoreParams, bool) {
	if annotations == nil {
		return RestoreParams{}, false
	}
	if _, ok := annotations[AnnotationActiveRestore]; !ok {
		return RestoreParams{}, false
	}
	mode := annotations[AnnotationRestoreMode]
	if mode == "" {
		mode = RestoreModeInPlace
	}
	return RestoreParams{
		SnapshotPath:         annotations[AnnotationRestoreSnapshotPath],
		NodeName:             annotations[AnnotationRestoreNodeName],
		Mode:                 mode,
		MACRewrites:          annotations[AnnotationRestoreMACRewrites],
		AppendCmdlineMarker:  annotations[AnnotationRestoreAppendCmdlineMarker] == "true",
		RuntimeDirFromPrefix: annotations[AnnotationRestoreRuntimeDirFromPrefix],
		RuntimeDirToPrefix:   annotations[AnnotationRestoreRuntimeDirToPrefix],
		NullifyHostMAC:       annotations[AnnotationRestoreNullifyHostMAC] == "true",
	}, true
}

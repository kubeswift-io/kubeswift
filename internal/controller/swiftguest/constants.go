package swiftguest

import "os"

// LauncherImageEnv is the env var to override the launcher image. When set, it overrides LauncherImageDefault.
const LauncherImageEnv = "KUBESWIFT_LAUNCHER_IMAGE"

// LauncherImageDefault is the default container image for the guest pod (swiftletd + Cloud Hypervisor).
const LauncherImageDefault = "ghcr.io/projectbeskar/kubeswift/swiftletd:latest"

// LauncherImage returns the launcher image, from KUBESWIFT_LAUNCHER_IMAGE env or LauncherImageDefault.
func LauncherImage() string {
	if img := os.Getenv(LauncherImageEnv); img != "" {
		return img
	}
	return LauncherImageDefault
}

// PodAnnotationGuestIP is the annotation key for the guest's primary IP (set by swiftletd when discovered).
const PodAnnotationGuestIP = "kubeswift.io/guest-ip"

// PodAnnotationGuestInterfaces is the annotation key for guest network interfaces JSON (set by swiftletd lease poller).
const PodAnnotationGuestInterfaces = "kubeswift.io/guest-interfaces"

// PodAnnotationGuestRuntimePID is the annotation key for the CH process PID (set by swiftletd on socket ready).
const PodAnnotationGuestRuntimePID = "kubeswift.io/guest-runtime-pid"

// PodAnnotationGuestSerialSocket is the annotation key for the serial socket path (set by swiftletd on socket ready).
const PodAnnotationGuestSerialSocket = "kubeswift.io/guest-serial-socket"

// PodAnnotationGuestHypervisor is the annotation key for the hypervisor type (set by swiftletd on socket ready).
const PodAnnotationGuestHypervisor = "kubeswift.io/guest-hypervisor"

// PodAnnotationEgress is the annotation key ("true"/"false") for VM->cluster-DNS
// ClusterIP reachability, set by the launcher entrypoint's egress probe (service
// exposure §4). Mapped to status.network.egress + the EgressReady condition.
const PodAnnotationEgress = "kubeswift.io/egress-cluster-reachable"

// PodAnnotationPrimaryUDNIface marks a launcher pod whose guest rides the namespace
// primary OVN-Kubernetes UDN (Model A); the value is the in-pod UDN interface name
// (ovn-udn1). The pod-builder stamps it. swiftletd cannot reach the apiserver from a
// primary-UDN pod (the UDN is bridged to the guest and eth0 is infrastructure-locked),
// so the controller derives status for these guests from the OVN annotation + launcher
// readiness instead of swiftletd's annotations. See docs/design/udn-primary-integration.md.
const PodAnnotationPrimaryUDNIface = "kubeswift.io/primary-udn-interface"

// OVNPodNetworksAnnotation is OVN-Kubernetes' status annotation carrying each attached
// network's IP/MAC/routes. For a Model A pod the guest's IP is the entry that is NOT
// the cluster default network ("default", role infrastructure-locked).
const OVNPodNetworksAnnotation = "k8s.ovn.org/pod-networks"

// Mount path constants. Must match internal/runtimeintent and rust/swiftletd.
const (
	DisksRootPath = "/var/lib/kubeswift/disks/root"
	DisksDataPath = "/var/lib/kubeswift/disks/data"
	SeedPath      = "/var/lib/kubeswift/seed"
	IntentPath    = "/var/lib/kubeswift/intent"
	IntentFile    = "runtime-intent.json"
	RunDirPath    = "/var/lib/kubeswift/run"

	// DiskRootDevicePath is the in-pod device path for a Block-mode
	// root disk (W9 — runtime path for spec.storage.volumeMode: Block).
	// Cloud Hypervisor's --disk path=<value> opens this path opaquely;
	// for Block-mode PVCs, the path resolves to a raw block device
	// surfaced via VolumeDevices, not a filesystem-mounted file.
	//
	// Distinct from DisksRootPath (the Filesystem mount): the two are
	// mutually exclusive on the same volume by Kubernetes contract
	// (VolumeMounts and VolumeDevices cannot share a volume name —
	// kubelet rejects with "volume X has volumeMode Block, but is
	// specified in volumeMounts", which was the W9 surface point).
	//
	// Brand prefix is deliberate: /dev/kubeswift-root is unambiguous
	// in pod logs, swiftletd diagnostics, and CH --disk arg dumps. We
	// avoid /dev/vda* / /dev/sd* (kernel-managed virtio/SCSI) and
	// /dev/disk/by-* (udev-reserved-feeling) per architect Q3 review.
	DiskRootDevicePath = "/dev/kubeswift-root"

	// SnapshotsHostPath is the on-node directory Cloud Hypervisor writes
	// Tier B snapshot directories into (config.json, state.json, memory-
	// ranges) when the SwiftSnapshot controller drives a vm.snapshot
	// action. It is mounted on every launcher pod (writable hostPath)
	// so the source-guest's CH process can write to it without pod
	// recreation. The validation webhook constrains operator-provided
	// hostPaths to live under this prefix.
	//
	// The path also doubles as the read-side mount for restore-receive
	// pods, but those mount the specific snapshot subdirectory (not
	// this parent) — see internal/controller/swiftguest/restore.go.
	SnapshotsHostPath = "/var/lib/kubeswift/snapshots"
)

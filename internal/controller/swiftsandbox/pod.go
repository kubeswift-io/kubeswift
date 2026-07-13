package swiftsandbox

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

const (
	intentConfigMapSuffix = "-runtime-intent"
	kernelNodeLabel       = "kubeswift.io/kernel-node"
	defaultKernelProfile  = "sandbox"
	materializeInitName   = "sandbox-materialize"
	launcherName          = "launcher"
	// SandboxLabelKey ties the launcher pod (and its intent ConfigMap) to the
	// owning SwiftSandbox.
	SandboxLabelKey = "sandbox.kubeswift.io/sandbox"
)

func intentConfigMapName(sb *sandboxv1alpha1.SwiftSandbox) string {
	return sb.Name + intentConfigMapSuffix
}

func privileged() *corev1.SecurityContext {
	return &corev1.SecurityContext{Privileged: ptr.To(true)}
}

// networked reports whether the sandbox gets a network (every mode except "none").
func networked(sb *sandboxv1alpha1.SwiftSandbox) bool {
	return sb.Spec.Network.Mode != sandboxv1alpha1.SandboxNetworkNone
}

// egressMode is the effective egress posture for a networked sandbox. The CRD
// default is restricted; the fallthrough also covers a pre-admission empty value.
func egressMode(sb *sandboxv1alpha1.SwiftSandbox) sandboxv1alpha1.SandboxNetworkMode {
	if sb.Spec.Network.Mode == sandboxv1alpha1.SandboxNetworkOpen {
		return sandboxv1alpha1.SandboxNetworkOpen
	}
	return sandboxv1alpha1.SandboxNetworkRestricted
}

// The bridge-initramfs reads the workload exec spec from the config disk. swiftletd
// emits it right after the (optional) block rootfs, and a sandbox carries no
// seed/data disks, so it enumerates as:
//   - /dev/vdb for block rootfs (rootfs is /dev/vda, config is next).
//   - /dev/vda for virtio-fs rootfs (no block rootfs disk — the virtio-fs share
//     is not a virtio-blk device, so the config disk is the first /dev/vd*).
const (
	sandboxConfigDeviceBlock    = "/dev/vdb"
	sandboxConfigDeviceVirtiofs = "/dev/vda"
)

// buildIntent constructs the mode-3 sandbox RuntimeIntent: kernel boot + the RO
// OCI rootfs disk + (when the workload has a defined exec) the config disk + the
// bridge cmdline. idle marks a warm-pool keeper slot: it carries no workload (a
// checkout injects one later over vsock) and boots straight to the bridge's idle
// loop via kubeswift.idle=1 — so warming never depends on the image having a sleep
// binary, and distroless images can be pooled.
func buildIntent(sb *sandboxv1alpha1.SwiftSandbox, kernelName, rootfsPath string, exec execSpec, idle bool) *runtimeintent.RuntimeIntent {
	kernelDir := kernelv1alpha1.KernelLocalPath(sb.Namespace, kernelName)
	virtiofs := sb.Spec.RootfsMode == sandboxv1alpha1.SandboxRootfsVirtiofs
	rootfsKind := "block"
	if virtiofs {
		rootfsKind = "virtiofs"
	}
	cmdline := "console=ttyS0 kubeswift.rootfs=" + rootfsKind
	if idle {
		cmdline += " kubeswift.idle=1"
	}
	if networked(sb) {
		// Kernel IP autoconfig (the sandbox kernel has CONFIG_IP_PNP_DHCP=y). A bare
		// OCI workload (e.g. /bin/sh) runs no DHCP client and the bridge-init does no
		// network setup, so the guest would otherwise never acquire the dnsmasq lease
		// / an IP (and status.network.primaryIP would stay empty). ip=dhcp makes the
		// kernel DHCP eth0 at boot. Omitted for network:none — there is no dnsmasq,
		// so kernel DHCP would only stall the boot.
		cmdline += " ip=dhcp"
		// The kernel ip=dhcp path captures the DHCP nameserver but NOT the search list
		// (DHCP option 119), so cluster-internal SHORT names (e.g. kubernetes.default)
		// would NXDOMAIN. Pass the standard k8s search domains for the sandbox's
		// namespace; the bridge-init writes them into the guest's /etc/resolv.conf.
		// (Cluster domain assumed cluster.local — the near-universal default.)
		cmdline += " kubeswift.dns-search=" + sb.Namespace + ".svc.cluster.local,svc.cluster.local,cluster.local"
	}
	var sandboxExec *runtimeintent.SandboxExecSpec
	if !idle && exec.nonTrivial() {
		// The workload argv/env/cwd ride the config disk (kubeswift.config points the
		// bridge at it) — never the cmdline, so env stays out of /proc/cmdline + logs.
		// A keeper carries no workload (idle), so it gets no config disk.
		configDev := sandboxConfigDeviceBlock
		if virtiofs {
			configDev = sandboxConfigDeviceVirtiofs
		}
		cmdline += " kubeswift.config=" + configDev
		sandboxExec = &runtimeintent.SandboxExecSpec{Argv: exec.Argv, Env: exec.Env, Cwd: exec.Cwd}
	}
	cpu := int(sb.Spec.CPU)
	if cpu < 1 {
		cpu = 1
	}
	memMiB := int(sb.Spec.Memory.Value() >> 20)
	if memMiB < 128 {
		memMiB = 128
	}
	// Rootfs delivery. block: the ext4 rides SandboxRootfs.Path as /dev/vda.
	// virtiofs: the unpacked tree at rootfsPath is shared over virtio-fs (tag
	// "sandboxroot"); SandboxRootfs stays present (the sandbox marker) but carries
	// no block path. Either way the bridge overlays a tmpfs upper.
	sandboxRootfs := &runtimeintent.SandboxRootfsSpec{Virtiofs: virtiofs}
	var filesystems []runtimeintent.FilesystemIntent
	if virtiofs {
		filesystems = []runtimeintent.FilesystemIntent{{
			Name:       "sandboxroot",
			Tag:        "sandboxroot",
			SourcePath: rootfsPath,
			ReadOnly:   true,
		}}
	} else {
		sandboxRootfs.Path = rootfsPath
	}

	return &runtimeintent.RuntimeIntent{
		KernelBoot: &runtimeintent.KernelBootSpec{
			KernelPath:    kernelDir + "/bzImage",
			InitramfsPath: kernelDir + "/rootfs.cpio.gz",
			Cmdline:       cmdline,
		},
		SandboxRootfs: sandboxRootfs,
		Filesystems:   filesystems,
		SandboxExec:   sandboxExec,
		CPU:           cpu,
		Memory:        memMiB,
		Lifecycle:     "start",
		GuestID:       sb.Namespace + "/" + sb.Name,
		// Networked unless mode=none: network-init sets up br0/tap0 and the
		// launcher-entrypoint starts dnsmasq; a deny-ingress NetworkPolicy enforces
		// the "restricted" posture (built by the controller).
		Network:    networked(sb),
		Hypervisor: "cloud-hypervisor",
		// vsock control channel for the in-guest agent (swiftctl sandbox exec/attach).
		// The agent is baked into the sandbox initramfs and the bridge runs it; the host
		// reaches it through CH's vsock unix socket. Always wired for sandboxes — vsock
		// is host<->guest only (not network-reachable), so it costs nothing to have.
		Vsock: &runtimeintent.VsockIntent{CID: runtimeintent.DeriveVsockCID(sb.Namespace, sb.Name)},
	}
}

// buildIntentConfigMap wraps the serialized intent in the ConfigMap the launcher
// mounts (swiftletd reads swiftguest.IntentPath/IntentFile).
func buildIntentConfigMap(sb *sandboxv1alpha1.SwiftSandbox, intentJSON []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: intentConfigMapName(sb), Namespace: sb.Namespace},
		Data:       map[string]string{swiftguest.IntentFile: string(intentJSON)},
	}
}

// buildPod builds the sandbox launcher pod: a sandbox-materialize init container
// (pulls the image + produces the RO ext4 in the node cache) followed by the
// swiftletd launcher (mode-3 direct-kernel boot of that rootfs). RestartPolicy
// Never; pinned to a kernel node.
func buildPod(sb *sandboxv1alpha1.SwiftSandbox, kernelName string) *corev1.Pod {
	kernelDir := kernelv1alpha1.KernelLocalPath(sb.Namespace, kernelName)

	nodeSelector := map[string]string{kernelNodeLabel: "true"}
	for k, v := range sb.Spec.NodeSelector {
		nodeSelector[k] = v
	}

	matArgs := []string{
		"--image", sb.Spec.Image,
		"--cache-dir", rootfsCacheDir,
		"--mode", sandboxRootfsMode(sb),
		"--result-file", "/dev/termination-log",
	}
	// (--pull-secret mount when spec.imagePullSecret is set — follow-up.)

	matMounts := []corev1.VolumeMount{{Name: "rootfs-cache", MountPath: rootfsCacheDir}}
	var matEnv []corev1.EnvVar
	var verifyVolumes []corev1.Volume
	if ref := sb.Spec.VerifyKeySecretRef; ref != nil && ref.Name != "" {
		// cosign-verify image@digest BEFORE materializing. The public key is mounted
		// read-only at /verify-key/cosign.pub; cosign needs a writable HOME/TMPDIR,
		// served by an emptyDir. A bad/missing signature fails this init container, so
		// the sandbox goes Failed and never boots. Mirrors the SwiftImage import path.
		matArgs = append(matArgs, "--verify-key=/verify-key/cosign.pub")
		matEnv = append(matEnv,
			corev1.EnvVar{Name: "HOME", Value: "/cosign-home"},
			corev1.EnvVar{Name: "TMPDIR", Value: "/cosign-home"},
		)
		matMounts = append(matMounts,
			corev1.VolumeMount{Name: "verify-key", MountPath: "/verify-key", ReadOnly: true},
			corev1.VolumeMount{Name: "cosign-home", MountPath: "/cosign-home"},
		)
		verifyVolumes = append(verifyVolumes,
			corev1.Volume{Name: "verify-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: ref.Name,
				Items:      []corev1.KeyToPath{{Key: "cosign.pub", Path: "cosign.pub"}},
			}}},
			corev1.Volume{Name: "cosign-home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		)
	}

	initContainers := []corev1.Container{{
		Name:            materializeInitName,
		Image:           SandboxMaterializeImage(),
		Args:            matArgs,
		Env:             matEnv,
		SecurityContext: privileged(),
		VolumeMounts:    matMounts,
	}}
	if networked(sb) {
		// network-init (br0/tap0/dnsmasq) runs first; it mounts the same
		// runtime-intent + run volumes the launcher uses. KUBESWIFT_SANDBOX_EGRESS
		// tells it whether to install the restricted-egress FORWARD iptables. This is
		// the ONLY layer that can filter the VM's egress precisely: a pod-level
		// NetworkPolicy can't, because after MASQUERADE the VM's traffic and
		// swiftletd's own API-server traffic share the pod IP — a NetworkPolicy that
		// blocked cluster egress would also cut swiftletd's status reporting (#347).
		// The FORWARD chain matches the VM's pre-NAT source (bridge subnet) only.
		ni := swiftguest.NetworkInitContainer()
		ni.Env = append(ni.Env, corev1.EnvVar{Name: "KUBESWIFT_SANDBOX_EGRESS", Value: string(egressMode(sb))})
		initContainers = append([]corev1.Container{ni}, initContainers...)
	}

	dirCreate := corev1.HostPathDirectoryOrCreate
	charDev := corev1.HostPathCharDev

	volumes := []corev1.Volume{
		{Name: "kernel-artifacts", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: kernelDir, Type: &dirCreate}}},
		{Name: "rootfs-cache", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: rootfsCacheDir, Type: &dirCreate}}},
		{Name: "runtime-intent", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: intentConfigMapName(sb)}}}},
		{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "dev-kvm", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/kvm", Type: &charDev}}},
	}
	volumes = append(volumes, verifyVolumes...)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.Name,
			Namespace: sb.Namespace,
			Labels:    map[string]string{SandboxLabelKey: sb.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			NodeSelector:   nodeSelector,
			InitContainers: initContainers,
			Containers: []corev1.Container{{
				Name:            launcherName,
				Image:           swiftguest.LauncherImage(),
				SecurityContext: privileged(),
				// swiftletd reads POD_NAME/POD_NAMESPACE (downward API) to know which
				// pod to report onto; without them it skips the report + lease paths
				// entirely (guest pid/hypervisor/IP never surface). KUBESWIFT_REPORT_GUEST_CR
				// tells swiftletd NOT to patch a SwiftGuest CR status (there is none for a
				// sandbox — the SwiftSandbox controller owns status from the pod annotations).
				Env: []corev1.EnvVar{
					{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
					{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
					{Name: "KUBESWIFT_REPORT_GUEST_CR", Value: "false"},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "kernel-artifacts", MountPath: kernelDir},
					{Name: "rootfs-cache", MountPath: rootfsCacheDir},
					{Name: "runtime-intent", MountPath: swiftguest.IntentPath},
					{Name: "run", MountPath: swiftguest.RunDirPath},
					{Name: "dev-kvm", MountPath: "/dev/kvm"},
				},
			}},
			Volumes: volumes,
		},
	}
}

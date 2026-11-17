package swiftsandbox

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// A sandbox's SPIFFE identity. The chart's two ClusterSPIFFEIDs register the
// launcher pods carrying SPIFFELabelKey; SPIRE (or another SPIFFE
// implementation behind the same Workload API) issues the SVID, and the
// egress proxy fetches it from the Workload API socket mounted at
// spiffeMountDir. Neither swiftletd nor the guest sees the socket.
//
//   - A warm slot is registered as .../ns/<ns>/pool/<pool>/slot/<slot>, from
//     PoolLabelKey and SlotNameLabelKey, which a checkout does not change: the
//     SVID exists before a sandbox claims the slot, so a checkout waits for
//     nothing. (Templating the ID from the sandbox's name made every checkout
//     wait for the SPIRE agent's next sync, 1.5 to 7 s.)
//   - A cold launcher is registered as .../ns/<ns>[/pool/<poolRef>]/sandbox/<name>,
//     from SandboxLabelKey and PoolRefLabelKey.
//
// The ID is as strong as pod creation in the namespace: whoever may create a
// pod there can label it like a launcher. That is the usual SPIFFE-on-
// Kubernetes trust boundary, and launcher namespaces already treat pod
// creation as node-level trust.
const (
	// SPIFFELabelKey marks a launcher pod whose sandbox (or pool) has
	// spec.identity.spiffe Enabled.
	SPIFFELabelKey = "sandbox.kubeswift.io/spiffe"
	// PoolRefLabelKey is a cold launcher's spec.poolRef. Not PoolLabelKey:
	// that one marks slot membership.
	PoolRefLabelKey = "sandbox.kubeswift.io/pool-ref"
	spiffeVolume    = "spiffe-workload-api"
	spiffeMountDir  = "/run/spiffe"
)

func identityEnabled(id *sandboxv1alpha1.SandboxIdentity) bool {
	return id != nil && id.SPIFFE == sandboxv1alpha1.SandboxIdentitySPIFFEEnabled
}

// coldSPIFFEID is the ID registered for a cold launcher of sb.
func coldSPIFFEID(sb *sandboxv1alpha1.SwiftSandbox) string {
	id := "spiffe://" + sandboxwebhook.SPIFFE.TrustDomain + "/ns/" + sb.Namespace
	if sb.Spec.PoolRef != nil {
		id += "/pool/" + sb.Spec.PoolRef.Name
	}
	return id + "/sandbox/" + sb.Name
}

// slotSPIFFEID is the ID registered for warm slot `slot` of pool `pool`.
func slotSPIFFEID(namespace, pool, slot string) string {
	return "spiffe://" + sandboxwebhook.SPIFFE.TrustDomain + "/ns/" + namespace + "/pool/" + pool + "/slot/" + slot
}

// launcherSPIFFEID is the ID of the launcher pod running sb, or "" when it
// has none: the slot's ID for a pod of a pool, the sandbox's otherwise.
func launcherSPIFFEID(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) string {
	if !identityEnabled(sb.Spec.Identity) || sandboxwebhook.SPIFFE == nil || pod == nil || pod.Labels[SPIFFELabelKey] != "enabled" {
		return ""
	}
	if pool, slot := pod.Labels[PoolLabelKey], pod.Labels[SlotNameLabelKey]; pool != "" && slot != "" {
		return slotSPIFFEID(pod.Namespace, pool, slot)
	}
	return coldSPIFFEID(sb)
}

// setIdentityStatus records the launcher's registered ID.
func setIdentityStatus(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) {
	if id := launcherSPIFFEID(sb, pod); id != "" {
		sb.Status.Identity = &sandboxv1alpha1.SandboxIdentityStatus{SPIFFEID: id}
	}
}

// workloadAPISocket is the Workload API address inside the proxy container.
func workloadAPISocket() string {
	return "unix://" + spiffeMountDir + "/" + sandboxwebhook.SPIFFE.SocketName
}

// addSPIFFE labels a launcher pod for registration and gives its egress proxy
// the Workload API socket. Runs after addEgressProxy. A warm slot's pool
// labels are added by the pool controller; a cold launcher of a pooled
// sandbox gets PoolRefLabelKey here.
func addSPIFFE(pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) {
	cfg := sandboxwebhook.SPIFFE
	if cfg == nil || !identityEnabled(sb.Spec.Identity) || !hasEgressProxy(pod) {
		return
	}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[SPIFFELabelKey] = "enabled"
	if sb.Spec.PoolRef != nil {
		pod.Labels[PoolRefLabelKey] = sb.Spec.PoolRef.Name
	}
	vol := corev1.Volume{Name: spiffeVolume}
	if cfg.CSIDriver != "" {
		vol.VolumeSource = corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: cfg.CSIDriver, ReadOnly: ptr.To(true)}}
	} else {
		vol.VolumeSource = corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: cfg.HostPath, Type: ptr.To(corev1.HostPathDirectory)}}
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, vol)
	for i := range pod.Spec.InitContainers {
		if c := &pod.Spec.InitContainers[i]; c.Name == EgressProxyContainerName {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: spiffeVolume, MountPath: spiffeMountDir, ReadOnly: true})
			c.Env = append(c.Env, corev1.EnvVar{Name: "SPIFFE_ENDPOINT_SOCKET", Value: workloadAPISocket()})
		}
	}
}

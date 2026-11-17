package swiftsandbox

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// A sandbox's SPIFFE identity. The chart's two ClusterSPIFFEIDs register the
// launcher pods carrying SPIFFELabelKey; SPIRE (or another SPIFFE
// implementation behind the same Workload API) issues the SVID, fetched from
// the Workload API socket mounted at spiffeMountDir by whatever holds the
// sandbox's egress: the egress proxy container, or without one the launcher
// container, whose built-in broker originates TLS with it for
// allow[].tls rules. The guest never sees the socket.
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

// addSPIFFE labels a launcher pod for registration and gives the egress
// proxy container, or without one the launcher container (the built-in
// broker), the Workload API socket; the broker also gets its TLS rules. Runs
// after addEgressProxy and addProxiedLauncherEnv. A warm slot's pool labels
// are added by the pool controller; a cold launcher of a pooled sandbox gets
// PoolRefLabelKey here.
func addSPIFFE(pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) {
	cfg := sandboxwebhook.SPIFFE
	if cfg == nil || !identityEnabled(sb.Spec.Identity) || sb.Spec.Network.Mode != sandboxv1alpha1.SandboxNetworkProxied {
		return
	}
	holder, holderInit := launcherName, false
	if hasEgressProxy(pod) {
		holder, holderInit = EgressProxyContainerName, true
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
	cs := pod.Spec.Containers
	if holderInit {
		cs = pod.Spec.InitContainers
	}
	for i := range cs {
		if c := &cs[i]; c.Name == holder {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: spiffeVolume, MountPath: spiffeMountDir, ReadOnly: true})
			c.Env = append(c.Env, corev1.EnvVar{Name: "SPIFFE_ENDPOINT_SOCKET", Value: workloadAPISocket()})
			if !holderInit {
				c.Env = append(c.Env, brokerTLSEnv(sb)...)
			}
		}
	}
}

// brokerTLSEnv lists the allow rules with tls.originate for the built-in
// broker, in the formats of KUBESWIFT_SANDBOX_EGRESS_ALLOW (the resolved
// cidr and service entries of those rules) and KUBESWIFT_SANDBOX_EGRESS_HOSTS.
func brokerTLSEnv(sb *sandboxv1alpha1.SwiftSandbox) []corev1.EnvVar {
	from, hosts := tlsRules(sb)
	var tls []sandboxv1alpha1.SandboxEgressAllowed
	for _, a := range egressAllowed(sb) {
		if from[a.From] {
			tls = append(tls, a)
		}
	}
	var out []corev1.EnvVar
	if v := egressAllowEnv(tls); v != "" {
		out = append(out, corev1.EnvVar{Name: "KUBESWIFT_EGRESS_BROKER_TLS_ALLOW", Value: v})
	}
	if len(hosts) > 0 {
		out = append(out, corev1.EnvVar{Name: "KUBESWIFT_EGRESS_BROKER_TLS_HOSTS", Value: strings.Join(hosts, " ")})
	}
	return out
}

// tlsRules are the allow rules with tls.originate: the From labels of the
// cidr and service rules among them (as on their resolved entries) and the
// host rules as "<host>,<port>" (0: every port), sorted.
func tlsRules(sb *sandboxv1alpha1.SwiftSandbox) (map[string]bool, []string) {
	e := sb.Spec.Network.Egress
	if e == nil {
		return nil, nil
	}
	from := map[string]bool{}
	var hosts []string
	for _, r := range e.Allow {
		if r.TLS == nil || r.TLS.Originate != sandboxv1alpha1.SandboxEgressTLSSPIFFE {
			continue
		}
		switch {
		case r.Host != "":
			if len(r.Ports) == 0 {
				hosts = append(hosts, r.Host+",0")
			}
			for _, p := range r.Ports {
				hosts = append(hosts, r.Host+","+strconv.Itoa(int(p.Port)))
			}
		case r.CIDR != "":
			if cidr, err := sandboxwebhook.EgressCIDR(r.CIDR); err == nil {
				from["cidr "+cidr] = true
			}
		case r.Service != nil:
			ns := r.Service.Namespace
			if ns == "" {
				ns = sb.Namespace
			}
			from["service "+ns+"/"+r.Service.Name] = true
		}
	}
	sort.Strings(hosts)
	return from, hosts
}

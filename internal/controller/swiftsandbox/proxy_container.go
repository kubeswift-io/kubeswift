package swiftsandbox

import (
	"encoding/json"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	egressintentv1 "github.com/kubeswift-io/kubeswift/api/egressintent/v1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// An egress proxy (spec.network.egress.proxy) replaces the built-in broker of
// a proxied sandbox: an ordinary, unprivileged container in the launcher pod,
// started before the launcher (a native sidecar, restarted if it exits) under
// EgressProxyUID, so network-init's uid chain holds its connections to the
// restricted rules plus proxy.upstreams. Guest TCP is redirected to its
// transparent listener; with a DNS listener, dnsmasq forwards every guest
// query to it. In config Intent it reads the egress-intent file.
const (
	EgressProxyContainerName = "egress-proxy"
	egressIntentKey          = "egress-intent.json"
	egressIntentVolume       = "egress-intent"
	egressProxyTmpVolume     = "egress-proxy-tmp"
	egressProxyNoToken       = "egress-proxy-no-token"
)

// egressProxy is the sandbox's proxy container spec, or nil for the built-in
// broker (or a mode other than proxied).
func egressProxy(sb *sandboxv1alpha1.SwiftSandbox) *sandboxv1alpha1.SandboxEgressProxy {
	if !proxiedLauncher(sb) || sb.Spec.Network.Egress == nil {
		return nil
	}
	return sb.Spec.Network.Egress.Proxy
}

// proxyConfig is the proxy's policy model: as set, else Intent for a preset
// and Self for an image.
func proxyConfig(p *sandboxv1alpha1.SandboxEgressProxy) sandboxv1alpha1.SandboxEgressProxyConfig {
	if p.Config != "" {
		return p.Config
	}
	if p.Preset != "" {
		return sandboxv1alpha1.SandboxEgressProxyConfigIntent
	}
	return sandboxv1alpha1.SandboxEgressProxyConfigSelf
}

func intentConfig(sb *sandboxv1alpha1.SwiftSandbox) bool {
	p := egressProxy(sb)
	return p != nil && proxyConfig(p) == sandboxv1alpha1.SandboxEgressProxyConfigIntent
}

// proxyKey is the proxy spec in a pool's slot shape: a slot booted with one
// proxy is not handed to a sandbox asking for another.
func proxyKey(n sandboxv1alpha1.SandboxNetwork) string {
	if n.Mode != sandboxv1alpha1.SandboxNetworkProxied || n.Egress == nil || n.Egress.Proxy == nil {
		return ""
	}
	b, err := json.Marshal(n.Egress.Proxy)
	if err != nil {
		// A struct of strings, ints and EnvVars always marshals.
		panic(err)
	}
	return string(b)
}

// upstreamsEnv renders proxy.upstreams for network-init's uid chain, like
// egressAllowEnv: "<proto>,<cidr>,<port>". Only cidr entries (service
// entries are refused in this version). Validated, so only [a-z0-9.,/].
func upstreamsEnv(p *sandboxv1alpha1.SandboxEgressProxy) string {
	var out []string
	for _, r := range p.Upstreams {
		cidr, err := sandboxwebhook.EgressCIDR(r.CIDR)
		if err != nil {
			continue
		}
		if len(r.Ports) == 0 {
			out = append(out, "any,"+cidr+",0")
		}
		for _, pt := range r.Ports {
			out = append(out, "tcp,"+cidr+","+strconv.Itoa(int(pt.Port)))
		}
	}
	return strings.Join(out, " ")
}

// egressIntentJSON is the egress-intent.kubeswift.io/v1 file for the proxy:
// the guest's allowlist (host rules as written, cidr and service rules as
// resolved) and the listeners. ownerKind/ownerName name the pool for a warm
// slot, the sandbox otherwise.
func egressIntentJSON(sb *sandboxv1alpha1.SwiftSandbox, ownerKind, ownerName string) (string, error) {
	p := egressProxy(sb)
	in := egressintentv1.EgressIntent{
		APIVersion: egressintentv1.APIVersion,
		Kind:       egressintentv1.Kind,
		Owner:      egressintentv1.Owner{Namespace: sb.Namespace, Kind: ownerKind, Name: ownerName},
		Allow:      []egressintentv1.Rule{},
	}
	if p != nil && p.Listeners != nil {
		in.Listeners = egressintentv1.Listeners{Transparent: p.Listeners.Transparent, DNS: p.Listeners.DNS, HTTP: p.Listeners.HTTP}
	}
	for _, r := range sb.Spec.Network.Egress.Allow {
		if r.Host == "" {
			continue
		}
		rule := egressintentv1.Rule{Host: r.Host, From: "host " + r.Host}
		for _, pt := range r.Ports {
			rule.Ports = append(rule.Ports, egressintentv1.Port{Port: pt.Port, Protocol: string(corev1.ProtocolTCP)})
		}
		in.Allow = append(in.Allow, rule)
	}
	// Resolved cidr and service rules, one per address, ports merged.
	byCIDR := map[string]int{}
	for _, a := range egressAllowed(sb) {
		key := a.CIDR + " " + a.From
		i, seen := byCIDR[key]
		if !seen {
			in.Allow = append(in.Allow, egressintentv1.Rule{CIDR: a.CIDR, From: a.From})
			i = len(in.Allow) - 1
			byCIDR[key] = i
		}
		if a.Port != 0 {
			in.Allow[i].Ports = append(in.Allow[i].Ports, egressintentv1.Port{Port: a.Port, Protocol: string(corev1.ProtocolTCP)})
		}
	}
	b, err := json.MarshalIndent(in, "", "  ")
	return string(b), err
}

// addEgressIntent puts the egress-intent file in the launcher's intent
// ConfigMap (immutable, owner-controlled, like the runtime intent) when the
// proxy is in config Intent.
func addEgressIntent(cm *corev1.ConfigMap, sb *sandboxv1alpha1.SwiftSandbox, ownerKind, ownerName string) error {
	if !intentConfig(sb) {
		return nil
	}
	js, err := egressIntentJSON(sb, ownerKind, ownerName)
	if err != nil {
		return err
	}
	cm.Data[egressIntentKey] = js
	return nil
}

// hasEgressProxy reports whether a launcher pod runs a proxy container.
func hasEgressProxy(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == EgressProxyContainerName {
			return true
		}
	}
	return false
}

// addEgressProxy adds the proxy container to a proxied launcher pod: a native
// sidecar (an init container that keeps running), last among the init
// containers so network-init has installed the redirect and the uid chain
// before it starts, and started before the launcher.
func addEgressProxy(pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) {
	p := egressProxy(sb)
	if p == nil {
		return
	}
	uid := int64(EgressProxyUID)
	c := corev1.Container{
		Name:          EgressProxyContainerName,
		Image:         p.Image,
		Args:          append([]string(nil), p.Args...),
		Env:           append([]corev1.EnvVar(nil), p.Env...),
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To(uid),
			RunAsGroup:               ptr.To(uid),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Privileged:               ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: egressProxyTmpVolume, MountPath: "/tmp"},
			// The launcher's service account token is not the proxy's.
			{Name: egressProxyNoToken, MountPath: serviceAccountPath, ReadOnly: true},
		},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{Name: egressProxyTmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: resource.NewQuantity(64<<20, resource.BinarySI)}}},
		corev1.Volume{Name: egressProxyNoToken, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(1<<10, resource.BinarySI)}}},
	)
	if intentConfig(sb) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: egressIntentVolume, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: intentConfigMapName(sb)},
				Items:                []corev1.KeyToPath{{Key: egressIntentKey, Path: "intent.json"}},
			}}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: egressIntentVolume, MountPath: "/etc/kubeswift/egress", ReadOnly: true})
		c.Env = append(c.Env, corev1.EnvVar{Name: "KUBESWIFT_EGRESS_INTENT", Value: egressintentv1.Path})
	}
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, c)
}

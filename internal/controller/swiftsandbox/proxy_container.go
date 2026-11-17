package swiftsandbox

import (
	"encoding/json"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	egressintentv1 "github.com/kubeswift-io/kubeswift/api/egressintent/v1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
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
	egressProxyConfigVolume  = "egress-proxy-config"
	// EgressTranslatorContainerName runs a preset's translator.
	EgressTranslatorContainerName = "egress-translator"
	// EgressProxyConfigDir is where a translator writes the proxy's
	// configuration, and where the proxy reads it.
	EgressProxyConfigDir = "/etc/kubeswift/egress-proxy"
)

// resolvedProxy is the proxy a launcher runs: the spec's image, or the
// administrator's preset it names, with the policy model decided.
type resolvedProxy struct {
	Preset          string                                      `json:"preset,omitempty"`
	Image           string                                      `json:"image"`
	Args            []string                                    `json:"args,omitempty"`
	Env             []corev1.EnvVar                             `json:"env,omitempty"`
	TranslatorImage string                                      `json:"translatorImage,omitempty"`
	TranslatorArgs  []string                                    `json:"translatorArgs,omitempty"`
	Listeners       sandboxv1alpha1.SandboxEgressProxyListeners `json:"listeners"`
	Config          sandboxv1alpha1.SandboxEgressProxyConfig    `json:"config"`
	Upstreams       []sandboxv1alpha1.SandboxEgressRule         `json:"upstreams,omitempty"`
}

// resolveProxySpec resolves a network's proxy; nil for the built-in broker,
// a mode other than proxied, or a preset this cluster does not define (which
// validation refuses before any launcher is built).
func resolveProxySpec(n sandboxv1alpha1.SandboxNetwork) *resolvedProxy {
	if n.Mode != sandboxv1alpha1.SandboxNetworkProxied || n.Egress == nil || n.Egress.Proxy == nil {
		return nil
	}
	spec := n.Egress.Proxy
	rp := &resolvedProxy{Preset: spec.Preset, Config: proxyConfig(spec), Upstreams: spec.Upstreams}
	if spec.Preset != "" {
		pr, ok := sandboxwebhook.EgressProxyPresets[spec.Preset]
		if !ok {
			return nil
		}
		rp.Image, rp.Args = pr.Image, pr.Args
		rp.TranslatorImage, rp.TranslatorArgs = pr.TranslatorImage, pr.TranslatorArgs
		rp.Listeners = sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: pr.Listeners.Transparent, DNS: pr.Listeners.DNS, HTTP: pr.Listeners.HTTP}
		return rp
	}
	if spec.Listeners == nil {
		return nil
	}
	rp.Image, rp.Args, rp.Env, rp.Listeners = spec.Image, spec.Args, spec.Env, *spec.Listeners
	return rp
}

// egressProxy is the sandbox's resolved proxy, or nil for the built-in broker
// (or a mode other than proxied).
func egressProxy(sb *sandboxv1alpha1.SwiftSandbox) *resolvedProxy {
	if !proxiedLauncher(sb) {
		return nil
	}
	return resolveProxySpec(sb.Spec.Network)
}

// intentReader is the image that reads the egress-intent file: the preset's
// translator, else the proxy itself. "" when the proxy is in config Self.
func (p *resolvedProxy) intentReader() string {
	if p == nil || p.Config != sandboxv1alpha1.SandboxEgressProxyConfigIntent {
		return ""
	}
	if p.TranslatorImage != "" {
		return p.TranslatorImage
	}
	return p.Image
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
	return p != nil && p.Config == sandboxv1alpha1.SandboxEgressProxyConfigIntent
}

// proxyKey is the resolved proxy in a pool's slot shape: a slot booted with
// one proxy (or an earlier definition of a preset) is not handed to a
// sandbox asking for another.
func proxyKey(n sandboxv1alpha1.SandboxNetwork) string {
	rp := resolveProxySpec(n)
	if rp == nil {
		return ""
	}
	b, err := json.Marshal(rp)
	if err != nil {
		// A struct of strings, ints and EnvVars always marshals.
		panic(err)
	}
	return string(b)
}

// intentFeatures are the optional parts of v1 the sandbox's intent file uses.
func intentFeatures(sb *sandboxv1alpha1.SwiftSandbox) []string {
	if froms, hosts := tlsRules(sb); len(froms) > 0 || len(hosts) > 0 {
		return []string{egressintentv1.FeatureTLSOriginate}
	}
	return nil
}

// intentFeatureVerdict refuses an intent reader that does not declare a
// feature the sandbox's intent uses (FeaturesLabel).
func intentFeatureVerdict(image string, labels map[string]string, sb *sandboxv1alpha1.SwiftSandbox) (reason, msg string) {
	have := map[string]bool{}
	for _, f := range strings.Split(labels[egressintentv1.FeaturesLabel], ",") {
		have[strings.TrimSpace(f)] = true
	}
	for _, f := range intentFeatures(sb) {
		if !have[f] {
			return reasonEgressIntentUnsupported, "egress proxy image " + image + " does not list " + f + " in its " +
				egressintentv1.FeaturesLabel + " label, which spec.network.egress.allow[].tls needs"
		}
	}
	return "", ""
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
	if p != nil {
		in.Listeners = egressintentv1.Listeners{Transparent: p.Listeners.Transparent, DNS: p.Listeners.DNS, HTTP: p.Listeners.HTTP}
	}
	for _, r := range sb.Spec.Network.Egress.Allow {
		if r.Host == "" {
			continue
		}
		rule := egressintentv1.Rule{Host: r.Host, From: "host " + r.Host}
		if r.TLS != nil {
			rule.TLS = &egressintentv1.RuleTLS{Originate: string(r.TLS.Originate)}
		}
		for _, pt := range r.Ports {
			rule.Ports = append(rule.Ports, egressintentv1.Port{Port: pt.Port, Protocol: string(corev1.ProtocolTCP)})
		}
		in.Allow = append(in.Allow, rule)
	}
	// Resolved cidr and service rules, one per address, ports merged.
	tlsFrom, _ := tlsRules(sb)
	byCIDR := map[string]int{}
	for _, a := range egressAllowed(sb) {
		key := a.CIDR + " " + a.From
		i, seen := byCIDR[key]
		if !seen {
			in.Allow = append(in.Allow, egressintentv1.Rule{CIDR: a.CIDR, From: a.From})
			i = len(in.Allow) - 1
			if tlsFrom[a.From] {
				in.Allow[i].TLS = &egressintentv1.RuleTLS{Originate: string(sandboxv1alpha1.SandboxEgressTLSSPIFFE)}
			}
			byCIDR[key] = i
		}
		if a.Port != 0 {
			in.Allow[i].Ports = append(in.Allow[i].Ports, egressintentv1.Port{Port: a.Port, Protocol: string(corev1.ProtocolTCP)})
		}
	}
	key := coldPlaceholderKey(sb)
	if ownerKind == "SwiftSandboxPool" {
		key = poolPlaceholderKey(sb.Namespace, ownerName)
	}
	addCredentialsToIntent(&in, sb.Spec.Network, key)
	if identityEnabled(sb.Spec.Identity) && sandboxwebhook.SPIFFE != nil {
		id := coldSPIFFEID(sb)
		if ownerKind == "SwiftSandboxPool" {
			id = slotSPIFFEID(sb.Namespace, ownerName, sb.Name)
		}
		in.Identity = &egressintentv1.Identity{SPIFFE: &egressintentv1.SPIFFE{WorkloadAPISocket: workloadAPISocket(), ID: id}}
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
	if intentConfig(sb) && p.TranslatorImage != "" {
		// The translator runs once, before the proxy starts, and writes the
		// proxy's configuration into a volume the proxy reads.
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: egressProxyConfigVolume, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(4<<20, resource.BinarySI)}}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: egressProxyConfigVolume, MountPath: EgressProxyConfigDir, ReadOnly: true})
		t := corev1.Container{
			Name:            EgressTranslatorContainerName,
			Image:           p.TranslatorImage,
			Args:            append([]string(nil), p.TranslatorArgs...),
			Env:             []corev1.EnvVar{{Name: "KUBESWIFT_EGRESS_INTENT", Value: egressintentv1.Path}, {Name: "KUBESWIFT_EGRESS_PROXY_CONFIG_DIR", Value: EgressProxyConfigDir}},
			SecurityContext: c.SecurityContext.DeepCopy(),
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: egressIntentVolume, MountPath: "/etc/kubeswift/egress", ReadOnly: true},
				{Name: egressProxyConfigVolume, MountPath: EgressProxyConfigDir},
				{Name: egressProxyNoToken, MountPath: serviceAccountPath, ReadOnly: true},
			},
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, t)
	}
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, c)
}

// reasonEgressIntentUnsupported: the image that reads the egress-intent file
// does not declare a version this controller writes.
const reasonEgressIntentUnsupported = "EgressIntentUnsupported"

// intentReaderRequest is the registry request for the image that reads the
// egress-intent file (a preset's translator, else the proxy), so its
// io.kubeswift.egress-intent.versions label can be checked before any
// launcher starts. ok=false when nothing reads the file.
func intentReaderRequest(sb *sandboxv1alpha1.SwiftSandbox) (req resolveRequest, image string, ok bool) {
	image = egressProxy(sb).intentReader()
	if image == "" {
		return resolveRequest{}, "", false
	}
	return newResolveRequest(resolveImageKind, image, materialize.ModeBlock, rootfsCacheDir, nil), image, true
}

// intentVersionVerdict refuses an intent reader whose label does not list
// the version KubeSwift writes; "" when it does.
func intentVersionVerdict(image string, labels map[string]string, sb *sandboxv1alpha1.SwiftSandbox) (reason, msg string) {
	declared := labels[egressintentv1.ImageLabel]
	for _, v := range strings.Split(declared, ",") {
		if strings.TrimSpace(v) == "v1" {
			return intentFeatureVerdict(image, labels, sb)
		}
	}
	if declared == "" {
		return reasonEgressIntentUnsupported, "egress proxy image " + image + " has no " + egressintentv1.ImageLabel +
			" label, so it does not say it reads egress-intent.kubeswift.io/v1"
	}
	return reasonEgressIntentUnsupported, "egress proxy image " + image + " reads egress-intent versions " + declared +
		"; this KubeSwift writes v1"
}

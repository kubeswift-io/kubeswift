package swiftsandbox

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// Mode proxied (built-in broker): the guest's TCP is redirected in the
// launcher pod to a broker swiftletd runs under EgressProxyUID, DNS goes to
// the launcher's dnsmasq, which answers only allowed names, and nothing is
// forwarded. network-init installs the redirect and the chains; the
// EgressProxyUID chain applies the restricted rules to the broker's own
// connections, so the broker can never reach what a restricted guest cannot.
const (
	// EgressProxyUID runs the broker (and, later, a proxy container).
	EgressProxyUID = 65531
)

// proxiedLauncher is whether buildPod sets the sandbox up for mode proxied.
func proxiedLauncher(sb *sandboxv1alpha1.SwiftSandbox) bool {
	return sb.Spec.Network.Mode == sandboxv1alpha1.SandboxNetworkProxied
}

// egressHostsEnv renders the host rules for dnsmasq and the broker,
// space-separated "<host>,<port>" with port 0 for every port, sorted. Hosts
// were validated (lowercase DNS names, optional leading "*."), so the value
// holds only [a-z0-9.*,-].
func egressHostsEnv(n sandboxv1alpha1.SandboxNetwork) string {
	if n.Egress == nil {
		return ""
	}
	var out []string
	for _, r := range n.Egress.Allow {
		if r.Host == "" {
			continue
		}
		if len(r.Ports) == 0 {
			out = append(out, r.Host+",0")
		}
		for _, p := range r.Ports {
			out = append(out, r.Host+","+strconv.Itoa(int(p.Port)))
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// egressServicesEnv names the Services the allowlist reaches, as
// "<name>.<namespace>", so dnsmasq resolves their cluster names
// (<name>.<namespace>.svc.<cluster domain>). Only resolution: the broker
// still connects to nothing but the resolved ClusterIP rules.
func egressServicesEnv(ns string, n sandboxv1alpha1.SandboxNetwork) string {
	if n.Egress == nil {
		return ""
	}
	set := map[string]bool{}
	for _, r := range n.Egress.Allow {
		if r.Service == nil {
			continue
		}
		svcNS := r.Service.Namespace
		if svcNS == "" {
			svcNS = ns
		}
		set[r.Service.Name+"."+svcNS] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// proxiedEnv is the environment network-init and the launcher both need for
// mode proxied.
//
// KUBESWIFT_EGRESS_BROKER_PORT is the transparent listener guest TCP is
// redirected to: the built-in broker's, or the proxy container's.
// KUBESWIFT_SANDBOX_EGRESS_ALLOW is what the uid chain lets the broker or
// proxy reach besides DNS and the public internet: the resolved allowlist
// for the broker (which enforces it), proxy.upstreams for a proxy (which
// enforces the allowlist itself).
func proxiedEnv(sb *sandboxv1alpha1.SwiftSandbox) []corev1.EnvVar {
	port := sandboxwebhook.EgressBrokerPort
	allow := egressAllowEnv(egressAllowed(sb))
	var extra []corev1.EnvVar
	if p := egressProxy(sb); p != nil && p.Listeners != nil {
		port = int(p.Listeners.Transparent)
		allow = upstreamsEnv(p)
		extra = append(extra, corev1.EnvVar{Name: "KUBESWIFT_EGRESS_PROXY", Value: "container"})
		if p.Listeners.DNS != 0 {
			extra = append(extra, corev1.EnvVar{Name: "KUBESWIFT_EGRESS_PROXY_DNS_PORT", Value: strconv.Itoa(int(p.Listeners.DNS))})
		}
	}
	env := []corev1.EnvVar{
		{Name: "KUBESWIFT_EGRESS_BROKER_PORT", Value: strconv.Itoa(port)},
		{Name: "KUBESWIFT_EGRESS_PROXY_UID", Value: strconv.Itoa(EgressProxyUID)},
	}
	env = append(env, extra...)
	if allow != "" {
		env = append(env, corev1.EnvVar{Name: "KUBESWIFT_SANDBOX_EGRESS_ALLOW", Value: allow})
	}
	if hosts := egressHostsEnv(sb.Spec.Network); hosts != "" {
		env = append(env, corev1.EnvVar{Name: "KUBESWIFT_SANDBOX_EGRESS_HOSTS", Value: hosts})
	}
	if svcs := egressServicesEnv(sb.Namespace, sb.Spec.Network); svcs != "" {
		env = append(env, corev1.EnvVar{Name: "KUBESWIFT_SANDBOX_EGRESS_SERVICES", Value: svcs})
	}
	return env
}

// addProxiedLauncherEnv gives the launcher container what dnsmasq (the
// entrypoint) and the broker (swiftletd) read in mode proxied.
func addProxiedLauncherEnv(pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) {
	if !networked(sb) || !proxiedLauncher(sb) {
		return
	}
	for i := range pod.Spec.Containers {
		if c := &pod.Spec.Containers[i]; c.Name == launcherName {
			c.Env = append(c.Env, corev1.EnvVar{Name: "KUBESWIFT_SANDBOX_EGRESS", Value: string(sandboxv1alpha1.SandboxNetworkProxied)})
			c.Env = append(c.Env, proxiedEnv(sb)...)
		}
	}
}

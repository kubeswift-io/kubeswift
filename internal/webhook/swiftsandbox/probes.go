package swiftsandbox

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// ValidateProbes checks spec.readinessProbe and spec.livenessProbe. swiftletd
// runs them against the guest from inside the launcher, so only what can run
// there is accepted: httpGet (HTTP) or tcpSocket, no host, and a port that is a
// number or the name of a spec.network.ports entry. The controller runs it too.
func ValidateProbes(s *sandboxv1alpha1.SwiftSandboxSpec) error {
	for _, pr := range []struct {
		field    string
		p        *corev1.Probe
		liveness bool
	}{{"spec.readinessProbe", s.ReadinessProbe, false}, {"spec.livenessProbe", s.LivenessProbe, true}} {
		if pr.p == nil {
			continue
		}
		if err := validateProbe(pr.p, s.Network, pr.liveness); err != nil {
			return fmt.Errorf("%s: %w", pr.field, err)
		}
	}
	return nil
}

func validateProbe(p *corev1.Probe, n sandboxv1alpha1.SandboxNetwork, liveness bool) error {
	if n.Mode == sandboxv1alpha1.SandboxNetworkNone {
		return fmt.Errorf("needs a network to reach the guest; mode none has none")
	}
	switch {
	case p.Exec != nil:
		return fmt.Errorf("exec probes are not supported; use httpGet or tcpSocket")
	case p.GRPC != nil:
		return fmt.Errorf("grpc probes are not supported; use httpGet or tcpSocket")
	case p.HTTPGet != nil && p.TCPSocket != nil:
		return fmt.Errorf("set exactly one of httpGet and tcpSocket")
	case p.HTTPGet != nil:
		h := p.HTTPGet
		if h.Host != "" {
			return fmt.Errorf("httpGet.host cannot be set: the probe targets the guest")
		}
		if h.Scheme != "" && h.Scheme != corev1.URISchemeHTTP {
			return fmt.Errorf("httpGet.scheme %s is not supported; only HTTP", h.Scheme)
		}
		if h.Path != "" && !strings.HasPrefix(h.Path, "/") {
			return fmt.Errorf("httpGet.path %q must start with /", h.Path)
		}
		if strings.ContainsAny(h.Path, " \r\n") {
			return fmt.Errorf("httpGet.path %q contains whitespace", h.Path)
		}
		if len(h.HTTPHeaders) > 10 {
			return fmt.Errorf("httpGet.httpHeaders has %d entries, at most 10", len(h.HTTPHeaders))
		}
		for _, hd := range h.HTTPHeaders {
			if errs := validation.IsHTTPHeaderName(hd.Name); len(errs) > 0 {
				return fmt.Errorf("httpGet.httpHeaders %q: %s", hd.Name, errs[0])
			}
			if strings.ContainsAny(hd.Value, "\r\n") {
				return fmt.Errorf("httpGet.httpHeaders %q: the value contains a line break", hd.Name)
			}
		}
		if _, err := ProbePort(h.Port, n); err != nil {
			return fmt.Errorf("httpGet.port: %w", err)
		}
	case p.TCPSocket != nil:
		if p.TCPSocket.Host != "" {
			return fmt.Errorf("tcpSocket.host cannot be set: the probe targets the guest")
		}
		if _, err := ProbePort(p.TCPSocket.Port, n); err != nil {
			return fmt.Errorf("tcpSocket.port: %w", err)
		}
	default:
		return fmt.Errorf("set httpGet or tcpSocket")
	}
	if p.TerminationGracePeriodSeconds != nil {
		return fmt.Errorf("terminationGracePeriodSeconds is not supported")
	}
	for name, v := range map[string]int32{
		"initialDelaySeconds": p.InitialDelaySeconds, "periodSeconds": p.PeriodSeconds,
		"timeoutSeconds": p.TimeoutSeconds, "successThreshold": p.SuccessThreshold,
		"failureThreshold": p.FailureThreshold,
	} {
		if v < 0 || v > 86400 {
			return fmt.Errorf("%s %d is out of range 0-86400", name, v)
		}
	}
	if liveness && p.SuccessThreshold > 1 {
		return fmt.Errorf("successThreshold must be 1 for a liveness probe")
	}
	return nil
}

// ProbePort resolves a probe port: a number, or the name of a
// spec.network.ports entry.
func ProbePort(port intstr.IntOrString, n sandboxv1alpha1.SandboxNetwork) (int32, error) {
	if port.Type == intstr.Int {
		if port.IntVal < 1 || port.IntVal > 65535 {
			return 0, fmt.Errorf("%d is out of range 1-65535", port.IntVal)
		}
		return port.IntVal, nil
	}
	for _, p := range n.Ports {
		if p.Name == port.StrVal {
			return p.Port, nil
		}
	}
	return 0, fmt.Errorf("%q names no spec.network.ports entry", port.StrVal)
}

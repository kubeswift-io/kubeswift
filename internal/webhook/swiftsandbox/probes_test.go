package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func httpProbe(port intstr.IntOrString) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: port}}}
}

func probedSpec(readiness, liveness *corev1.Probe) *sandboxv1alpha1.SwiftSandboxSpec {
	return &sandboxv1alpha1.SwiftSandboxSpec{
		Image:          "busybox:1",
		Network:        sandboxv1alpha1.SandboxNetwork{Ports: []sandboxv1alpha1.SandboxPort{{Name: "http-app", Port: 3000}}},
		ReadinessProbe: readiness,
		LivenessProbe:  liveness,
	}
}

func TestValidateProbes(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *sandboxv1alpha1.SwiftSandboxSpec
	}{
		{"http on a named port", probedSpec(httpProbe(intstr.FromString("http-app")), nil)},
		{"http on an undeclared number", probedSpec(httpProbe(intstr.FromInt32(8080)), nil)},
		{"tcp liveness", probedSpec(nil, &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http-app")}}, FailureThreshold: 2})},
		{"explicit HTTP scheme and headers", probedSpec(&corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Port: intstr.FromInt32(80), Scheme: corev1.URISchemeHTTP, HTTPHeaders: []corev1.HTTPHeader{{Name: "X-Probe", Value: "1"}}}}}, nil)},
	} {
		if err := ValidateProbes(tc.s); err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
	}

	exec := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
	grpc := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 9000}}}
	withHost := httpProbe(intstr.FromInt32(80))
	withHost.HTTPGet.Host = "10.0.0.1"
	https := httpProbe(intstr.FromInt32(443))
	https.HTTPGet.Scheme = corev1.URISchemeHTTPS
	relative := httpProbe(intstr.FromInt32(80))
	relative.HTTPGet.Path = "healthz"
	crlf := httpProbe(intstr.FromInt32(80))
	crlf.HTTPGet.HTTPHeaders = []corev1.HTTPHeader{{Name: "X", Value: "a\r\nInjected: 1"}}
	grace := httpProbe(intstr.FromInt32(80))
	grace.TerminationGracePeriodSeconds = new(int64)
	both := httpProbe(intstr.FromInt32(80))
	both.TCPSocket = &corev1.TCPSocketAction{Port: intstr.FromInt32(80)}
	livenessTwice := httpProbe(intstr.FromInt32(80))
	livenessTwice.SuccessThreshold = 2
	noNet := probedSpec(httpProbe(intstr.FromInt32(80)), nil)
	noNet.Network = sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkNone}

	for _, tc := range []struct {
		name, want string
		s          *sandboxv1alpha1.SwiftSandboxSpec
	}{
		{"exec", "exec probes are not supported", probedSpec(exec, nil)},
		{"grpc", "grpc probes are not supported", probedSpec(nil, grpc)},
		{"a host", "host cannot be set", probedSpec(withHost, nil)},
		{"HTTPS", "only HTTP", probedSpec(https, nil)},
		{"a relative path", "must start with /", probedSpec(relative, nil)},
		{"a header with a line break", "line break", probedSpec(crlf, nil)},
		{"terminationGracePeriodSeconds", "terminationGracePeriodSeconds", probedSpec(grace, nil)},
		{"two handlers", "exactly one", probedSpec(both, nil)},
		{"no handler", "set httpGet or tcpSocket", probedSpec(&corev1.Probe{}, nil)},
		{"an unknown port name", "names no spec.network.ports entry", probedSpec(httpProbe(intstr.FromString("nope")), nil)},
		{"liveness successThreshold 2", "must be 1 for a liveness probe", probedSpec(nil, livenessTwice)},
		{"mode none", "mode none has none", noNet},
	} {
		err := ValidateProbes(tc.s)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

package podmode

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const shim = "ghcr.io/kubeswift-io/kubeswift/pod-shim:v0.27.0"

// sandboxdPod is shaped like agent-sandbox's sandboxd example after the API
// server's defaulting and the ServiceAccount admission plugin.
func sandboxdPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-1", Namespace: "tenant", Labels: map[string]string{"app": "x"}},
		Spec: corev1.PodSpec{
			RuntimeClassName:              ptr.To(RuntimeClassName),
			RestartPolicy:                 corev1.RestartPolicyAlways,
			DNSPolicy:                     corev1.DNSNone,
			DNSConfig:                     &corev1.PodDNSConfig{Nameservers: []string{"8.8.8.8"}},
			SchedulerName:                 "default-scheduler",
			TerminationGracePeriodSeconds: ptr.To[int64](30),
			ServiceAccountName:            "default",
			EnableServiceLinks:            ptr.To(true),
			SecurityContext:               &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), FSGroup: ptr.To[int64](1000)},
			Tolerations:                   []corev1.Toleration{{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists}},
			Volumes: []corev1.Volume{
				{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "kube-api-access-abcde", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}},
				}}},
			},
			Containers: []corev1.Container{{
				Name:       "sandbox",
				Image:      "registry.k8s.io/agent-sandbox/sandboxd:v1.0.12",
				Args:       []string{"--root", "/workspace"},
				Env:        []corev1.EnvVar{{Name: "A", Value: "b"}},
				WorkingDir: "/workspace",
				Ports: []corev1.ContainerPort{
					{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
					{ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
				},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
					Path: "/v1/health", Port: intstr.FromInt32(8080)}}},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1500m"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: "/workspace"},
					{Name: "kube-api-access-abcde", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, RunAsNonRoot: ptr.To(true),
				},
				ImagePullPolicy:          corev1.PullIfNotPresent,
				TerminationMessagePath:   corev1.TerminationMessagePathDefault,
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			}},
		},
	}
}

func TestRewriteStandIn(t *testing.T) {
	in := sandboxdPod()
	out, err := Rewrite(in, shim)
	if err != nil {
		t.Fatal(err)
	}
	if in.Spec.RuntimeClassName == nil || len(in.Spec.Volumes) != 2 {
		t.Fatal("Rewrite changed its input")
	}
	s := out.Spec
	if s.RuntimeClassName != nil || s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken {
		t.Errorf("runtimeClassName %v, automount %v", s.RuntimeClassName, s.AutomountServiceAccountToken)
	}
	if out.Labels[LabelStandIn] != "true" || out.Labels["app"] != "x" {
		t.Errorf("labels %v", out.Labels)
	}
	if s.NodeSelector["kubeswift.io/kernel-node"] != "true" || len(s.Tolerations) != 1 {
		t.Errorf("scheduling not kept: %v %v", s.NodeSelector, s.Tolerations)
	}
	if len(s.Volumes) != 1 || !shimVolumeOK(s.Volumes[0]) || len(s.Volumes[0].Projected.Sources) != 2 ||
		s.Volumes[0].Projected.Sources[1].ServiceAccountToken == nil {
		t.Errorf("volumes %+v", s.Volumes)
	}
	if len(s.Containers) != 1 {
		t.Fatalf("containers %d", len(s.Containers))
	}
	c := s.Containers[0]
	if c.Name != "sandbox" || c.Image != shim || len(c.Command) != 0 {
		t.Errorf("container %s %s %v", c.Name, c.Image, c.Command)
	}
	if want := []string{"pod-shim", "--annotations-file", ShimAnnotationsFile, "--ports", "8080,9090"}; !reflect.DeepEqual(c.Args, want) {
		t.Errorf("args %v, want %v", c.Args, want)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/v1/health" {
		t.Error("readiness probe not kept")
	}
	if q := c.Resources.Requests[corev1.ResourceMemory]; q.String() != "1Gi" {
		t.Errorf("requests not kept: %v", c.Resources.Requests)
	}
	if sc := c.SecurityContext; *sc.RunAsUser != ShimUID || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation {
		t.Errorf("shim security context %+v", sc)
	}
	if !ShapeOK(out, shim) {
		t.Error("ShapeOK false for a fresh stand-in")
	}

	w, err := ParseWorkload(out)
	if err != nil {
		t.Fatal(err)
	}
	if w.Image != "registry.k8s.io/agent-sandbox/sandboxd:v1.0.12" || w.WorkingDir != "/workspace" ||
		!reflect.DeepEqual(w.Args, []string{"--root", "/workspace"}) || len(w.Env) != 1 {
		t.Errorf("workload %+v", w)
	}
	if want := []Port{{Name: "http", Port: 8080}, {Name: "p9090", Port: 9090}}; !reflect.DeepEqual(w.Ports, want) {
		t.Errorf("ports %+v", w.Ports)
	}
	if w.CPU != 2 || w.Memory != "1024Mi" || *w.RunAsUser != 1000 || !*w.RunAsNonRoot || w.RunAsGroup != nil {
		t.Errorf("cpu %d memory %s user %v nonroot %v group %v", w.CPU, w.Memory, w.RunAsUser, w.RunAsNonRoot, w.RunAsGroup)
	}
}

func TestRewriteRefusesByName(t *testing.T) {
	cases := map[string]struct {
		mut  func(*corev1.Pod)
		want string
	}{
		"two containers": {func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, p.Spec.Containers[0]) }, "spec.containers (exactly one"},
		"init container": {func(p *corev1.Pod) { p.Spec.InitContainers = []corev1.Container{{Name: "i"}} }, "spec.initContainers"},
		"host network":   {func(p *corev1.Pod) { p.Spec.HostNetwork = true }, "spec.hostNetwork"},
		"host aliases":   {func(p *corev1.Pod) { p.Spec.HostAliases = []corev1.HostAlias{{IP: "1.2.3.4"}} }, "spec.hostAliases"},
		"sysctls":        {func(p *corev1.Pod) { p.Spec.SecurityContext.Sysctls = []corev1.Sysctl{{Name: "a", Value: "1"}} }, "spec.securityContext.sysctls"},
		"secret volume": {func(p *corev1.Pod) {
			p.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}
		}, "spec.volumes[workspace]"},
		"envFrom":        {func(p *corev1.Pod) { p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{}} }, "spec.containers[sandbox].envFrom"},
		"valueFrom":      {func(p *corev1.Pod) { p.Spec.Containers[0].Env[0].ValueFrom = &corev1.EnvVarSource{} }, "env[A].valueFrom"},
		"liveness":       {func(p *corev1.Pod) { p.Spec.Containers[0].LivenessProbe = &corev1.Probe{} }, "spec.containers[sandbox].livenessProbe"},
		"lifecycle":      {func(p *corev1.Pod) { p.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{} }, "spec.containers[sandbox].lifecycle"},
		"tty":            {func(p *corev1.Pod) { p.Spec.Containers[0].TTY = true }, "spec.containers[sandbox].tty"},
		"udp":            {func(p *corev1.Pod) { p.Spec.Containers[0].Ports[1].Protocol = corev1.ProtocolUDP }, "ports[9090].protocol"},
		"host port":      {func(p *corev1.Pod) { p.Spec.Containers[0].Ports[0].HostPort = 80 }, "ports[8080].hostPort"},
		"duplicate port": {func(p *corev1.Pod) { p.Spec.Containers[0].Ports[1].ContainerPort = 8080 }, "declared twice"},
		"privileged":     {func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true) }, "securityContext.privileged"},
		"capability add": {func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		}, "capabilities.add"},
		"other mount":     {func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].Name = "nope" }, "volumeMounts[nope]"},
		"resource claims": {func(p *corev1.Pod) { p.Spec.Containers[0].Resources.Claims = []corev1.ResourceClaim{{Name: "gpu"}} }, "resources.claims"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := sandboxdPod()
			tc.mut(p)
			_, err := Rewrite(p, shim)
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("err %v, want a RefusedError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%q does not name %q", err, tc.want)
			}
		})
	}
}

func TestRewriteListsEveryRefusal(t *testing.T) {
	p := sandboxdPod()
	p.Spec.HostPID = true
	p.Spec.Containers[0].StartupProbe = &corev1.Probe{}
	_, err := Rewrite(p, shim)
	var refused *RefusedError
	if !errors.As(err, &refused) || len(refused.Fields) != 2 {
		t.Fatalf("err %v", err)
	}
}

func TestGuestSize(t *testing.T) {
	cases := []struct {
		r        corev1.ResourceRequirements
		cpu      int32
		mem      string
		describe string
	}{
		{corev1.ResourceRequirements{}, 1, "512Mi", "unset"},
		{corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")}}, 1, "256Mi", "small"},
		{corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("2G")},
		}, 3, "1908Mi", "limits win, rounded up to MiB"},
	}
	for _, tc := range cases {
		if got := guestCPU(tc.r); got != tc.cpu {
			t.Errorf("%s: cpu %d, want %d", tc.describe, got, tc.cpu)
		}
		if got := guestMemory(tc.r); got != tc.mem {
			t.Errorf("%s: memory %s, want %s", tc.describe, got, tc.mem)
		}
	}
}

func TestShapeOK(t *testing.T) {
	fresh := func() *corev1.Pod {
		p, err := Rewrite(sandboxdPod(), shim)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := fresh(); !ShapeOK(p, "ghcr.io/kubeswift-io/kubeswift/pod-shim:v0.28.0") {
		t.Error("a stand-in from before an upgrade (older tag) must stay OK")
	}
	bad := map[string]func(*corev1.Pod){
		"image":     func(p *corev1.Pod) { p.Spec.Containers[0].Image = "docker.io/library/busybox" },
		"command":   func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"/bin/sh"} },
		"args":      func(p *corev1.Pod) { p.Spec.Containers[0].Args = []string{"follow-console"} },
		"ephemeral": func(p *corev1.Pod) { p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}} },
		"volume":    func(p *corev1.Pod) { p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host"}) },
		"privileged": func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true)
		},
		"token for another audience": func(p *corev1.Pod) {
			p.Spec.Volumes[0].Projected.Sources[1].ServiceAccountToken.Audience = "https://kubernetes.default.svc"
		},
		"secret in the shim volume": func(p *corev1.Pod) {
			p.Spec.Volumes[0].Projected.Sources = append(p.Spec.Volumes[0].Projected.Sources,
				corev1.VolumeProjection{Secret: &corev1.SecretProjection{}})
		},
		"downward API volume instead": func(p *corev1.Pod) {
			p.Spec.Volumes[0].Projected = nil
			p.Spec.Volumes[0].DownwardAPI = &corev1.DownwardAPIVolumeSource{}
		},
	}
	for name, mut := range bad {
		p := fresh()
		mut(p)
		if ShapeOK(p, shim) {
			t.Errorf("%s: ShapeOK true", name)
		}
	}
}

func TestImageRepository(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/a/b:v1":                   "ghcr.io/a/b",
		"localhost:5000/a/b:v1":            "localhost:5000/a/b",
		"localhost:5000/a/b":               "localhost:5000/a/b",
		"ghcr.io/a/b@sha256:abc":           "ghcr.io/a/b",
		"ghcr.io/a/b:v1@sha256:abc":        "ghcr.io/a/b",
		"registry.example:443/x/y:sha-123": "registry.example:443/x/y",
	} {
		if got := imageRepository(in); got != want {
			t.Errorf("imageRepository(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseWorkloadRejects(t *testing.T) {
	for name, ann := range map[string]string{
		"missing":  "",
		"garbage":  "{",
		"version":  `{"v":2,"image":"x"}`,
		"no image": `{"v":1}`,
	} {
		p := &corev1.Pod{}
		if ann != "" {
			p.Annotations = map[string]string{AnnotationWorkload: ann}
		}
		if _, err := ParseWorkload(p); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestExitAnnotation(t *testing.T) {
	if v := FormatExit(3, 137); v != "3:137" {
		t.Errorf("FormatExit %q", v)
	}
	if run, code, ok := ParseExit("3:137"); !ok || run != 3 || code != 137 {
		t.Errorf("ParseExit %d %d %v", run, code, ok)
	}
	for _, bad := range []string{"", "3", "x:1", "0:1", "1:x"} {
		if _, _, ok := ParseExit(bad); ok {
			t.Errorf("ParseExit(%q) ok", bad)
		}
	}
	for _, tc := range []struct {
		policy corev1.RestartPolicy
		code   int
		want   bool
	}{
		{"", 0, true}, {corev1.RestartPolicyAlways, 0, true},
		{corev1.RestartPolicyOnFailure, 0, false}, {corev1.RestartPolicyOnFailure, 1, true},
		{corev1.RestartPolicyNever, 1, false},
	} {
		if got := Restarts(tc.policy, tc.code); got != tc.want {
			t.Errorf("Restarts(%q, %d) = %v", tc.policy, tc.code, got)
		}
	}
}

func TestRewriteAddsReadinessGateOnce(t *testing.T) {
	p := sandboxdPod()
	p.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: "example.com/ready"}, {ConditionType: ConditionMicroVMReady}}
	out, err := Rewrite(p, shim)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Spec.ReadinessGates) != 2 {
		t.Errorf("gates %v", out.Spec.ReadinessGates)
	}
}

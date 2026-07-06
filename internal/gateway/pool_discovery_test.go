package gateway

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func mkSvc(ns, name string, labels map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	if len(ports) == 0 {
		ports = []corev1.ServicePort{{Name: "web", Port: 9090}}
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec:       corev1.ServiceSpec{Ports: ports},
	}
}

func TestDiscoverPrometheus(t *testing.T) {
	promLabel := map[string]string{"app.kubernetes.io/name": "prometheus"}
	nss := []string{"monitoring", "kube-prometheus-stack", "observability", "prometheus"}

	tests := []struct {
		name      string
		objs      []runtime.Object
		wantEP    string
		detailSub string // substring the detail must contain (skipped when empty)
	}{
		{
			name:   "operated service found by well-known name",
			objs:   []runtime.Object{mkSvc("monitoring", "prometheus-operated", nil)},
			wantEP: "http://prometheus-operated.monitoring.svc:9090",
		},
		{
			name:   "labeled ClusterIP service found by label",
			objs:   []runtime.Object{mkSvc("monitoring", "kps-kube-prometheus-prometheus", promLabel)},
			wantEP: "http://kps-kube-prometheus-prometheus.monitoring.svc:9090",
		},
		{
			name:   "nothing found -> empty, no error",
			objs:   nil,
			wantEP: "",
		},
		{
			name: "namespace order wins (earlier ns beats later)",
			objs: []runtime.Object{
				mkSvc("prometheus", "prometheus-operated", nil),
				mkSvc("monitoring", "prometheus-operated", nil),
			},
			wantEP: "http://prometheus-operated.monitoring.svc:9090",
		},
		{
			name: "multiple in a namespace -> lexicographic pick, reports the other",
			objs: []runtime.Object{
				mkSvc("monitoring", "z-prometheus", promLabel),
				mkSvc("monitoring", "a-prometheus", promLabel),
			},
			wantEP:    "http://a-prometheus.monitoring.svc:9090",
			detailSub: "also saw z-prometheus.monitoring",
		},
		{
			name:   "web port honored on a non-9090 port",
			objs:   []runtime.Object{mkSvc("monitoring", "prometheus-operated", nil, corev1.ServicePort{Name: "web", Port: 9091})},
			wantEP: "http://prometheus-operated.monitoring.svc:9091",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tt.objs...)
			ep, detail, err := discoverPrometheus(context.Background(), cs, nss)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ep != tt.wantEP {
				t.Errorf("endpoint: got %q want %q", ep, tt.wantEP)
			}
			if tt.detailSub != "" && !strings.Contains(detail, tt.detailSub) {
				t.Errorf("detail %q missing %q", detail, tt.detailSub)
			}
		})
	}
}

func TestPrometheusServicePort(t *testing.T) {
	cases := []struct {
		name  string
		ports []corev1.ServicePort
		want  int32
	}{
		{"web named", []corev1.ServicePort{{Name: "web", Port: 9090}}, 9090},
		{"http named on odd port", []corev1.ServicePort{{Name: "http", Port: 9091}}, 9091},
		{"unnamed 9090 present", []corev1.ServicePort{{Name: "metrics", Port: 8080}, {Name: "x", Port: 9090}}, 9090},
		{"no match -> default 9090", []corev1.ServicePort{{Name: "metrics", Port: 8080}}, 9090},
		{"empty -> default 9090", nil, 9090},
	}
	for _, c := range cases {
		got := prometheusServicePort(&corev1.Service{Spec: corev1.ServiceSpec{Ports: c.ports}})
		if got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

// resolvePrometheus precedence: an explicit spec endpoint always wins (no
// discovery attempted); an unreachable member or discovery-disabled pool resolve
// to NotFound. These paths never touch cfg, so a nil cfg is safe.
func TestResolvePrometheus_Precedence(t *testing.T) {
	pEnabled := &ClientPool{discoveryNamespaces: []string{"monitoring"}}
	if ep, reason, _ := pEnabled.resolvePrometheus(context.Background(), nil, "http://explicit:9090", true); ep != "http://explicit:9090" || reason != prometheusReasonExplicit {
		t.Fatalf("explicit must win: ep=%q reason=%q", ep, reason)
	}
	if ep, reason, _ := pEnabled.resolvePrometheus(context.Background(), nil, "", false); ep != "" || reason != prometheusReasonNotFound {
		t.Fatalf("unreachable must be NotFound (no discovery): ep=%q reason=%q", ep, reason)
	}
	pDisabled := &ClientPool{discoveryNamespaces: nil}
	if ep, reason, _ := pDisabled.resolvePrometheus(context.Background(), nil, "", true); ep != "" || reason != prometheusReasonNotFound {
		t.Fatalf("discovery-disabled must be NotFound: ep=%q reason=%q", ep, reason)
	}
}

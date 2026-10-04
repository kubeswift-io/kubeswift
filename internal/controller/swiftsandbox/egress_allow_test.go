package swiftsandbox

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func clusterIPService(ns, name, ip string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.ServiceSpec{ClusterIP: ip, ClusterIPs: []string{ip}, Ports: ports},
	}
}

func tcpPort(p int32) corev1.ServicePort {
	return corev1.ServicePort{Port: p, Protocol: corev1.ProtocolTCP}
}

func allowSvc(name, ns string, ports ...int32) sandboxv1alpha1.SandboxEgressRule {
	r := sandboxv1alpha1.SandboxEgressRule{Service: &sandboxv1alpha1.SandboxEgressService{Name: name, Namespace: ns}}
	for _, p := range ports {
		r.Ports = append(r.Ports, sandboxv1alpha1.SandboxEgressPort{Port: p})
	}
	return r
}

func egressNet(rules ...sandboxv1alpha1.SandboxEgressRule) sandboxv1alpha1.SandboxNetwork {
	return sandboxv1alpha1.SandboxNetwork{Egress: &sandboxv1alpha1.SandboxEgress{Allow: rules}}
}

func TestResolveEgress(t *testing.T) {
	ctx := context.Background()
	dual := clusterIPService("inference", "llm", "10.96.0.12", tcpPort(8000), tcpPort(9000),
		corev1.ServicePort{Port: 53, Protocol: corev1.ProtocolUDP}, corev1.ServicePort{Port: 7, Protocol: corev1.ProtocolSCTP})
	dual.Spec.ClusterIPs = []string{"10.96.0.12", "fd00::12"}
	headless := clusterIPService("default", "db", corev1.ClusterIPNone, tcpPort(5432))
	local := clusterIPService("default", "otel", "10.96.0.40", tcpPort(4317))
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(dual, headless, local).Build()

	t.Run("a named port of a Service in another namespace", func(t *testing.T) {
		got, problem, err := resolveEgress(ctx, c, "default", egressNet(allowSvc("llm", "inference", 8000)))
		if err != nil || problem != nil {
			t.Fatalf("err=%v problem=%+v", err, problem)
		}
		want := []sandboxv1alpha1.SandboxEgressAllowed{{CIDR: "10.96.0.12/32", Protocol: corev1.ProtocolTCP, Port: 8000, From: "service inference/llm"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("no ports: every TCP and UDP port the Service declares, IPv4 only", func(t *testing.T) {
		got, problem, _ := resolveEgress(ctx, c, "default", egressNet(allowSvc("llm", "inference")))
		if problem != nil || len(got) != 3 {
			t.Fatalf("want 8000/tcp, 9000/tcp, 53/udp on the IPv4 address; got %+v (problem %+v)", got, problem)
		}
		for _, a := range got {
			if a.CIDR != "10.96.0.12/32" || a.Protocol == corev1.ProtocolSCTP {
				t.Errorf("unexpected rule %+v", a)
			}
		}
	})
	t.Run("the namespace defaults to the sandbox's", func(t *testing.T) {
		got, problem, _ := resolveEgress(ctx, c, "default", egressNet(allowSvc("otel", "")))
		if problem != nil || len(got) != 1 || got[0].From != "service default/otel" {
			t.Errorf("got %+v (problem %+v)", got, problem)
		}
	})
	t.Run("a cidr, canonicalised, one rule per port", func(t *testing.T) {
		got, _, _ := resolveEgress(ctx, c, "default", egressNet(sandboxv1alpha1.SandboxEgressRule{
			CIDR: "10.20.0.7/24", Ports: []sandboxv1alpha1.SandboxEgressPort{{Port: 5432}, {Port: 53, Protocol: corev1.ProtocolUDP}}}))
		if len(got) != 2 || got[0].CIDR != "10.20.0.0/24" || got[1].Protocol != corev1.ProtocolUDP || got[0].From != "cidr 10.20.0.0/24" {
			t.Errorf("got %+v", got)
		}
		if env := egressAllowEnv(got); env != "tcp,10.20.0.0/24,5432 udp,10.20.0.0/24,53" {
			t.Errorf("env = %q", env)
		}
		all, _, _ := resolveEgress(ctx, c, "default", egressNet(sandboxv1alpha1.SandboxEgressRule{CIDR: "10.30.0.0/16"}))
		if env := egressAllowEnv(all); env != "any,10.30.0.0/16,0" {
			t.Errorf("a cidr with no ports renders %q, want any,10.30.0.0/16,0", env)
		}
	})

	for _, tc := range []struct {
		name, reason string
		invalid      bool
		n            sandboxv1alpha1.SandboxNetwork
	}{
		{"a missing Service", "EgressServiceNotFound", false, egressNet(allowSvc("nope", "inference"))},
		{"a headless Service", "EgressServiceNoClusterIP", false, egressNet(allowSvc("db", ""))},
		{"a port the Service does not declare", "EgressServicePortNotFound", false, egressNet(allowSvc("llm", "inference", 8080))},
		{"the metadata address", "InvalidEgress", true, egressNet(sandboxv1alpha1.SandboxEgressRule{CIDR: "169.254.169.254/32"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, problem, err := resolveEgress(ctx, c, "default", tc.n)
			if err != nil || problem == nil || problem.Reason != tc.reason || problem.Invalid != tc.invalid || got != nil {
				t.Errorf("got %+v, problem %+v, err %v; want reason %s (invalid=%v)", got, problem, err, tc.reason, tc.invalid)
			}
		})
	}
}

func egressSandbox(t *testing.T, n sandboxv1alpha1.SandboxNetwork) *sandboxv1alpha1.SwiftSandbox {
	sb := plainSandbox(testImage(t))
	sb.Spec.Network = n
	return sb
}

func networkInitEnv(pod *corev1.Pod, name string) string {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == "network-init" {
			for _, e := range c.Env {
				if e.Name == name {
					return e.Value
				}
			}
		}
	}
	return ""
}

// A sandbox whose allowlist names a Service that does not exist yet waits,
// says which, and starts no launcher; once the Service exists it launches
// with the rule rendered for network-init and recorded in status.
func TestReconcile_EgressServiceWaitsThenLaunchesWithTheRule(t *testing.T) {
	ctx := context.Background()
	sb := egressSandbox(t, egressNet(allowSvc("llm", "inference", 8000)))
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))

	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPending || cond == nil || cond.Reason != "EgressServiceNotFound" ||
		!strings.Contains(cond.Message, "inference/llm") {
		t.Fatalf("want Pending with EgressServiceNotFound naming inference/llm, got phase %s cond %+v", got.Status.Phase, cond)
	}
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err == nil {
		t.Fatal("no launcher pod may exist while the allowlist cannot be resolved")
	}

	if err := c.Create(ctx, clusterIPService("inference", "llm", "10.96.0.12", tcpPort(8000))); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "sb")
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatalf("launcher pod: %v", err)
	}
	if env := networkInitEnv(&pod, "KUBESWIFT_SANDBOX_EGRESS_ALLOW"); env != "tcp,10.96.0.12/32,8000" {
		t.Errorf("network-init KUBESWIFT_SANDBOX_EGRESS_ALLOW = %q", env)
	}
	want := []sandboxv1alpha1.SandboxEgressAllowed{{CIDR: "10.96.0.12/32", Protocol: corev1.ProtocolTCP, Port: 8000, From: "service inference/llm"}}
	if got := parseEgressAllowed(pod.Annotations[EgressAllowedAnnotation]); !reflect.DeepEqual(got, want) {
		t.Errorf("pod annotation = %+v", got)
	}
	got = getSandbox(t, c, "sb")
	if got.Status.Network == nil || !reflect.DeepEqual(got.Status.Network.EgressAllowed, want) {
		t.Errorf("status.network.egressAllowed = %+v, want %+v", got.Status.Network, want)
	}
}

// With the webhook off (the default), a rule that would open the metadata
// endpoint must still never reach a launcher: the sandbox fails first.
func TestReconcile_InvalidEgressFailsBeforeLaunch(t *testing.T) {
	sb := egressSandbox(t, egressNet(sandboxv1alpha1.SandboxEgressRule{CIDR: "169.254.169.254/32"}))
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || !strings.Contains(got.Status.Message, "never allowed") {
		t.Errorf("want Failed naming the metadata range, got %s: %q", got.Status.Phase, got.Status.Message)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err == nil {
		t.Error("an invalid allowlist must never produce a launcher pod")
	}
}

// No allowlist, no env and no annotation: the restricted rules are as before.
func TestBuildPod_NoEgressAllowRendersNothing(t *testing.T) {
	pod := buildPod(egSandbox(""), "sandbox")
	if v := networkInitEnv(pod, "KUBESWIFT_SANDBOX_EGRESS_ALLOW"); v != "" {
		t.Errorf("KUBESWIFT_SANDBOX_EGRESS_ALLOW = %q, want unset", v)
	}
	if _, ok := pod.Annotations[EgressAllowedAnnotation]; ok {
		t.Error("no allowlist must leave no annotation")
	}
}

func egressPool(image string) *sandboxv1alpha1.SwiftSandboxPool {
	return &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{
			Image: image, MinWarm: 1, CPU: 1, Memory: resource.MustParse("512Mi"),
			Network: egressNet(allowSvc("llm", "inference", 8000)),
		},
	}
}

// A pool resolves its allowlist every pass: slots carry the rule, a slot
// holding an address the Service no longer has is replaced, and while the
// Service is missing no slot carrying rules is kept or warmed.
func TestPool_SlotsFollowTheServiceAddress(t *testing.T) {
	ctx := context.Background()
	llm := clusterIPService("inference", "llm", "10.96.0.12", tcpPort(8000))
	r, c := poolReconciler(egressPool(testImage(t)), llm, readyKernel("default", defaultKernelProfile))

	reconcilePool(t, r, "p")
	slots := poolSlotPods(t, c, "p")
	if len(slots) != 1 || networkInitEnv(&slots[0], "KUBESWIFT_SANDBOX_EGRESS_ALLOW") != "tcp,10.96.0.12/32,8000" {
		t.Fatalf("want one slot allowing 10.96.0.12:8000, got %d slots (env %q)", len(slots), networkInitEnv(&slots[0], "KUBESWIFT_SANDBOX_EGRESS_ALLOW"))
	}
	first := slots[0].Name

	// The Service is recreated with another ClusterIP.
	llm.Spec.ClusterIP, llm.Spec.ClusterIPs = "10.96.0.99", []string{"10.96.0.99"}
	if err := c.Update(ctx, llm); err != nil {
		t.Fatal(err)
	}
	reconcilePool(t, r, "p")
	slots = poolSlotPods(t, c, "p")
	if len(slots) != 1 || slots[0].Name == first || networkInitEnv(&slots[0], "KUBESWIFT_SANDBOX_EGRESS_ALLOW") != "tcp,10.96.0.99/32,8000" {
		t.Fatalf("want the slot replaced by one allowing 10.96.0.99, got %+v", slotNames(slots))
	}

	// The Service goes away: the slot is replaced by nothing, and the pool says why.
	if err := c.Delete(ctx, llm); err != nil {
		t.Fatal(err)
	}
	reconcilePool(t, r, "p")
	if slots = poolSlotPods(t, c, "p"); len(slots) != 0 {
		t.Errorf("no slot may keep allowing an address its Service no longer has, got %v", slotNames(slots))
	}
	var pool sandboxv1alpha1.SwiftSandboxPool
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "p"}, &pool); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pool.Status.Message, "inference/llm") {
		t.Errorf("pool message %q should name the missing Service", pool.Status.Message)
	}
}

func slotNames(pods []corev1.Pod) []string {
	var out []string
	for _, p := range pods {
		out = append(out, p.Name)
	}
	return out
}

// A checkout reports the allowlist its slot enforces.
func TestCheckout_ReportsTheSlotsEgressAllowlist(t *testing.T) {
	pool := shapedPool()
	pool.Namespace = "default"
	pool.Spec.Network = egressNet(sandboxv1alpha1.SandboxEgressRule{CIDR: "10.20.0.0/24"})
	allowed := []sandboxv1alpha1.SandboxEgressAllowed{{CIDR: "10.20.0.0/24", From: "cidr 10.20.0.0/24"}}
	slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
	slot.Namespace = "default"
	slot.Annotations[EgressAllowedAnnotation] = egressAllowedJSON(allowed)
	sb := matchingSandbox()
	sb.Namespace = "default"
	sb.Spec.Network = egressNet(sandboxv1alpha1.SandboxEgressRule{CIDR: "10.20.0.0/24"})
	r, c := sandboxReconciler(pool, slot, sb)

	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.PodRef != slot.Name {
		t.Fatalf("want a checkout of %s, got podRef %q (%s)", slot.Name, got.Status.PodRef, got.Status.Message)
	}
	if got.Status.Network == nil || !reflect.DeepEqual(got.Status.Network.EgressAllowed, allowed) {
		t.Errorf("status.network.egressAllowed = %+v, want %+v", got.Status.Network, allowed)
	}
}

// The allowlist is part of the slot shape, compared order-free with the
// namespace and protocol defaults applied.
func TestSlotMismatches_EgressAllowlist(t *testing.T) {
	pool := shapedPool()
	pool.Spec.Network = egressNet(allowSvc("llm", "", 8000), sandboxv1alpha1.SandboxEgressRule{CIDR: "10.20.0.7/24"})

	same := matchingSandbox()
	same.Spec.Network = egressNet(sandboxv1alpha1.SandboxEgressRule{CIDR: "10.20.0.0/24"}, sandboxv1alpha1.SandboxEgressRule{
		Service: &sandboxv1alpha1.SandboxEgressService{Name: "llm", Namespace: "ns"},
		Ports:   []sandboxv1alpha1.SandboxEgressPort{{Port: 8000, Protocol: corev1.ProtocolTCP}},
	})
	if got := slotMismatches(pool, same); len(got) != 0 {
		t.Errorf("the same allowlist written differently must match, got %q", got)
	}

	none := matchingSandbox()
	got := slotMismatches(pool, none)
	if len(got) != 1 || !strings.HasPrefix(got[0], "network egress (pool cidr 10.20.0.0/24 all ports; service ns/llm tcp/8000, sandbox none)") {
		t.Errorf("a sandbox without the pool's allowlist must not take its slot, got %q", got)
	}
}

// The guest IP report rewrites status.network; it must keep the allowlist.
func TestApplyGuestAnnotations_KeepsTheEgressAllowlist(t *testing.T) {
	sb := egSandbox("")
	allowed := []sandboxv1alpha1.SandboxEgressAllowed{{CIDR: "10.0.0.1/32", From: "cidr 10.0.0.1/32"}}
	setEgressAllowed(sb, allowed)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"kubeswift.io/guest-ip": "192.168.99.10"}}}
	applyGuestAnnotations(sb, pod)
	if sb.Status.Network == nil || !reflect.DeepEqual(sb.Status.Network.EgressAllowed, allowed) {
		t.Errorf("status.network after the IP report = %+v", sb.Status.Network)
	}
}

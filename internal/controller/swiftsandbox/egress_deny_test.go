package swiftsandbox

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func TestParseEgressDeny(t *testing.T) {
	got, err := ParseEgressDeny("203.0.113.9, 198.51.100.0/24\n10.1.2.3/8")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"203.0.113.9/32", "198.51.100.0/24", "10.0.0.0/8"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, err := ParseEgressDeny(""); err != nil || got != nil {
		t.Errorf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{"fd00::1", "2001:db8::/32", "host.example", "10.0.0.1/33", "10.0.0.1 -j ACCEPT"} {
		if _, err := ParseEgressDeny(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func addrNode(name string, addrs ...corev1.NodeAddress) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Addresses: addrs}}
}

func TestComputeEgressDeny(t *testing.T) {
	nodes := []corev1.Node{
		addrNode("a",
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "203.0.113.10"},
			corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "198.51.100.10"},
			corev1.NodeAddress{Type: corev1.NodeHostName, Address: "a.example"},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "fd00::10"}),
		addrNode("b", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "203.0.113.11"}),
	}
	// A control plane that is not a Node.
	api := &discoveryv1.EndpointSlice{Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"203.0.113.2"}}}}
	got := computeEgressDeny(nodes, api, []string{"192.0.2.0/24", "203.0.113.10/32"})
	want := "192.0.2.0/24 198.51.100.10/32 203.0.113.10/32 203.0.113.11/32 203.0.113.2/32"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := computeEgressDeny(nil, nil, nil); got != "" {
		t.Errorf("nothing: %q", got)
	}
}

// A restricted launcher pod carries the list and hands it to network-init and
// the launcher as a file; an open or unnetworked one does not.
func TestBuildPod_EgressDeny(t *testing.T) {
	v := "203.0.113.10/32"
	currentEgressDeny.Store(&v)
	t.Cleanup(func() { currentEgressDeny.Store(nil) })

	pod := buildPod(egSandbox(""), "sandbox")
	if got, ok := pod.Annotations[EgressDenyAnnotation]; !ok || got != v {
		t.Fatalf("annotation = %q, %v", got, ok)
	}
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == egressDenyVolume {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.DownwardAPI == nil || vol.DownwardAPI.Items[0].Path != EgressDenyFile ||
		vol.DownwardAPI.Items[0].FieldRef.FieldPath != "metadata.annotations['"+EgressDenyAnnotation+"']" {
		t.Fatalf("volume = %+v", vol)
	}
	mounted := func(cs []corev1.Container, name string) bool {
		for _, c := range cs {
			if c.Name != name {
				continue
			}
			for _, m := range c.VolumeMounts {
				if m.Name == egressDenyVolume && m.MountPath == EgressDenyDir && m.ReadOnly {
					return true
				}
			}
		}
		return false
	}
	if !mounted(pod.Spec.InitContainers, networkInitName) || !mounted(pod.Spec.Containers, launcherName) {
		t.Error("the list is not mounted into network-init and the launcher")
	}

	for _, mode := range []sandboxv1alpha1.SandboxNetworkMode{sandboxv1alpha1.SandboxNetworkOpen, sandboxv1alpha1.SandboxNetworkNone} {
		p := buildPod(egSandbox(mode), "sandbox")
		if _, ok := p.Annotations[EgressDenyAnnotation]; ok {
			t.Errorf("%s: carries the deny annotation", mode)
		}
	}

	// A tenant cannot replace the list through spec.podMetadata.
	sb := egSandbox("")
	sb.Spec.PodMetadata = &sandboxv1alpha1.SandboxPodMetadata{Annotations: map[string]string{EgressDenyAnnotation: ""}}
	if got := buildPod(sb, "sandbox").Annotations[EgressDenyAnnotation]; got != v {
		t.Errorf("podMetadata replaced the list: %q", got)
	}
}

// Before the first compute a pod gets the admin-set part alone.
func TestEgressDenyValue_BeforeFirstCompute(t *testing.T) {
	currentEgressDeny.Store(nil)
	old := ExtraEgressDeny
	ExtraEgressDeny = []string{"192.0.2.0/24"}
	t.Cleanup(func() { ExtraEgressDeny = old })
	if got := egressDenyValue(); got != "192.0.2.0/24" {
		t.Errorf("got %q", got)
	}
}

func launcherPod(name, deny string, hasDeny bool, phase corev1.PodPhase) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{SandboxLabelKey: name}},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if hasDeny {
		p.Annotations = map[string]string{EgressDenyAnnotation: deny}
	}
	return p
}

// The refresher recomputes the list and patches only running restricted
// launcher pods whose copy is stale.
func TestEgressDenyRefresher(t *testing.T) {
	t.Cleanup(func() { currentEgressDeny.Store(nil) })
	n := addrNode("a", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "203.0.113.10"})
	api := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"203.0.113.2"}}},
	}
	want := "203.0.113.10/32 203.0.113.2/32"
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		&n, api,
		launcherPod("stale", "203.0.113.2/32", true, corev1.PodRunning),
		launcherPod("current", want, true, corev1.PodRunning),
		launcherPod("open", "", false, corev1.PodRunning),
		launcherPod("done", "", true, corev1.PodSucceeded),
	).Build()
	r := &EgressDenyRefresher{Client: c, APIReader: c}
	if err := r.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := egressDenyValue(); got != want {
		t.Errorf("current list = %q, want %q", got, want)
	}
	get := func(name string) *corev1.Pod {
		p := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := get("stale").Annotations[EgressDenyAnnotation]; got != want {
		t.Errorf("stale pod not patched: %q", got)
	}
	if _, ok := get("open").Annotations[EgressDenyAnnotation]; ok {
		t.Error("an open sandbox's pod got the list")
	}
	if got := get("done").Annotations[EgressDenyAnnotation]; got != "" {
		t.Errorf("a finished pod was patched: %q", got)
	}
}

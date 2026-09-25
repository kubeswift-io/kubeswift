package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// natLauncher is a running launcher with pod IP podIP, whose swiftletd has
// reported guestIP as the guest's address (none when guestIP is empty).
func natLauncher(uid types.UID, podIP, guestIP string) *corev1.Pod {
	p := runningLauncher("worker-1")
	p.UID = uid
	p.Status.PodIP = podIP
	if guestIP != "" {
		p.Annotations = map[string]string{PodAnnotationGuestIP: guestIP}
	}
	return p
}

// primaryOnNAD makes g's primary interface ride a multi-node NAD.
func primaryOnNAD(g *swiftv1alpha1.SwiftGuest) *swiftv1alpha1.SwiftGuest {
	g.Spec.Network = &swiftv1alpha1.GuestNetworkSpec{Binding: "bridge"}
	g.Spec.Interfaces = []swiftv1alpha1.GuestInterface{{
		Name: "l2", Primary: true, NetworkRef: &swiftv1alpha1.NetworkReference{Name: "l2-net"},
	}}
	return g
}

// Every nat launcher hands out the same private range, so two guests report
// the same primaryIP. The scope says so, and the pod IP tells them apart.
func TestMapPodToStatus_NatGuestsSharePrimaryIPButNotPodIP(t *testing.T) {
	a, b := &swiftv1alpha1.SwiftGuestStatus{}, &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(kernelGuest(), natLauncher("pod-a", "10.244.1.7", "192.168.99.12"), a)
	MapPodToStatus(kernelGuest(), natLauncher("pod-b", "10.244.2.9", "192.168.99.12"), b)

	for _, tc := range []struct {
		st        *swiftv1alpha1.SwiftGuestStatus
		wantPodIP string
	}{{a, "10.244.1.7"}, {b, "10.244.2.9"}} {
		n := tc.st.Network
		if n == nil || n.PrimaryIP != "192.168.99.12" {
			t.Fatalf("network = %+v, want primaryIP 192.168.99.12", n)
		}
		if n.PrimaryIPScope != swiftv1alpha1.PrimaryIPScopePod {
			t.Errorf("primaryIPScope = %q, want Pod: the lease is on the launcher's private bridge", n.PrimaryIPScope)
		}
		if n.PodIP != tc.wantPodIP {
			t.Errorf("podIP = %q, want %q (the launcher's own address)", n.PodIP, tc.wantPodIP)
		}
	}
}

// A primary on a multi-node NAD takes its address from the NAD's IPAM: it is
// reachable on that network, not only inside the pod.
func TestMapPodToStatus_PrimaryOnANADIsNetworkScope(t *testing.T) {
	st := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(primaryOnNAD(kernelGuest()), natLauncher("pod-1", "10.244.1.7", "10.60.0.21"), st)
	if st.Network == nil || st.Network.PrimaryIPScope != swiftv1alpha1.PrimaryIPScopeNetwork {
		t.Fatalf("network = %+v, want primaryIPScope Network", st.Network)
	}
	if st.Network.PodIP != "10.244.1.7" {
		t.Errorf("podIP = %q, want the launcher's own address", st.Network.PodIP)
	}
}

// A bridge binding alone does not move the primary off the in-pod bridge; the
// primary interface decides the datapath.
func TestMapPodToStatus_BridgeBindingWithoutANADPrimaryIsPodScope(t *testing.T) {
	g := kernelGuest()
	g.Spec.Network = &swiftv1alpha1.GuestNetworkSpec{Binding: "bridge"}
	st := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(g, natLauncher("pod-1", "10.244.1.7", "192.168.99.12"), st)
	if st.Network == nil || st.Network.PrimaryIPScope != swiftv1alpha1.PrimaryIPScopePod {
		t.Fatalf("network = %+v, want primaryIPScope Pod", st.Network)
	}
}

// A primary-UDN guest's address is the pod's UDN address, reachable on the
// UDN. It is also scoped when primaryIP was mapped by an earlier pass (the
// derivation is skipped then), as for a status written before the field
// existed.
func TestMapPodToStatus_PrimaryUDNIsNetworkScope(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "g", Namespace: "model-a", UID: "pod-1",
			Annotations: map[string]string{
				PodAnnotationPrimaryUDNIface: "ovn-udn1",
				OVNPodNetworksAnnotation:     ovnPodNetworksFixture,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.1.36"},
	}
	for name, st := range map[string]*swiftv1alpha1.SwiftGuestStatus{
		"first pass": {},
		"already mapped": {
			PodRef:  &corev1.ObjectReference{Name: "g", UID: "pod-1"},
			Network: &swiftv1alpha1.GuestNetworkStatus{PrimaryIP: "10.50.0.26", Ready: true},
		},
	} {
		MapPodToStatus(kernelGuest(), pod, st)
		if st.Network == nil || st.Network.PrimaryIP != "10.50.0.26" {
			t.Fatalf("%s: network = %+v, want primaryIP 10.50.0.26", name, st.Network)
		}
		if st.Network.PrimaryIPScope != swiftv1alpha1.PrimaryIPScopeNetwork {
			t.Errorf("%s: primaryIPScope = %q, want Network", name, st.Network.PrimaryIPScope)
		}
	}
}

// No primaryIP, no scope: a launcher without a lease yet still reports its own
// address, which is where the guest's ports will be.
func TestMapPodToStatus_NoPrimaryIPNoScope(t *testing.T) {
	pod := natLauncher("pod-1", "10.244.1.7", "")
	st := &swiftv1alpha1.SwiftGuestStatus{}
	MapPodToStatus(kernelGuest(), pod, st)
	if st.Network == nil || st.Network.PodIP != "10.244.1.7" {
		t.Fatalf("network = %+v, want podIP 10.244.1.7", st.Network)
	}
	if st.Network.PrimaryIPScope != "" {
		t.Errorf("primaryIPScope = %q with no primaryIP", st.Network.PrimaryIPScope)
	}
}

func TestClearRunState_DropsPodIPAndScope(t *testing.T) {
	st := ranStatus("pod-1")
	ClearRunState(st, "Stopped", "stopped")
	if st.Network.PodIP != "" || st.Network.PrimaryIPScope != "" {
		t.Errorf("podIP = %q, primaryIPScope = %q; both went with the launcher", st.Network.PodIP, st.Network.PrimaryIPScope)
	}
}

// A new launcher (a restart, or an offline migration's recreated pod) has a
// new IP; the old one must not linger, not even while the new pod has none.
func TestMapPodToStatus_ANewLauncherReplacesThePodIP(t *testing.T) {
	st := ranStatus("pod-1")
	MapPodToStatus(kernelGuest(), natLauncher("pod-2", "10.244.3.4", "192.168.99.13"), st)
	if st.Network.PodIP != "10.244.3.4" {
		t.Errorf("podIP = %q, want the new launcher's 10.244.3.4", st.Network.PodIP)
	}
	if st.Network.PrimaryIP != "192.168.99.13" || st.Network.PrimaryIPScope != swiftv1alpha1.PrimaryIPScopePod {
		t.Errorf("primaryIP = %q scope %q, want the new lease, Pod", st.Network.PrimaryIP, st.Network.PrimaryIPScope)
	}

	// The new launcher not running yet, and no pod IP assigned.
	st = ranStatus("pod-1")
	starting := natLauncher("pod-2", "", "")
	starting.Status.Phase = corev1.PodPending
	MapPodToStatus(kernelGuest(), starting, st)
	if st.Network.PodIP != "" {
		t.Errorf("podIP = %q; that was the previous launcher's", st.Network.PodIP)
	}
}

// A source launcher that handed its VM to a live migration describes nothing
// about the guest any more, its IP included.
func TestMapPodToStatus_AHandedOffLauncherKeepsPodIPAndScope(t *testing.T) {
	st := ranStatus("pod-1")
	src := handedOffLauncher()
	src.Status.PodIP = "10.244.9.9"
	MapPodToStatus(kernelGuest(), src, st)
	if st.Network.PodIP != "10.244.1.7" || st.Network.PrimaryIPScope != swiftv1alpha1.PrimaryIPScopePod {
		t.Errorf("podIP = %q scope %q; a handed-off launcher must change neither", st.Network.PodIP, st.Network.PrimaryIPScope)
	}
}

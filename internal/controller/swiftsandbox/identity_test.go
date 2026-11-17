package swiftsandbox

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	egressintentv1 "github.com/kubeswift-io/kubeswift/api/egressintent/v1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

func withSPIFFE(t *testing.T, c *sandboxwebhook.SPIFFEConfig) {
	t.Helper()
	saved := sandboxwebhook.SPIFFE
	sandboxwebhook.SPIFFE = c
	t.Cleanup(func() { sandboxwebhook.SPIFFE = saved })
}

var csiSPIFFE = &sandboxwebhook.SPIFFEConfig{TrustDomain: "kubeswift.test", CSIDriver: "csi.spiffe.io", SocketName: "spire-agent.sock"}

func spiffeSandbox() *sandboxv1alpha1.SwiftSandbox {
	sb := proxySandbox(sandboxv1alpha1.SandboxEgressProxyConfigIntent)
	sb.Spec.Identity = &sandboxv1alpha1.SandboxIdentity{SPIFFE: sandboxv1alpha1.SandboxIdentitySPIFFEEnabled}
	return sb
}

func proxyContainer(pod *corev1.Pod) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == EgressProxyContainerName {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

// The proxy, and only the proxy, gets the Workload API socket; the launcher
// is labeled for the chart's ClusterSPIFFEIDs.
func TestBuildPod_SPIFFE(t *testing.T) {
	withSPIFFE(t, csiSPIFFE)
	sb := spiffeSandbox()
	pod := buildPod(sb, "sandbox")
	if pod.Labels[SPIFFELabelKey] != "enabled" || pod.Labels[PoolRefLabelKey] != "" {
		t.Errorf("labels %v", pod.Labels)
	}
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == spiffeVolume {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.CSI == nil || vol.CSI.Driver != "csi.spiffe.io" || !*vol.CSI.ReadOnly {
		t.Fatalf("volume %+v", vol)
	}
	c := proxyContainer(pod)
	if c == nil || containerEnv([]corev1.Container{*c}, EgressProxyContainerName)["SPIFFE_ENDPOINT_SOCKET"] != "unix:///run/spiffe/spire-agent.sock" {
		t.Fatalf("proxy env %+v", c)
	}
	mounted := func(c corev1.Container) bool {
		for _, m := range c.VolumeMounts {
			if m.Name == spiffeVolume {
				return m.ReadOnly && m.MountPath == spiffeMountDir
			}
		}
		return false
	}
	if !mounted(*c) {
		t.Error("proxy has no read-only socket mount")
	}
	for _, other := range append(append([]corev1.Container(nil), pod.Spec.Containers...), pod.Spec.InitContainers...) {
		if other.Name != EgressProxyContainerName && mounted(other) {
			t.Errorf("%s has the Workload API socket", other.Name)
		}
	}

	// A cold launcher of a pooled sandbox names the pool in pool-ref, not in
	// the slot-membership label.
	sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: "agents"}
	pod = buildPod(sb, "sandbox")
	if pod.Labels[PoolRefLabelKey] != "agents" || pod.Labels[PoolLabelKey] != "" {
		t.Errorf("pooled cold labels %v", pod.Labels)
	}

	// hostPath instead of the CSI driver.
	withSPIFFE(t, &sandboxwebhook.SPIFFEConfig{TrustDomain: "kubeswift.test", HostPath: "/run/spire/agent-sockets", SocketName: "agent.sock"})
	pod = buildPod(spiffeSandbox(), "sandbox")
	for _, v := range pod.Spec.Volumes {
		if v.Name == spiffeVolume && (v.HostPath == nil || v.HostPath.Path != "/run/spire/agent-sockets") {
			t.Errorf("hostPath volume %+v", v)
		}
	}
}

// Nothing changes without the identity, without a proxy, or with SPIFFE off.
func TestBuildPod_NoSPIFFE(t *testing.T) {
	withSPIFFE(t, csiSPIFFE)
	off := proxySandbox(sandboxv1alpha1.SandboxEgressProxyConfigSelf)
	noProxy := proxiedSandbox()
	noProxy.Spec.Identity = &sandboxv1alpha1.SandboxIdentity{SPIFFE: sandboxv1alpha1.SandboxIdentitySPIFFEEnabled}
	for name, sb := range map[string]*sandboxv1alpha1.SwiftSandbox{"identity unset": off, "no proxy": noProxy} {
		pod := buildPod(sb, "sandbox")
		if _, ok := pod.Labels[SPIFFELabelKey]; ok {
			t.Errorf("%s: labeled", name)
		}
		for _, v := range pod.Spec.Volumes {
			if v.Name == spiffeVolume {
				t.Errorf("%s: socket volume", name)
			}
		}
	}
	withSPIFFE(t, nil)
	if _, ok := buildPod(spiffeSandbox(), "sandbox").Labels[SPIFFELabelKey]; ok {
		t.Error("labeled with SPIFFE off on the platform")
	}
}

// The intent names the ID the launcher is registered with: the slot's for a
// warm slot, the sandbox's (with its poolRef) for a cold launcher.
func TestEgressIntent_SPIFFE(t *testing.T) {
	withSPIFFE(t, csiSPIFFE)
	id := func(js string) *egressintentv1.SPIFFE {
		var in egressintentv1.EgressIntent
		if err := json.Unmarshal([]byte(js), &in); err != nil {
			t.Fatal(err)
		}
		if in.Identity == nil {
			return nil
		}
		return in.Identity.SPIFFE
	}
	sb := spiffeSandbox()
	js, _ := egressIntentJSON(sb, "SwiftSandbox", sb.Name)
	if s := id(js); s == nil || s.ID != "spiffe://kubeswift.test/ns/default/sandbox/"+sb.Name || s.WorkloadAPISocket != "unix:///run/spiffe/spire-agent.sock" {
		t.Errorf("cold %+v (%s)", s, sb.Namespace)
	}
	sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: "agents"}
	js, _ = egressIntentJSON(sb, "SwiftSandbox", sb.Name)
	if s := id(js); s == nil || !strings.HasSuffix(s.ID, "/pool/agents/sandbox/"+sb.Name) {
		t.Errorf("pooled cold %+v", s)
	}
	slot := spiffeSandbox()
	slot.Name = "agents-x7k2p"
	js, _ = egressIntentJSON(slot, "SwiftSandboxPool", "agents")
	if s := id(js); s == nil || s.ID != "spiffe://kubeswift.test/ns/"+slot.Namespace+"/pool/agents/slot/agents-x7k2p" {
		t.Errorf("slot %+v", s)
	}
	js, _ = egressIntentJSON(proxySandbox(sandboxv1alpha1.SandboxEgressProxyConfigIntent), "SwiftSandbox", "x")
	if id(js) != nil {
		t.Error("identity in the intent without spec.identity")
	}
}

// status.identity names the slot's ID after a checkout, the sandbox's for a
// cold launcher, and nothing for a launcher that is not registered.
func TestSetIdentityStatus(t *testing.T) {
	withSPIFFE(t, csiSPIFFE)
	sb := spiffeSandbox()
	slot := &corev1.Pod{}
	slot.Namespace = sb.Namespace
	slot.Labels = map[string]string{SPIFFELabelKey: "enabled", PoolLabelKey: "agents", SlotNameLabelKey: "agents-x7k2p", SandboxLabelKey: sb.Name}
	setIdentityStatus(sb, slot)
	if sb.Status.Identity == nil || !strings.HasSuffix(sb.Status.Identity.SPIFFEID, "/pool/agents/slot/agents-x7k2p") {
		t.Errorf("slot %+v", sb.Status.Identity)
	}
	sb = spiffeSandbox()
	cold := &corev1.Pod{}
	cold.Labels = map[string]string{SPIFFELabelKey: "enabled", SandboxLabelKey: sb.Name}
	setIdentityStatus(sb, cold)
	if sb.Status.Identity == nil || !strings.HasSuffix(sb.Status.Identity.SPIFFEID, "/sandbox/"+sb.Name) {
		t.Errorf("cold %+v", sb.Status.Identity)
	}
	sb = spiffeSandbox()
	setIdentityStatus(sb, &corev1.Pod{})
	if sb.Status.Identity != nil {
		t.Errorf("unregistered launcher %+v", sb.Status.Identity)
	}
}

// A sandbox claims only slots registered the way it asks.
func TestSlotMismatch_Identity(t *testing.T) {
	pool := testPool(1)
	sb := plainSandbox(pool.Spec.Image)
	sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: pool.Name}
	base := len(slotMismatches(pool, sb))
	sb.Spec.Identity = &sandboxv1alpha1.SandboxIdentity{SPIFFE: sandboxv1alpha1.SandboxIdentitySPIFFEEnabled}
	got := slotMismatches(pool, sb)
	if len(got) != base+1 || !strings.Contains(strings.Join(got, ";"), "identity") {
		t.Errorf("mismatches %v", got)
	}
	pool.Spec.Identity = sb.Spec.Identity
	if len(slotMismatches(pool, sb)) != base {
		t.Errorf("same identity: %v", slotMismatches(pool, sb))
	}
	if !strings.Contains(poolSlotProfile(pool), `"identity":"SPIFFE"`) {
		t.Error("slot profile lacks the identity")
	}
	pool.Spec.Identity = nil
	if strings.Contains(poolSlotProfile(pool), "identity") {
		t.Error("slot profile changes for pools without identity")
	}
}

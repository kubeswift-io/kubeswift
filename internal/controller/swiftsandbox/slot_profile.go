package swiftsandbox

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// SlotNameLabelKey carries a warm slot's own name on its pod. Unlike
// SandboxLabelKey, it does not change at checkout, so the slot's deny-ingress
// NetworkPolicy -- which selects on it -- keeps covering the pod after a
// sandbox claims it. Selecting on SandboxLabelKey, which checkout rewrites to
// the claiming sandbox's name, left every checked-out workload with no ingress
// isolation.
const SlotNameLabelKey = "sandbox.kubeswift.io/slot"

// SlotProfileAnnotation records, on a warm slot pod, the shape it was booted
// with (see slotShape).
const SlotProfileAnnotation = "sandbox.kubeswift.io/slot-profile"

// slotShape is everything a warm slot was booted with that a checkout cannot
// change: a checkout only injects a command, so the slot's image, network
// mode, verification key, rootfs mode, kernel, size, node, GPU and model are
// what the workload gets.
//
// Checkout used to compare only the first three. A sandbox asking for 2 vCPUs
// and 1Gi ran in a 1 vCPU, 512Mi slot, and one with a nodeSelector ran on
// whatever node the slot had, with nothing saying so.
type slotShape struct {
	Image        string            `json:"image"`
	Network      string            `json:"network"`
	Egress       []string          `json:"egress,omitempty"`
	Ports        []string          `json:"ports,omitempty"`
	Ingress      string            `json:"ingress,omitempty"`
	VerifyKey    string            `json:"verifyKey,omitempty"`
	RootfsMode   string            `json:"rootfsMode"`
	Kernel       string            `json:"kernel"`
	CPU          int32             `json:"cpu"`
	Memory       string            `json:"memory"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	GPUProfile   string            `json:"gpuProfile,omitempty"`
	Model        string            `json:"model,omitempty"`
	ModelMount   string            `json:"modelMount,omitempty"`
}

func poolShape(pool *sandboxv1alpha1.SwiftSandboxPool) slotShape {
	s := slotShape{
		Image:      pool.Spec.Image,
		Network:    networkMode(pool.Spec.Network),
		Egress:     egressKey(pool.Namespace, pool.Spec.Network),
		Ports:      portsKey(pool.Spec.Network),
		Ingress:    ingressKey(pool.Spec.Network),
		VerifyKey:  secretName(pool.Spec.VerifyKeySecretRef),
		RootfsMode: rootfsMode(pool.Spec.RootfsMode),
		// As the pool resolves it for its slots: a GPU pool boots the
		// module-capable kernel unless it names one.
		Kernel: resolveKernelProfile(&sandboxv1alpha1.SwiftSandbox{Spec: sandboxv1alpha1.SwiftSandboxSpec{
			KernelProfileRef: pool.Spec.KernelProfileRef,
			GPUProfileRef:    pool.Spec.GPUProfileRef,
		}}),
		CPU:          cpuCount(pool.Spec.CPU),
		Memory:       pool.Spec.Memory.String(),
		NodeSelector: pool.Spec.NodeSelector,
	}
	if pool.Spec.GPUProfileRef != nil {
		s.GPUProfile = pool.Spec.GPUProfileRef.Name
	}
	if m := pool.Spec.Model; m != nil {
		s.Model, s.ModelMount = m.ImageRef, m.ModelMountPath()
	}
	return s
}

// poolSlotProfile is the annotation value for a slot booted under the pool's
// current spec. A warm slot whose annotation differs was booted under an
// earlier spec: the pool recycles it, and checkout never claims it.
func poolSlotProfile(pool *sandboxv1alpha1.SwiftSandboxPool) string {
	b, err := json.Marshal(poolShape(pool))
	if err != nil {
		// A struct of strings and a string map always marshals.
		panic(err)
	}
	return string(b)
}

// slotMismatches lists every way the pool's slots differ from what the
// sandbox asks for, or nothing when a slot honors the sandbox's spec.
//
// GPU and model are the slot's to give: a pooled sandbox sets neither and
// inherits the pool's. One that sets its own GPU (the webhook refuses that,
// but it is off by default), its own model, or a scratch disk asks for
// something no warm slot has, so it boots cold. The nodeSelector is honored
// when every label the sandbox requires is one the pool requires too, so
// every slot's node satisfies it.
func slotMismatches(pool *sandboxv1alpha1.SwiftSandboxPool, sb *sandboxv1alpha1.SwiftSandbox) []string {
	p := poolShape(pool)
	var out []string
	differ := func(field, poolVal, sbVal string) {
		if poolVal != sbVal {
			out = append(out, fmt.Sprintf("%s (pool %s, sandbox %s)", field, orNone(poolVal), orNone(sbVal)))
		}
	}
	differ("image", p.Image, sb.Spec.Image)
	differ("network mode", p.Network, networkMode(sb.Spec.Network))
	differ("network egress", strings.Join(p.Egress, "; "), strings.Join(egressKey(sb.Namespace, sb.Spec.Network), "; "))
	differ("network ports", strings.Join(p.Ports, ", "), strings.Join(portsKey(sb.Spec.Network), ", "))
	differ("network ingress", p.Ingress, ingressKey(sb.Spec.Network))
	differ("verifyKeySecretRef", p.VerifyKey, secretName(sb.Spec.VerifyKeySecretRef))
	differ("rootfsMode", p.RootfsMode, rootfsMode(sb.Spec.RootfsMode))
	differ("kernel", p.Kernel, checkoutKernel(pool, sb))
	differ("cpu", strconv.Itoa(int(p.CPU)), strconv.Itoa(int(cpuCount(sb.Spec.CPU))))
	if pool.Spec.Memory.Cmp(sb.Spec.Memory) != 0 {
		out = append(out, fmt.Sprintf("memory (pool %s, sandbox %s)", p.Memory, sb.Spec.Memory.String()))
	}
	if missing := unmetSelector(p.NodeSelector, sb.Spec.NodeSelector); len(missing) > 0 {
		out = append(out, fmt.Sprintf("nodeSelector (the pool does not require %s)", strings.Join(missing, ", ")))
	}
	if sb.UsesGPU() {
		out = append(out, "gpu (a sandbox with its own GPU boots cold)")
	}
	if m := sb.Spec.Model; m != nil && (m.ImageRef != p.Model || m.ModelMountPath() != p.ModelMount) {
		out = append(out, fmt.Sprintf("model (pool %s, sandbox %s at %s)", orNone(p.Model), m.ImageRef, m.ModelMountPath()))
	}
	if sb.Spec.ScratchDisk != nil {
		out = append(out, "scratchDisk (warm slots have none)")
	}
	if len(sb.Spec.Artifacts) > 0 {
		out = append(out, "artifacts (warm slots have none)")
	}
	return out
}

// checkoutKernel is the kernel the sandbox asks for when it claims from this
// pool: the one it names, else the GPU kernel when the pool's slots carry a
// GPU (the sandbox inherits it), else the base sandbox kernel.
func checkoutKernel(pool *sandboxv1alpha1.SwiftSandboxPool, sb *sandboxv1alpha1.SwiftSandbox) string {
	if sb.Spec.KernelProfileRef != nil && sb.Spec.KernelProfileRef.Name != "" {
		return sb.Spec.KernelProfileRef.Name
	}
	if pool.Spec.GPUProfileRef != nil {
		return gpuSandboxKernelProfile
	}
	return defaultKernelProfile
}

// unmetSelector returns the sandbox's node labels the pool does not require,
// sorted, as key=value.
func unmetSelector(pool, sb map[string]string) []string {
	var missing []string
	for k, v := range sb {
		if pv, ok := pool[k]; !ok || pv != v {
			missing = append(missing, k+"="+v)
		}
	}
	sort.Strings(missing)
	return missing
}

func networkMode(n sandboxv1alpha1.SandboxNetwork) string {
	if n.Mode == "" {
		return string(sandboxv1alpha1.SandboxNetworkRestricted) // the CRD default
	}
	return string(n.Mode)
}

func rootfsMode(m sandboxv1alpha1.SandboxRootfsMode) string {
	if m == "" {
		return string(sandboxv1alpha1.SandboxRootfsBlock) // the CRD default
	}
	return string(m)
}

func cpuCount(n int32) int32 {
	if n < 1 {
		return 1 // the CRD default
	}
	return n
}

func secretName(ref *sandboxv1alpha1.SecretObjectReference) string {
	if ref == nil {
		return ""
	}
	return ref.Name
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// egressKey is spec.network.egress.allow in a canonical, order-free form:
// one "service <ns>/<name> <ports>" or "cidr <cidr> <ports>" per entry, with
// the namespace and protocol defaults applied, sorted. Resolved addresses are
// not part of the shape; the pool replaces slots whose addresses go stale.
func egressKey(ns string, n sandboxv1alpha1.SandboxNetwork) []string {
	if n.Egress == nil {
		return nil
	}
	var out []string
	for _, r := range n.Egress.Allow {
		var ports []string
		for _, p := range r.Ports {
			ports = append(ports, fmt.Sprintf("%s/%d", strings.ToLower(string(protocol(p.Protocol))), p.Port))
		}
		sort.Strings(ports)
		dest := "cidr " + r.CIDR
		if c, err := sandboxwebhook.EgressCIDR(r.CIDR); err == nil {
			dest = "cidr " + c
		}
		if r.Service != nil {
			svcNS := r.Service.Namespace
			if svcNS == "" {
				svcNS = ns
			}
			dest = "service " + svcNS + "/" + r.Service.Name
		}
		if len(ports) == 0 {
			ports = []string{"all ports"}
		}
		out = append(out, dest+" "+strings.Join(ports, ","))
	}
	sort.Strings(out)
	return out
}

// portsKey is spec.network.ports as sorted "name:port/tcp", for networked modes.
func portsKey(n sandboxv1alpha1.SandboxNetwork) []string {
	var out []string
	for _, p := range n.Ports {
		out = append(out, fmt.Sprintf("%s:%d/%s", p.Name, p.Port, strings.ToLower(string(protocol(p.Protocol)))))
	}
	sort.Strings(out)
	return out
}

// ingressKey is spec.network.ingress.from as JSON; "" when any source may
// connect (no ingress, or an empty from).
func ingressKey(n sandboxv1alpha1.SandboxNetwork) string {
	if n.Ingress == nil || len(n.Ingress.From) == 0 {
		return ""
	}
	b, err := json.Marshal(n.Ingress.From)
	if err != nil {
		panic(err) // a list of API structs always marshals
	}
	return string(b)
}

package swiftsandbox

import (
	"strings"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// SlotNameLabelKey carries a warm slot's own name on its pod. Unlike
// SandboxLabelKey, it does not change at checkout, so the slot's deny-ingress
// NetworkPolicy -- which selects on it -- keeps covering the pod after a
// sandbox claims it. Selecting on SandboxLabelKey, which checkout rewrites to
// the claiming sandbox's name, left every checked-out workload with no ingress
// isolation.
const SlotNameLabelKey = "sandbox.kubeswift.io/slot"

// SlotProfileAnnotation records, on a warm slot pod, the security-relevant
// settings it was booted with (see slotProfile).
const SlotProfileAnnotation = "sandbox.kubeswift.io/slot-profile"

// slotProfile is the part of a sandbox's launch that a checkout must honor
// exactly: the image the workload runs in, the network mode it is confined to,
// and the key its image signature was verified against. A warm slot has
// already booted with all three, and a checkout only injects a command.
//
// Checkout used to ignore them. A "restricted" sandbox that claimed a slot of
// an "open" pool ran with open egress, a sandbox that required a verified
// image ran whatever the pool booted unverified, and a slot booted before a
// pool edit (say, open -> restricted) kept the old settings until it was
// claimed.
func slotProfile(image string, network sandboxv1alpha1.SandboxNetwork, verify *sandboxv1alpha1.SecretObjectReference) string {
	mode := network.Mode
	if mode == "" {
		mode = sandboxv1alpha1.SandboxNetworkRestricted // the CRD default
	}
	key := ""
	if verify != nil {
		key = verify.Name
	}
	return strings.Join([]string{"image=" + image, "network=" + string(mode), "verifyKey=" + key}, ";")
}

func poolSlotProfile(pool *sandboxv1alpha1.SwiftSandboxPool) string {
	return slotProfile(pool.Spec.Image, pool.Spec.Network, pool.Spec.VerifyKeySecretRef)
}

func sandboxSlotProfile(sb *sandboxv1alpha1.SwiftSandbox) string {
	return slotProfile(sb.Spec.Image, sb.Spec.Network, sb.Spec.VerifyKeySecretRef)
}

package clonecommon

import (
	"strings"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

// ComputeMACRewrites returns a CSV of new MACs indexed by NIC ordinal matching
// config.net[], deterministic in (targetNs, targetName, iface). Order: the
// source guest's interfaces in declaration order; a SwiftGuest with no explicit
// Interfaces uses a single default NIC named "eth0".
//
// This is the load-bearing per-clone collision-avoidance value: each clone's
// hypervisor config.net[].mac is rewritten to a value unique to its own
// (namespace, name), so two clones never share a host-side tap/bridge MAC. (The
// guest-VISIBLE MAC is inherited on CH --restore until the clone reboots — see
// the Phase 2 resume-vs-boot limitation; per-pod netns isolation prevents an
// actual L2 collision regardless.)
func ComputeMACRewrites(targetNs, targetName string, source *swiftv1alpha1.SwiftGuest) string {
	if len(source.Spec.Interfaces) == 0 {
		return runtimeintent.GenerateMAC(runtimeintent.InterfaceMACSeed(targetNs, targetName, "eth0"))
	}
	parts := make([]string, 0, len(source.Spec.Interfaces))
	for _, iface := range source.Spec.Interfaces {
		seed := runtimeintent.InterfaceMACSeed(targetNs, targetName, iface.Name)
		parts = append(parts, runtimeintent.GenerateMAC(seed))
	}
	return strings.Join(parts, ",")
}

package gateway

import (
	"google.golang.org/protobuf/types/known/timestamppb"
	apimeta "k8s.io/apimachinery/pkg/api/meta"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
)

// guestRunningCondition is the SwiftGuest condition that reports the VM is
// actually up. The operator writes it as a string literal (not a centralized
// const), so the gateway mirrors the literal here.
const guestRunningCondition = "GuestRunning"

// guestToProto maps a SwiftGuest to the flat, UI-shaped inventory row, stamped
// with its member cluster (the D2 dimension). It reads only the SwiftGuest
// object itself — no cross-resource lookups — so a List stays one round-trip
// per cluster; the expanded view (pod, events, gpu, storage) is GetGuestDetail
// in P1. cpu/memory live on the resolved SwiftGuestClass, not the guest status,
// so they are left zero here and enriched in P1.
func guestToProto(cluster string, g *swiftv1alpha1.SwiftGuest) *kubeswiftv1.Guest {
	out := &kubeswiftv1.Guest{
		Ref: &kubeswiftv1.ObjectRef{
			Cluster:   cluster,
			Namespace: g.Namespace,
			Name:      g.Name,
		},
		Phase:        string(g.Status.Phase),
		NodeName:     g.Status.NodeName,
		GuestClass:   g.Spec.GuestClassRef.Name,
		BootSource:   bootSource(g),
		GuestRunning: apimeta.IsStatusConditionTrue(g.Status.Conditions, guestRunningCondition),
	}
	if g.Status.Runtime != nil {
		out.Hypervisor = g.Status.Runtime.Hypervisor
	}
	if g.Status.Network != nil {
		out.PrimaryIp = g.Status.Network.PrimaryIP
	}
	if !g.CreationTimestamp.IsZero() {
		out.CreatedAt = timestamppb.New(g.CreationTimestamp.Time)
	}
	for i := range g.Status.Conditions {
		out.Conditions = append(out.Conditions, conditionToProto(&g.Status.Conditions[i]))
	}
	if len(g.Labels) > 0 {
		out.Labels = make(map[string]string, len(g.Labels))
		for k, v := range g.Labels {
			out.Labels[k] = v
		}
	}
	return out
}

// bootSource summarises the guest's boot origin into one human string for the
// inventory row (the three boot sources are mutually exclusive).
func bootSource(g *swiftv1alpha1.SwiftGuest) string {
	switch {
	case g.Spec.ImageRef != nil && g.Spec.ImageRef.Name != "":
		return "image/" + g.Spec.ImageRef.Name
	case g.Spec.KernelRef != nil && g.Spec.KernelRef.Name != "":
		return "kernel/" + g.Spec.KernelRef.Name
	case g.Spec.CloneFromSnapshot != nil && g.Spec.CloneFromSnapshot.SnapshotRef.Name != "":
		return "clone/" + g.Spec.CloneFromSnapshot.SnapshotRef.Name
	default:
		return ""
	}
}

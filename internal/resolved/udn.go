package resolved

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OVNPrimaryUDNNamespaceLabel marks a namespace whose pods ride a primary
// OVN-Kubernetes UserDefinedNetwork. A cluster admin sets it at namespace
// creation (immutable — OVN-K enforces it via a CEL webhook). KubeSwift reads it
// to decide whether a guest rides the namespace primary UDN (Model A); the label
// VALUE is ignored (presence is the signal), matching OVN-K.
const OVNPrimaryUDNNamespaceLabel = "k8s.ovn.org/primary-user-defined-network"

// OVNPrimaryUDNInterface is OVN-Kubernetes' deterministic in-pod interface name
// for the namespace primary UDN — the guest's portable L2. eth0 stays on the
// cluster default network for the swiftletd->apiserver control path and egress.
const OVNPrimaryUDNInterface = "ovn-udn1"

// NamespaceHasPrimaryUDN reports whether the namespace carries the
// OVN-Kubernetes primary-UDN label. Read-only; used by the resolver (Model A
// detection) and the SwiftMigration webhook (live-migration eligibility). The
// controller-runtime cached client opens a Namespace informer for this Get, so
// the manager needs `namespaces` get,list,watch RBAC (the W7/W8 lesson).
func NamespaceHasPrimaryUDN(ctx context.Context, c client.Client, namespace string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		// A missing namespace is impossible for a real guest (it lives in the
		// namespace), so NotFound means "no primary-UDN label" and resolution
		// proceeds node-local. Transient errors propagate so a real primary-UDN
		// guest is never silently downgraded (Principle #6).
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	_, ok := ns.Labels[OVNPrimaryUDNNamespaceLabel]
	return ok, nil
}

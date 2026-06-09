# Migration — Networking and Storage Requirements

> What your cluster must look like for Phase 1 offline migration to
> work, and the constraints on each axis.

## Storage

The guest's root-disk PVC must support **cross-node attach**: the
PV must be mountable from the destination node after the source-
node mount is released.

| Storage class | Phase 1 cross-node migration |
|---|---|
| Longhorn (RWO, full-copy) | Works. Detach window ~13s; reattach ~5s. |
| Rook Ceph RBD (RWO, CoW) | Works. Detach + reattach near-instant. |
| AWS EBS (RWO, cloud) | Works. Detach + reattach in cloud-API time (5-15s). |
| GCE PD (RWO, cloud) | Works. Similar to EBS. |
| RWX classes (CephFS, NFS-CSI) | Works trivially — no detach needed; the PV is concurrently mountable. |
| **local-path-provisioner** | **Rejected by the webhook.** Volumes are physically tied to one node. |

The webhook does not currently introspect the PVC's StorageClass to
preflight this — failures surface at the Preparing phase's VolumeAttachment
poll (it never clears). For now, operators avoid this by not using
local-only storage classes. A follow-up will add the StorageClass
preflight to Validating.

## Networking

### IP preservation cross-node

KubeSwift's default networking is a **node-local bridge** with per-
node dnsmasq DHCP. The guest's IP is local to the node it runs on;
cross-node migration produces a **fresh IP** from the destination
node's bridge.

**The validation webhook rejects** cross-node migrations of guests
on default networking unless the operator opts in via
`spec.allowIPChange=true`. When the opt-in triggers, the controller
writes the `IPWillChange=True` warning condition.

### Networks that preserve IPs cross-node

> Framework + recommendation for the multi-node-L2 options (and the code
> gaps to close):
> [`../design/network-architecture-requirements.md`](../design/network-architecture-requirements.md).

For IP preservation, attach the guest to a multi-node network. The
SwiftGuest's `spec.interfaces[*].networkRef` references a Multus
NetworkAttachmentDefinition. Compatible NAD types:

- **Multus + macvlan / bridge** on a shared L2 segment that spans
  both nodes.
- **OVN-Kubernetes layer-2** secondary network (logical switch
  spanning all nodes).
- **OVN-Kubernetes user-defined network (UDN)** of the right scope.

Layer-3 / routed networks (Calico) do NOT preserve guest IPs
cross-node by default — the guest's IP is bound to the source node's
veth/IPAM allocation.

The webhook treats any interface with `networkRef != nil` as
multi-node-capable in Phase 1 (we don't introspect the NAD content).
If your NAD is actually node-local (uncommon but possible), the
migration will succeed at the API level but the guest will lose its
IP — same behaviour as the default-bridge case but without the
explicit warning.

### SR-IOV interfaces

A guest with any `spec.interfaces[*].type: sriov` is treated as a
VFIO workload by the migration webhook and **rejected for cross-node
migration**. The VF is bound to the source-node hardware; reattach
on the destination would require freeing the source VF and binding
on the target — Phase 4+ work.

## Cluster baseline checks

Before submitting a SwiftMigration, verify:

```bash
# Worker nodes Ready and not cordoned.
kubectl get nodes

# Storage class for the guest's root PVC supports cross-node.
kubectl get pvc -n <ns> | grep swiftguest-root-<guest>
kubectl get pv <PV> -o jsonpath='{.spec.storageClassName}'

# The destination node has capacity for the guest.
kubectl describe node <target> | grep -E "Allocatable|Allocated"
```

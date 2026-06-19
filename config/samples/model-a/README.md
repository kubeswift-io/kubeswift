# Model A — namespace-native VM tenancy (primary OVN-K UDN)

A SwiftGuest on its namespace's **primary** OVN-Kubernetes UserDefinedNetwork: every pod
and VM in the namespace shares one isolated tenant network, and the VM holds a **native
UDN IP**, reachable cross-node. Full guide: [`docs/networking/udn-primary-tenancy.md`](../../../docs/networking/udn-primary-tenancy.md).

## Prerequisites

- **OVN-Kubernetes as the cluster CNI**, network-segmentation enabled. On OVN-K
  release-1.2 disable the identity webhook (`global.enableOvnKubeIdentity=false`) or
  primary-UDN pods fall back to the default network.
- KubeSwift **≥ v0.5.0**.
- A Ready `SwiftImage` named `ubuntu-noble` in `tenant-a` (provide your own).

## Apply

```bash
kubectl apply -f 00-namespace-and-udn.yaml   # namespace + primary-UDN label + UDN
kubectl apply -f 10-class-and-seed.yaml      # SwiftGuestClass + SwiftSeedProfile
# (create your SwiftImage `ubuntu-noble` in tenant-a here)
kubectl apply -f 20-guest.yaml               # the guest — no Model-A-specific spec
```

## Verify

```bash
kubectl -n tenant-a get swiftguest tenant-vm -o wide
# status.network.primaryIP is a 10.50.0.x (the UDN), GuestRunning=True.

# Cross-node reachability (a pod in tenant-a on another node reaches the VM's UDN IP):
kubectl -n tenant-a run probe --image=nicolaka/netshoot --restart=Never -- sleep 3600
kubectl -n tenant-a exec probe -- ping -c2 <the primaryIP>
```

## Scope (v1)

Boot + native UDN IP + cross-node + tenant isolation. **Offline** migration / drain
evacuation works (the target gets a fresh UDN IP). **Live** migration and snapshot of a
Model A guest are **v2** — `mode: live` is rejected at admission; `auto` resolves to
offline. For IP-preserving live migration today, use the secondary-UDN path
([Model B](../../../docs/networking/udn-multi-tenancy.md)).

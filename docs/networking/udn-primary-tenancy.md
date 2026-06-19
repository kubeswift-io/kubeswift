# Namespace-native VM tenancy — guest on the primary OVN-Kubernetes UDN (Model A)

Put a SwiftGuest VM **directly on its namespace's primary network** when that primary
is an OVN-Kubernetes **UserDefinedNetwork (UDN)**. Every pod *and* VM in the namespace
shares one isolated tenant network — **namespace-native tenancy** — and each VM holds a
**native UDN IP**, reachable cross-node, isolated from other tenants.

This is the **Model A** path. Compare with the **Model B** path
([`udn-multi-tenancy.md`](udn-multi-tenancy.md)), where the VM rides a *secondary* UDN
via a per-guest `networkRef` and the launcher pod's `eth0` stays on the cluster-default
network. Pick Model A when the *namespace itself* is the tenant boundary (mixed pod+VM
workloads on one primary network, no per-workload `networkRef`); pick Model B when you
want per-guest opt-in on top of a normal namespace.

## Scope (v1)

| Capability | Model A v1 |
|---|---|
| Boot with a native UDN IP | ✅ |
| Cross-node reachability on the UDN IP | ✅ |
| Tenant L2 isolation | ✅ (OVN-K UDN) |
| **Offline** migration / drain evacuation | ✅ (target acquires a fresh UDN IP) |
| **Live** migration (IP-preserving) | ❌ **v2** — rejected at admission (see below) |
| Snapshot / clone of a Model A guest | ❌ **v2** |

Live migration of a Model A guest is **not supported in v1** and is rejected with a clear
message: the primary UDN withholds the swiftletd↔swiftletd migration channel from the
launcher pod (the destination pod's `eth0` is infrastructure-locked, so the migration TCP
has no pod-to-pod path). `mode: auto` resolves to **offline** for these guests, so
`kubectl drain` still evacuates them (with a fresh IP on the target). If you need
IP-preserving live migration today, use **Model B** (a secondary UDN —
[`udn-multi-tenancy.md`](udn-multi-tenancy.md)).

## Prerequisites

- **OVN-Kubernetes as the cluster (primary) CNI**, with network-segmentation /
  UserDefinedNetworks enabled. Install: [`ovn-kubernetes-install.md`](ovn-kubernetes-install.md)
  and [`kubeadm-ovn-kubernetes-setup.md`](kubeadm-ovn-kubernetes-setup.md).
- **OVN-K release-1.2 caveat:** its pod-admission identity webhook rejects a primary-UDN
  pod annotation. Disable it (`--set global.enableOvnKubeIdentity=false` on the OVN-K
  Helm release) or use a newer OVN-K. Without this, primary-UDN pods silently fall back to
  the cluster-default network.
- KubeSwift **≥ v0.5.0**.

## Recipe

### 1. Create the tenant namespace with the primary-UDN label

OVN-K binds a namespace's primary UDN by the **presence** of this label (the value is
ignored):

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: tenant-a
  labels:
    k8s.ovn.org/primary-user-defined-network: ""
```

The label is immutable and must be set **at namespace creation** (OVN-K enforces this).

### 2. Create the primary UserDefinedNetwork

```yaml
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: tenant-a-udn
  namespace: tenant-a
spec:
  topology: Layer2
  layer2:
    role: Primary
    subnets: ["10.50.0.0/16"]
```

Every pod created in `tenant-a` now gets an `ovn-udn1` interface on this network as its
primary, plus an `eth0` on the cluster-default network locked to kubelet health checks.

### 3. Deploy a guest — no special spec

A plain SwiftGuest in the namespace is automatically Model A (KubeSwift detects the
namespace label):

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: tenant-vm
  namespace: tenant-a
spec:
  imageRef: { name: ubuntu-noble }
  guestClassRef: { name: small }
  seedProfileRef: { name: default-seed }
  runPolicy: Running
```

The VM boots holding a `10.50.0.x` address from the tenant UDN. `status.network.primaryIP`
reports it; `GuestRunning` flips True when Cloud Hypervisor is up. A pod (or VM) in the
same namespace on **any node** can reach it on that IP.

Full sample set: [`config/samples/model-a/`](../../config/samples/model-a/).

## How it works (brief)

- **Datapath (bridge-binding):** OVN-K assigns the launcher pod a UDN IP + an IP-derived
  MAC (`0a:58:<ip-hex>`); `network-init` bridges `ovn-udn1` to the VM's tap so the **VM
  adopts that exact IP+MAC** (OVN `port_security` pins them). This is the KubeVirt
  bridge-binding pattern.
- **Status (controller-derived):** because the UDN is bridged to the VM and `eth0` is
  infrastructure-locked, swiftletd **cannot reach the apiserver** from the pod. So the
  **controller** derives status: `primaryIP` from the pod's `k8s.ovn.org/pod-networks`
  annotation, `GuestRunning` from a launcher CH-socket readiness probe. swiftletd skips
  all apiserver calls for these guests.

Design detail: [`docs/design/udn-primary-integration.md`](../design/udn-primary-integration.md).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Guest stuck `Scheduling`, no IP | The pod has no `ovn-udn1` (OVN-K identity webhook rejected the primary-UDN annotation) — set `global.enableOvnKubeIdentity=false`. Check `kubectl get pod <g> -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/pod-networks}'`. |
| IP is `192.168.99.x` (node-local), not the UDN subnet | The namespace lacks the primary-UDN label, or the controller is < v0.5.0. Confirm the label and the controller image. |
| `swiftctl migrate … --mode live` rejected | Expected in v1 — Model A guests are offline-only. Use `--mode offline` (or `auto`). |
| `kubectl drain` of a Model A node | Works (offline migration, fresh IP). Remember `--delete-emptydir-data` (the launcher pod uses emptyDir). |

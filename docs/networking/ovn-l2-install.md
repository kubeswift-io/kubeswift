# Installing an OVN layer-2 secondary network (kube-ovn non-primary)

> Goal: stand up a **multi-node L2 overlay** that a KubeSwift guest can use as its
> **portable primary IP** — the network substrate for **IP-preserving live
> migration**, telco/NFV, and stateful services with external clients. This is the
> path KubeSwift's primary-on-NAD live migration is **validated end-to-end** on
> (cross-node `mode: live`, no `allowIPChange`, ~3.2 s downtime, IP preserved).
>
> See [Multi-node L2 networking](multi-node-l2.md) for the feature/CRD reference
> and [Network Architecture Requirements](../design/network-architecture-requirements.md)
> for the framework. Runnable manifests:
> [`config/samples/multi-node-l2/`](../../config/samples/multi-node-l2/).

---

## OVN-Kubernetes vs. kube-ovn — pick the right tool

Both are OVN-based CNIs. The choice hinges on **whether OVN runs your cluster's
primary pod network**:

| Your cluster | Use | Why |
|---|---|---|
| OVN-Kubernetes (or kube-ovn) is **already the primary CNI** | its **native** secondary networks (`topology: layer2` NAD) | the OVN control plane is already there |
| A **different** primary CNI (Calico, Cilium, Flannel, Canal…) and you want to **keep it** | **kube-ovn in non-primary mode** (this guide) | upstream OVN-Kubernetes secondary networks assume OVN-K *is* the primary CNI; **kube-ovn's non-primary mode** is purpose-built to run as a *secondary* alongside any primary CNI |
| Throwaway / greenfield, no primary CNI yet | OVN-Kubernetes or kube-ovn as the **primary** CNI | simplest; no coexistence concerns |

This guide covers the **kube-ovn non-primary** case (the common one: an existing
Calico/Cilium cluster you don't want to re-CNI). It was validated on a 3-node
**k0s** cluster with **Calico** primary; the steps are CNI-agnostic.

> KubeSwift does **not** import or depend on any OVN type. It attaches through the
> standard **Multus + NetworkAttachmentDefinition** interface, so the same
> SwiftGuest manifests work whether the NAD is backed by kube-ovn, upstream
> OVN-K, macvlan, or bridge CNI. For a kube-ovn-class NAD the controller
> additionally programs the guest's OVN port identity automatically (see
> [Step 4](#step-4--run-a-guest-on-the-segment)).

---

## Prerequisites

- A working cluster with a **primary CNI** (Calico/Cilium/…) and **Multus**
  installed (KubeSwift requires Multus already).
- The **`openvswitch` kernel module** available on every node. kube-ovn bundles
  the OVS *userspace* (in its `ovs-ovn` DaemonSet) but uses the host kernel
  module. On Ubuntu 24.04 it is present by default; verify with
  `lsmod | grep openvswitch` (or `modprobe openvswitch`). No host OVS package is
  needed.
- **Helm 3** and cluster-admin.
- Node-to-node connectivity on the OVN tunnel/DB ports (Geneve UDP **6081**,
  OVSDB **6641/6642**). On a cloud with a node firewall, allow these between
  nodes (the same way your CNI's overlay port is allowed).
- KubeSwift **≥ the release carrying the kube-ovn primary-NAD integration** (both
  the controller stamping, `#239`, **and** the launcher `network-init` datapath
  fix that re-MACs the pod NIC off the guest MAC). Both the controller-manager and
  the launcher (`swiftletd`) image must carry it. Earlier builds boot a
  kube-ovn-primary guest but it is **unreachable** (the OVN port identity isn't
  programmed, or the pod NIC shadows the guest's tap on `br0`).

---

## Step 1 — install kube-ovn in non-primary mode

```bash
helm repo add kubeovn https://kubeovn.github.io/kube-ovn/
helm repo update kubeovn

# Designate the node(s) that run the OVN central DBs. Any schedulable node works;
# a worker avoids control-plane NoSchedule taints. (MASTER_NODES="" => use the label.)
kubectl label node <a-worker-node> kube-ovn/role=master --overwrite

helm install kube-ovn kubeovn/kube-ovn --version v1.16.2 \
  -n kube-system -f kube-ovn-nonprimary-values.yaml
```

`kube-ovn-nonprimary-values.yaml` (also in
[`config/samples/multi-node-l2/kube-ovn-nonprimary-values.yaml`](../../config/samples/multi-node-l2/kube-ovn-nonprimary-values.yaml)):

```yaml
MASTER_NODES: ""                 # use the kube-ovn/role=master label
cni_conf:
  NON_PRIMARY_CNI: true          # do NOT take over the primary pod network
  CNI_CONFIG_PRIORITY: "99"      # SAFETY: sort the kube-ovn conflist AFTER the
                                 # primary CNI's so it can never preempt it
  CNI_BIN_DIR: "/opt/cni/bin"
  CNI_CONF_DIR: "/etc/cni/net.d"
  MOUNT_CNI_CONF_DIR: "/etc/cni/net.d"
kubelet_conf:
  KUBELET_DIR: "/var/lib/kubelet"   # k0s/kubeadm default; set to your kubelet dir
networking:
  NET_STACK: ipv4
  NETWORK_TYPE: geneve
  IFACE: ""                      # auto-detect the node-IP interface
ipv4:
  POD_CIDR: "10.16.0.0/16"       # OVN default VPC — MUST NOT overlap your pod CIDR
  POD_GATEWAY: "10.16.0.1"
  SVC_CIDR: "10.96.0.0/12"       # match your cluster service CIDR
  JOIN_CIDR: "100.64.0.0/16"
```

> **The `CNI_CONFIG_PRIORITY: "99"` is load-bearing.** kube-ovn's `install-cni`
> writes a `<priority>-kube-ovn.conflist` into `/etc/cni/net.d` *unconditionally*
> (the non-primary flag only changes the cni-server's runtime behavior). At the
> default `01` it would sort **before** `10-<your-cni>.conflist` and the kubelet
> would pick kube-ovn as the primary CNI — breaking all pod networking. `99`
> guarantees the primary CNI stays first. (KubeSwift's smoke test of this install
> confirmed Calico stayed primary.)

> **Match the CIDRs to your cluster.** `ipv4.POD_CIDR`/`JOIN_CIDR` are OVN's
> internal VPC ranges; they must not overlap your real pod CIDR, service CIDR, or
> node IPs. Set `ipv4.SVC_CIDR` to your cluster's service CIDR
> (`kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}'` is the first IP of
> it). The defaults above (`10.16/16`, `100.64/16`) are clear of the common
> Calico `10.244/16` and k0s `10.96/12`.

---

## Step 2 — verify the install (and that your primary CNI is untouched)

```bash
# All kube-ovn components Running (ovn-central, kube-ovn-controller, and the
# ovs-ovn + kube-ovn-cni DaemonSets on every node):
kubectl get pods -n kube-system | grep -E 'ovn|ovs'

# SAFETY CHECK — a plain pod must still get an address from your PRIMARY CNI,
# not from kube-ovn's 10.16/16:
kubectl run cni-check --image=busybox --restart=Never --command -- sleep 60
kubectl get pod cni-check -o jsonpath='{.status.podIP}{"\n"}'   # expect your CNI's range
kubectl delete pod cni-check
```

If the plain pod gets a `10.16.x` address, kube-ovn took over the primary network
— uninstall and re-check `CNI_CONFIG_PRIORITY`.

---

## Step 3 — create the L2 segment (kube-ovn Subnet + NAD)

```yaml
apiVersion: kubeovn.io/v1
kind: Subnet
metadata:
  name: ovn-l2                       # cluster-scoped
spec:
  protocol: IPv4
  cidrBlock: 10.20.0.0/16            # the guests' portable IP range (clear of all above)
  excludeIps: ["10.20.0.1"]
  gateway: "10.20.0.1"
  gatewayType: distributed
  natOutgoing: false                 # a flat L2 segment, not a routed/NAT'd subnet
  provider: ovn-l2.<namespace>.ovn   # <nad-name>.<nad-namespace>.ovn  (the link to the NAD)
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ovn-l2
  namespace: <namespace>             # same ns as your SwiftGuests
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "type": "kube-ovn",
      "server_socket": "/run/openvswitch/kube-ovn-daemon.sock",
      "provider": "ovn-l2.<namespace>.ovn"
    }
```

Verify it is Ready and that a pod attaches cross-node:

```bash
kubectl get subnet ovn-l2          # PROTOCOL IPv4, V4AVAILABLE > 0
# (optional) attach a test pod with annotation k8s.v1.cni.cncf.io/networks: <ns>/ovn-l2
# on two different nodes and ping between their net1 (10.20.x) addresses.
```

> kube-ovn secondary Subnets do **not** serve DHCP by default — the pod gets a
> static IPAM address on `net1`, and KubeSwift's per-pod dnsmasq hands that exact
> address to the guest. So there is **no DHCP conflict** to manage here (unlike a
> segment that runs its own DHCP).

> **Multiple tenants?** Give each tenant its own Subnet + NAD pair (isolated logical
> switches) — see [kubeovn-multi-tenancy.md](kubeovn-multi-tenancy.md) for the
> per-tenant recipe.

---

## Step 4 — run a guest on the segment

Put the guest's **primary** interface on the NAD:

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: portable-vm
  namespace: <namespace>
spec:
  imageRef: { name: ubuntu-noble }
  guestClassRef: { name: default }
  seedProfileRef: { name: default }
  runPolicy: Running
  # RWX+Block storage is required to LIVE-migrate (a migratable CSI):
  storage: { accessMode: ReadWriteMany, volumeMode: Block, storageClassName: <migratable-sc> }
  interfaces:
    - name: app
      primary: true                  # this NIC is the guest's primary (portable) IP
      networkRef: { name: ovn-l2 }    # ...riding the OVN segment
```

The guest comes up `Running` with `status.network.primaryIP` from the `10.20.x`
segment. **What KubeSwift does automatically for a kube-ovn-class NAD** (since
`#239`): OVN binds each logical-switch port to the *pod NIC's* MAC, but KubeSwift's
datapath bridges the guest's *own* hypervisor MAC behind the pod NIC — so the
controller stamps `<provider>.kubernetes.io/mac_address` (the guest MAC) on the
launcher pod, making the OVN port identity the guest. The guest is then reachable
on the segment with **no manual `ovn-nbctl`**. Verify from a pod on another node:

```bash
kubectl get swiftguest -n <namespace> portable-vm -o jsonpath='{.status.network.primaryIP}{"\n"}'
# ping that 10.20.x.y from a pod attached to ovn-l2 on a different node -> reachable
```

---

## Step 5 — live-migrate with the IP preserved

Because the primary rides a multi-node NAD, `mode: live` is accepted **without
`allowIPChange`**:

```bash
swiftctl migrate portable-vm -n <namespace> --to <other-node>   # mode auto -> live
# or apply a SwiftMigration with spec.mode: live
kubectl get swiftmigration -n <namespace> -w
```

Expect `Completed`, `observedDowntime` a few seconds, the guest on the target node
with the **same** IP, and reachability from a third node afterward. The controller
sets `kubevirt.io/migrationJobName` on the destination pod so kube-ovn lets the
destination keep the source's static IP through the cutover.

---

## k0s notes

- k0s manages its CNI/kubelet under `/var/lib/kubelet` and `/etc/cni/net.d` (the
  defaults above match). It does not reconcile away kube-ovn's Helm-installed
  workloads or the stray `99-kube-ovn.conflist`.
- The CNI bin dir is `/opt/cni/bin` (the default above); kube-ovn's `install-cni`
  copies its binary there and re-copies on DaemonSet restart, so it self-heals if
  anything prunes it.

## Troubleshooting

- **`kubectl get sg` returns a kube-ovn `security-groups` error.** kube-ovn
  registers `sg` as a short name that shadows SwiftGuest's. Use the full
  `kubectl get swiftguest` (or `swiftguests.swift.kubeswift.io`).
- **Guest gets an IP but is unreachable cross-node.** Either the controller is
  **older than `#239`** (no auto LSP-identity — confirm the launcher pod has a
  `<provider>.kubernetes.io/mac_address` annotation), or the **launcher image
  predates the `network-init` re-MAC fix** (the pod NIC shadows the guest's tap on
  `br0` — `bridge fdb show br0` shows a permanent `<guest-mac> -> net1` entry).
  Upgrade both images; or check the NAD `type` is exactly `kube-ovn`.
- **Stale ARP after a fix/migration.** A prober that pinged before the identity
  was programmed caches an incomplete entry; `ip neigh flush all` (or just wait)
  and re-ping.
- **dst pod can't get the IP during migration.** Ensure the controller image is
  `#239`+ (it sets `kubevirt.io/migrationJobName`) and ensure `--keep-vm-ip=true`
  is enabled in kube-ovn.

## Uninstall (return to primary-CNI-only)

```bash
kubectl delete subnet ovn-l2
kubectl delete -n <namespace> net-attach-def ovn-l2
helm uninstall kube-ovn -n kube-system
kubectl label node <a-worker-node> kube-ovn/role-
# the inert 99-kube-ovn.conflist (sorts after your primary CNI) can be left or
# removed from /etc/cni/net.d; the cni-server won't re-add it post-uninstall.
```

Your primary CNI is unaffected by the removal.

# Building a kubeadm cluster with OVN-Kubernetes as primary CNI for KubeSwift + UDN

> Goal: stand up a **kubeadm** cluster running **OVN-Kubernetes as the primary CNI**
> (with the multi-network / network-segmentation features), then install **Longhorn**
> + **KubeSwift**, and use **UserDefinedNetworks (UDN)** for per-tenant isolated VM
> networks with IP-preserving live migration.
>
> **Status: cluster-validated end-to-end (2026-06-18)** on a 3-node Ubuntu 24.04
> kubeadm v1.34 cluster. Every command below is what was actually run; the install
> gotchas are the ones really hit (and are the reason this guide exists).
>
> This is the **cluster bring-up** companion to
> [`ovn-kubernetes-install.md`](ovn-kubernetes-install.md) (which assumes the cluster
> already exists and covers *running guests* on it) and
> [`udn-multi-tenancy.md`](udn-multi-tenancy.md) (the per-tenant UDN recipe).

---

## What you'll build

```
kubeadm (no kube-proxy)
  └─ OVN-Kubernetes  ← primary CNI, single-node-zone (interconnect), segmentation ON
       └─ Multus     ← delegates to OVN-K; required by KubeSwift for NADs
            └─ Longhorn (migratable RWX+Block StorageClass)
                 └─ KubeSwift  ← VM runtime
                      └─ UserDefinedNetwork (per-tenant isolated VM L2 + live-migration)
```

## Reference environment

- 3 nodes, **Ubuntu 24.04**, kernel 6.8, ≥6 vCPU / ≥8 GiB each.
- A **dedicated disk** per node for Longhorn (mounted at `/var/lib/longhorn`); the
  validated cluster used a 60 GiB second disk. Longhorn can also use the root disk
  if it has space.
- A flat L2 between the nodes (the validated cluster: `172.16.56.0/24` on `enp1s0`).
- Pick non-overlapping CIDRs: pod `10.244.0.0/16`, service `10.96.0.0/16` (used below).

> **Memory sizing.** OVN-K single-node-zone runs a full NB+SB DB + northd on **every**
> node (~1 GiB) and Longhorn adds ~1 GiB; size nodes so a node can hold its
> system + a guest + a transient live-migration target. For lab validation, prefer
> small (≤2 GiB) guests.

---

## Step 1 — node prep (all nodes)

Containerd with the systemd cgroup driver, swap off, kernel modules, sysctls, Open
vSwitch, and the Longhorn block-storage prerequisites:

```bash
# containerd (systemd cgroup driver) + kubeadm/kubelet/kubectl per the upstream
# "Installing kubeadm" guide for your k8s version. Confirm:
#   containerd config: SystemdCgroup = true
#   swap OFF: swapoff -a  (and remove the swap line from /etc/fstab)

# kernel modules (OVN-K needs openvswitch; k8s needs overlay/br_netfilter)
cat <<'EOF' | sudo tee /etc/modules-load.d/k8s-ovn.conf
overlay
br_netfilter
openvswitch
EOF
sudo modprobe overlay br_netfilter openvswitch

# sysctls
cat <<'EOF' | sudo tee /etc/sysctl.d/k8s.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sudo sysctl --system

# Open vSwitch userspace (the OVN-K node pod manages OVS, but the host package
# provides the tooling + ensures the datapath kernel module is present)
sudo apt-get install -y openvswitch-switch

# Longhorn prerequisites: iSCSI + NFS
sudo apt-get install -y open-iscsi nfs-common
sudo systemctl enable --now iscsid
sudo modprobe iscsi_tcp

# Mount the dedicated Longhorn disk (example: /dev/vdb) at /var/lib/longhorn
sudo mkfs.ext4 -F /dev/vdb          # only if blank
echo '/dev/vdb /var/lib/longhorn ext4 defaults 0 2' | sudo tee -a /etc/fstab
sudo mkdir -p /var/lib/longhorn && sudo mount /var/lib/longhorn
```

> **Important — stop the host OVS service.** OVN-Kubernetes' `ovnkube-node` /
> `ovs-node` pods run `ovsdb-server` + `ovs-vswitchd` **in-container** and bind the
> host's `/var/run/openvswitch` socket. A running host `openvswitch-switch` service
> conflicts. Disable it (the kernel module stays loaded):
> ```bash
> sudo systemctl disable --now openvswitch-switch
> ```

---

## Step 2 — kubeadm init + join (NO kube-proxy)

OVN-Kubernetes provides services itself, so skip kube-proxy at init:

```bash
# On the control-plane node (advertise its real IP):
sudo kubeadm init \
  --pod-network-cidr=10.244.0.0/16 \
  --service-cidr=10.96.0.0/16 \
  --apiserver-advertise-address=<CONTROL_PLANE_IP> \
  --skip-phases=addon/kube-proxy

# Copy admin.conf out (use it as your KUBECONFIG):
mkdir -p ~/.kube && sudo cp /etc/kubernetes/admin.conf ~/.kube/config && sudo chown $(id -u):$(id -g) ~/.kube/config

# Join each worker with the `kubeadm join ...` command printed by init.
# (If you will run VMs on the control-plane node too, untaint it:)
kubectl taint nodes <CONTROL_PLANE> node-role.kubernetes.io/control-plane- || true
```

All nodes are `NotReady` until the CNI is up — expected.

---

## Step 3 — install the Multus-ecosystem CRDs **first** (the #1 gotcha)

OVN-Kubernetes with multi-network/segmentation enabled **watches three CRDs at
startup** and crash-loops in sequence if any is missing (`cluster-manager` fails to
list NADs → never stamps node zones → `ovnkube-node` times out). Install the **CRDs
only** now — the Multus *daemonset* comes later (it needs a working CNI, a circular
dependency the CRD-only install breaks):

```bash
# NetworkAttachmentDefinition
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/network-attachment-definition-client/master/artifacts/networks-crd.yaml
# MultiNetworkPolicy (ovnkube-controller watches it)
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multi-networkpolicy/master/scheme.yml
# IPAMClaim (cluster-manager watches it when persistent-IPs/segmentation is on)
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/ipamclaims/main/artifacts/k8s.cni.cncf.io_ipamclaims.yaml

kubectl get crd | grep -E 'network-attachment-definitions|multi-networkpolicies|ipamclaims'
```

---

## Step 4 — install OVN-Kubernetes (Helm, single-node-zone, segmentation ON)

Use the chart from the OVN-Kubernetes repo. `release-1.2` is the validated version
(latest released at the time):

```bash
git clone --depth 1 --branch release-1.2 https://github.com/ovn-kubernetes/ovn-kubernetes.git
cd ovn-kubernetes/helm/ovn-kubernetes

helm install ovn-kubernetes . -f values-single-node-zone.yaml \
  --set k8sAPIServer="https://<CONTROL_PLANE_IP>:6443" \
  --set global.image.repository=ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu \
  --set global.image.tag=release-1.2 \
  --set global.enableNetworkSegmentation=true \
  --set global.enableMultiNetwork=true
```

Notes:
- **`values-single-node-zone.yaml`** = interconnect mode where **each node is its own
  zone** (its own NB/SB DB) — this is the multi-node deployment.
- **`k8sAPIServer` MUST be overridden** — the chart default points at a placeholder
  (`https://172.25.0.2:6443`); set it to your real apiserver.
- `podNetwork`/`serviceNetwork` in the values default to `10.244.0.0/16/24` /
  `10.96.0.0/16` — match your kubeadm CIDRs (above) or override.
- `gatewayMode: shared` (the default) auto-detects the default-route NIC into
  `br-ex`. On the validated VMs this did **not** break SSH; keep a console handy on
  bare metal in case the gateway move disrupts the management NIC.

Wait for health (all `ovnkube-node` pods `6/6`, nodes `Ready`):

```bash
kubectl -n ovn-kubernetes get pods
kubectl get nodes
# A plain pod should now get a 10.244.x address (CNI works):
kubectl run cni-check --image=registry.k8s.io/pause:3.10 --restart=Never
kubectl get pod cni-check -o jsonpath='{.status.podIP}{"\n"}'; kubectl delete pod cni-check
```

---

## Step 5 — install Multus (the #2 gotcha: raise its memory)

```bash
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset-thick.yml
```

> **The thick Multus daemon OOMs at its 50Mi default limit**, producing
> `multus-shim ... Post "http://dummy/cni": EOF` on every pod sandbox (it looks like
> a CNI flake; it's the daemon being OOMKilled). Raise it once — permanent fix:
> ```bash
> kubectl set resources ds/kube-multus-ds -n kube-system --limits=memory=512Mi --requests=memory=128Mi
> ```

Confirm `kube-multus-ds` is `1/1` on every node with **0 restarts**, then re-check a
plain pod still gets an IP (Multus now delegates to OVN-K).

---

## Step 6 — (only for **primary** UDN) disable the identity webhook

**Skip this step** if you only use *secondary* UDNs / NADs (the recommended KubeSwift
multi-tenancy path — see [`udn-multi-tenancy.md`](udn-multi-tenancy.md)). It is needed
**only** if you create **primary** UserDefinedNetworks on OVN-K **release-1.2**, whose
`ovn-kubernetes-admission-webhook-pod` rejects a primary-UDN pod annotation
(*"no ovn pod annotation for NAD default"*):

```bash
helm upgrade ovn-kubernetes . -f values-single-node-zone.yaml \
  --set k8sAPIServer="https://<CONTROL_PLANE_IP>:6443" \
  --set global.image.repository=ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu \
  --set global.image.tag=release-1.2 \
  --set global.enableNetworkSegmentation=true \
  --set global.enableMultiNetwork=true \
  --set global.enableOvnKubeIdentity=false
```

This drops a security gate (the per-node cert/annotation admission). In single-node-zone
mode each node talks to its **local** NB/SB DB over a unix socket, so the datapath is
unaffected. A newer OVN-K release may fix the webhook and make this unnecessary.

---

## Step 7 — install Longhorn + a migratable StorageClass

Prerequisites (iSCSI, NFS, the data disk) were done in Step 1. Install the latest
Longhorn that supports your k8s version (validated: **v1.12.0** on k8s 1.34):

```bash
TAG=$(curl -s https://api.github.com/repos/longhorn/longhorn/releases/latest | grep '"tag_name"' | cut -d'"' -f4)
kubectl apply -f "https://raw.githubusercontent.com/longhorn/longhorn/$TAG/deploy/longhorn.yaml"
kubectl -n longhorn-system get pods -w     # wait until all Running
```

Create the **migratable RWX+Block** StorageClass KubeSwift live migration needs:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: longhorn-migratable
provisioner: driver.longhorn.io
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
parameters:
  numberOfReplicas: "2"
  staleReplicaTimeout: "30"
  migratable: "true"
  dataLocality: "best-effort"
```

---

## Step 8 — install KubeSwift

Install the chart, pinning images to a release tag (or `sha-<commit>` for a dev
build). Use a build **≥ the OVN-Kubernetes backend** (OVN-K arc P2, `#244`) — and
ideally one with the VolumeSnapshot-CRD tolerance (`#245`) so the controller does not
require the external-snapshotter CRDs (absent on a bare Longhorn cluster):

```bash
helm install kubeswift charts/kubeswift -n kubeswift-system --create-namespace \
  --set controllerManager.image.tag=<tag> \
  --set swiftletd.image.tag=<tag> \
  --set snapshotS3.image.tag=<tag> \
  --set migrationStunnel.image.tag=<tag>

kubectl -n kubeswift-system get pods   # controller-manager 1/1 Running
```

> On a cluster **without** the CSI VolumeSnapshot CRDs, a `#245`+ controller logs
> *"VolumeSnapshot CRDs not installed... disabled... core runtime unaffected"* and
> starts normally. An older controller crash-loops — install the external-snapshotter
> CRDs first if your build predates `#245`.

---

## Step 9 — set up UDN for KubeSwift (per-tenant isolated VM networks)

The recommended, **zero-code** multi-tenancy path: a per-tenant **secondary**
UserDefinedNetwork with persistent IPAM. OVN-K auto-generates an
`ovn-k8s-cni-overlay` layer2 NAD that KubeSwift's shipped backend drives — a guest
just references it by `networkRef`.

```yaml
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: tenant-a
  namespace: tenant-a
spec:
  topology: Layer2
  layer2:
    role: Secondary
    subnets: ["10.30.0.0/16"]
    ipam:
      lifecycle: Persistent       # REQUIRED for IP-preserving LIVE migration
```

Then point a SwiftGuest's primary interface at it (`networkRef: { name: tenant-a }`).
Full recipe, sample, and gotchas: **[`udn-multi-tenancy.md`](udn-multi-tenancy.md)**.

> **Primary UDN** (the guest on the namespace's *primary* network, `ovn-udn1`) is a
> KubeSwift **roadmap item** (Model A), not yet shipped — it needs a launcher-datapath
> integration on top of the Step 6 webhook caveat. Use the secondary-UDN path above
> for multi-tenancy today.

---

## Step 10 — validate (boot + IP-preserving live migration)

Follow [`ovn-kubernetes-install.md`](ovn-kubernetes-install.md) Steps 4–5: a guest on
the (UDN-generated or hand-written) layer2 NAD comes up `Running` with its segment IP,
is reachable cross-node, and **live-migrates `mode: live` with no `allowIPChange`,
IP preserved**. On the validated kubeadm cluster this Completed in **~3.2–3.3 s**
downtime with the IP preserved and reachable from a third node.

---

## Gotcha checklist (the things that actually bite)

| Symptom | Cause | Fix |
|---|---|---|
| `ovnkube-control-plane`/`-node` crash-loop right after install | NAD / MultiNetworkPolicy / IPAMClaim CRDs missing at startup | Step 3 — install all three CRDs **before** OVN-K |
| Pods stuck `ContainerCreating`, `multus-shim ... Post "http://dummy/cni": EOF` | thick-Multus daemon OOMKilled at 50Mi | Step 5 — raise the daemonset memory to 512Mi |
| `ovs-node` can't start / `Address already in use` | host `openvswitch-switch` conflicts with the in-pod OVS | Step 1 — `systemctl disable --now openvswitch-switch` |
| Primary-UDN pod lands on the default network / admission `denied` | OVN-K release-1.2 identity webhook rejects primary-UDN pod annotation | Step 6 — `enableOvnKubeIdentity=false` (or newer OVN-K) |
| Controller crash-loops on a bare cluster | missing CSI VolumeSnapshot CRDs (pre-`#245` builds) | use a `#245`+ build, or install the external-snapshotter CRDs |
| `cluster-manager` "context deadline ... waiting for node zone" | it crashed earlier (usually the IPAMClaim CRD) and never stamped node zones | fix the missing CRD (Step 3), then `kubectl -n ovn-kubernetes delete pod -l name=ovnkube-control-plane` |
| Cross-node L2 fails | Geneve (UDP 6081) blocked between nodes | open UDP 6081 in any inter-node firewall |

## See also

- [`ovn-kubernetes-install.md`](ovn-kubernetes-install.md) — running IP-preserving
  guests on the cluster you just built.
- [`udn-multi-tenancy.md`](udn-multi-tenancy.md) — the per-tenant secondary-UDN recipe.
- [`multi-node-l2.md`](multi-node-l2.md) — the feature/CRD reference + validation matrix.
- [`ovn-l2-install.md`](ovn-l2-install.md) — the kube-ovn-non-primary sibling path
  (keep a different primary CNI like Calico/Cilium).

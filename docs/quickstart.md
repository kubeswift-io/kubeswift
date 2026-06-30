# Quickstart

This guide gets you from zero to a running VM in the shortest path. It covers disk boot (cloud image), kernel boot (microVM), and connecting to the guest. Windows guests follow the disk-boot flow with `osType: windows` — see the [Windows overview](windows/overview.md).

## Prerequisites

- Kubernetes cluster (kind, k3s, or bare metal — v1.28+)
- Nodes with `/dev/kvm` available (KVM-capable hardware)
- `kubectl` configured against the cluster
- Helm 3+ installed
- `swiftctl` binary (see below)

### Install swiftctl

```bash
go install github.com/kubeswift-io/kubeswift/cmd/swiftctl@latest
```

Or download from the GitHub release page.

### Verify KVM on nodes

```bash
kubectl get nodes -o custom-columns=NAME:.metadata.name,STATUS:.status.conditions[-1].type
# Then on each node:
ls -la /dev/kvm
```

## Step 1: Install KubeSwift

**From OCI Helm chart:**

```bash
helm install kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift \
  --version 0.6.0 \
  -n kubeswift-system \
  --create-namespace
```

**From source:**

```bash
git clone https://github.com/kubeswift-io/kubeswift.git
cd kubeswift
make build-images
make deploy
```

Verify the controller is running:

```bash
kubectl -n kubeswift-system get pods
# Expected: controller-manager pod Running
```

Verify CRDs are installed:

```bash
kubectl get crd | grep kubeswift.io
# Expected (12 CRDs): swiftguests, swiftguestclasses, swiftguestpools,
#   swiftimages, swiftseedprofiles, swiftkernels,
#   swiftgpuprofiles, swiftgpunodes,
#   swiftsnapshots, swiftrestores, swiftsnapshotschedules,
#   swiftmigrations
```

## Step 2: Boot a disk-boot VM (Ubuntu Noble)

### Create the resources

```bash
# Resource template (CPU, memory, disk)
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml

# Disk image from Ubuntu cloud images
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml

# SSH key injection via cloud-init
kubectl apply -f config/samples/swiftseedprofile-ssh.yaml

# The VM itself
kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml
```

### Wait for image import

Image import downloads the Ubuntu Noble cloud image, converts it from qcow2 to raw, and patches GRUB for serial console. This takes 5–15 minutes depending on network speed.

```bash
kubectl get swiftimage ubuntu-noble -w
```

Expected progression:

```
NAME           PHASE       AGE
ubuntu-noble   Pending     0s
ubuntu-noble   Importing   5s
ubuntu-noble   Validating  3m
ubuntu-noble   Preparing   5m
ubuntu-noble   Ready       8m
```

### Wait for the VM to start

```bash
kubectl get swiftguest sample -w
```

Expected progression:

```
NAME     PHASE       AGE
sample   Pending     0s
sample   Scheduling  2s
sample   Running     15s
```

When `status.phase=Running` and `status.network.primaryIP` is populated, the VM is fully up and reachable.

### Check status

```bash
swiftctl describe sample
```

Output:

```
Name:        sample
Namespace:   default
Phase:       Running
Node:        worker-1
RunPolicy:   Running

Spec:
  Image:       ubuntu-noble
  Kernel:      (none)
  GuestClass:  default
  SeedProfile: ssh

Runtime:
  Hypervisor:  cloud-hypervisor
  PID:         12345

Console:
  SerialSocket: /var/lib/kubeswift/run/default-sample/serial.sock

Network:
  PrimaryIP:   192.168.99.11
  Interfaces:
    - eth0: 192.168.99.11

Conditions:
  Resolved: True
  PodScheduled: True
  GuestRunning: True
```

## Step 3: Connect to the VM

### Serial console

```bash
swiftctl console sample
```

The terminal enters raw mode. You will see the Linux login prompt. Press **Ctrl+O** to detach (Ctrl+C is sent to the guest).

### SSH

```bash
swiftctl ssh sample -u kubeswift -i ~/.ssh/id_rsa
```

This execs into the launcher pod and connects to the guest IP via SSH. Requires:
- Guest must be Running with `status.network.primaryIP` populated
- Your SSH public key must be in the SwiftSeedProfile `ssh_authorized_keys`

To use your own SSH key, edit `config/samples/swiftseedprofile-ssh.yaml` and replace the `ssh_authorized_keys` entry before applying.

## Step 4: Boot a kernel-boot microVM

Kernel boot uses a direct bzImage + initramfs, skipping firmware, GRUB, and cloud-init. It is faster and requires no PVC.

### Label a node

The SwiftKernel controller only pulls artifacts to labeled nodes:

```bash
kubectl label node <node-name> kubeswift.io/kernel-node=true
```

### Create the SwiftKernel

```bash
kubectl apply -f config/samples/kernel-boot/swiftkernel-faas.yaml
kubectl get swiftkernel faas-minimal -w
```

The controller creates a pull job on each labeled node. Expected progression:

```
NAME           PROFILE        PHASE     AGE
faas-minimal   faas-minimal   Pending   0s
faas-minimal   faas-minimal   Pulling   2s
faas-minimal   faas-minimal   Ready     15s
```

### Create the VM

```bash
kubectl apply -f config/samples/kernel-boot/swiftguest-faas.yaml
kubectl get swiftguest faas-test -w
```

### Connect

```bash
swiftctl console faas-test
# Press Ctrl+O to detach
```

The faas-minimal guest runs a BusyBox shell. It gets a DHCP IP from the tap+bridge network.

## Step 5: Lifecycle operations

```bash
# Stop a VM (sets runPolicy=Stopped, sends SIGTERM to hypervisor)
swiftctl stop sample

# Start a stopped VM
swiftctl start sample

# Restart (delete pod — controller recreates it)
swiftctl restart sample

# Tail launcher logs
swiftctl logs sample -f

# Troubleshoot
swiftctl debug sample
swiftctl debug sample --shell   # interactive shell in launcher container
```

## Step 6: Run a VM fleet (SwiftGuestPool)

SwiftGuestPool manages a fleet of identical VMs with ReplicaSet semantics, rolling updates, topology spread, and per-replica PVCs.

```bash
kubectl apply -f config/samples/pool/swiftguestpool-basic.yaml
kubectl get sgpool basic-pool -w
```

Expected progression:

```
NAME         DESIRED   READY   UPDATED   AVAILABLE   FAILED   AGE
basic-pool   2         2       2         2                     60s
```

Scale the pool:

```bash
kubectl scale sgpool basic-pool --replicas=4
```

Check pool members:

```bash
kubectl get sg -l swift.kubeswift.io/pool=basic-pool
```

## Step 7: Add a secondary NIC

Secondary NICs connect VMs to additional networks (storage, VLANs, isolated segments) via Multus CNI.

```bash
# Install Multus (if not already installed)
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset-thick.yml

# Create a network
kubectl apply -f config/samples/multi-nic/nad-bridge.yaml

# Create a VM with two NICs
kubectl apply -f config/samples/multi-nic/swiftguest-multi-nic.yaml
```

The SwiftGuest `spec.interfaces` defines primary and secondary NICs:

```yaml
interfaces:
- name: mgmt
- name: data
  networkRef:
    name: my-network
```

See the [Networking Operations Guide](networking/operations-guide.md) for full setup
instructions covering physical networks, VLANs, bonds, and isolated networks.

## Cleanup

```bash
kubectl delete swiftguest sample faas-test
kubectl delete swiftguestpool basic-pool
kubectl delete swiftkernel faas-minimal
kubectl delete swiftimage ubuntu-noble
kubectl delete swiftseedprofile ssh
kubectl delete swiftguestclass default
```

## Smoke test

The automated smoke test verifies end-to-end boot and networking:

```bash
make smoke-test
```

Success criteria:
- SwiftImage reaches `phase=Ready`
- SwiftGuest reaches `phase=Running` with `GuestRunning=True`
- `status.network.primaryIP` is populated

---

## Status

**Working and cluster-validated:** Linux disk boot (CLOUDHV.fd) and kernel boot; Windows guests
(`osType: windows`, Cloud Hypervisor v52.0 — see [Windows overview](windows/overview.md)); networking
(tap+bridge+dnsmasq) and [service exposure](networking/service-exposure.md); SR-IOV NIC passthrough;
SwiftGuestPool (scaling, rolling updates, topology spread, PVC per replica); per-guest root disk cloning;
[snapshots and clones](snapshots/csi-snapshots.md) (CSI, local/S3 memory, scheduled, cloneFromSnapshot);
[offline and live migration](migration/overview.md) (optional mTLS, `kubectl drain` integration);
Tier-1 PCIe GPU passthrough on Cloud Hypervisor — native or [via DRA](gpu/dra-allocation.md);
swiftctl CLI; cloud-init; vhost-user/virtiofs; Prometheus metrics and Grafana dashboards across every
feature; security-hardened containers.

Cloud Hypervisor runs every workload above. QEMU is the secondary runtime reserved for HGX SXM (Tier 2/3)
GPU topologies. See the [roadmap](architecture.md#design-principles) and feature docs for detail.

**Hardware-gated (implemented or designed, awaiting hardware):** HGX Tier 2/3 GPU validation,
multi-NIC + SR-IOV hardware validation, SEV-SNP confidential VMs.

## CRD short names

```bash
kubectl get sg       # SwiftGuest
kubectl get sgc      # SwiftGuestClass
kubectl get sgpool   # SwiftGuestPool
kubectl get si       # SwiftImage
kubectl get ssp      # SwiftSeedProfile
kubectl get sk       # SwiftKernel
kubectl get sgp      # SwiftGPUProfile
kubectl get sgn      # SwiftGPUNode
kubectl get swiftsnapshots          # SwiftSnapshot
kubectl get swiftrestores           # SwiftRestore
kubectl get swiftsnapshotschedules  # SwiftSnapshotSchedule
kubectl get swiftmigrations         # SwiftMigration
```

## Documentation

| Topic | Document |
|-------|----------|
| Multi-NIC networking | [docs/multi-nic.md](multi-nic.md) |
| Networking operations | [docs/networking/operations-guide.md](networking/operations-guide.md) |
| SR-IOV passthrough | [docs/networking/sriov.md](networking/sriov.md) |
| OVN-Kubernetes | [docs/networking/ovn-kubernetes.md](networking/ovn-kubernetes.md) |
| VMware/Proxmox comparison | [docs/networking/virtualization-comparison.md](networking/virtualization-comparison.md) |
| SwiftGuestPool API | [docs/api/swiftguestpool.md](api/swiftguestpool.md) |
| SwiftGuestPool guide | [docs/swiftguestpool-guide.md](swiftguestpool-guide.md) |
| SwiftGuestPool use cases | [docs/swiftguestpool-use-cases.md](swiftguestpool-use-cases.md) |
| Security audit | [docs/security-audit.md](security-audit.md) |
| GPU passthrough | [docs/gpu-passthrough.md](gpu-passthrough.md) |
| CRD reference | [docs/crds.md](crds.md) |
| Architecture | [docs/architecture.md](architecture.md) |
| swiftctl CLI | [docs/swiftctl.md](swiftctl.md) |
| Development | [docs/development.md](development.md) |

## Next steps

- [Networking Operations Guide](networking/operations-guide.md) -- connect VMs to physical networks and VLANs
- [CRD reference](crds.md) -- full field documentation for all CRDs
- [GPU passthrough](gpu-passthrough.md) -- GPU workload setup
- [SwiftGuestPool Guide](swiftguestpool-guide.md) -- manage VM fleets
- [swiftctl reference](swiftctl.md) -- all CLI commands and flags
- [Architecture](architecture.md) -- how the system works

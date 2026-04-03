# Quickstart

This guide gets you from zero to a running VM in the shortest path. It covers disk boot (cloud image), kernel boot (microVM), and connecting to the guest.

## Prerequisites

- Kubernetes cluster (kind, k3s, or bare metal — v1.28+)
- Nodes with `/dev/kvm` available (KVM-capable hardware)
- `kubectl` configured against the cluster
- Helm 3+ installed
- `swiftctl` binary (see below)

### Install swiftctl

```bash
go install github.com/projectbeskar/kubeswift/cmd/swiftctl@latest
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
helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift \
  --version 0.1.0 \
  -n kubeswift-system \
  --create-namespace
```

**From source:**

```bash
git clone https://github.com/projectbeskar/kubeswift.git
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
# Expected: swiftguests, swiftimages, swiftkernels, swiftseedprofiles, swiftguestclasses, swiftgpuprofiles, swiftgpunodes
```

## Step 2: Boot a disk-boot VM (Ubuntu Focal)

### Create the resources

```bash
# Resource template (CPU, memory, disk)
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml

# Disk image from Ubuntu cloud images
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-focal.yaml

# SSH key injection via cloud-init
kubectl apply -f config/samples/swiftseedprofile-ssh.yaml

# The VM itself
kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml
```

### Wait for image import

Image import downloads the Ubuntu Focal cloud image, converts it from qcow2 to raw, and patches GRUB for serial console. This takes 5–15 minutes depending on network speed.

```bash
kubectl get swiftimage ubuntu-cloud -w
```

Expected progression:

```
NAME           PHASE       AGE
ubuntu-cloud   Pending     0s
ubuntu-cloud   Importing   5s
ubuntu-cloud   Validating  3m
ubuntu-cloud   Preparing   5m
ubuntu-cloud   Ready       8m
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
  Image:       ubuntu-cloud
  Kernel:      (none)
  GuestClass:  default
  SeedProfile: ssh

Runtime:
  Hypervisor:  cloud-hypervisor
  PID:         12345

Console:
  SerialSocket: /var/lib/kubeswift/run/default-sample/serial.sock

Network:
  PrimaryIP:   10.244.125.11
  Interfaces:
    - eth0: 10.244.125.11

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

## Cleanup

```bash
kubectl delete swiftguest sample faas-test
kubectl delete swiftkernel faas-minimal
kubectl delete swiftimage ubuntu-cloud
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

## Next steps

- [CRD reference](crds.md) — full field documentation for all 7 CRDs
- [GPU passthrough](gpu-passthrough.md) — GPU workload setup
- [swiftctl reference](swiftctl.md) — all CLI commands and flags
- [Architecture](architecture.md) — how the system works

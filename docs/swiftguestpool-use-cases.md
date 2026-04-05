# SwiftGuestPool Use Cases

Five complete use cases with full manifests. Each includes the problem statement, why SwiftGuestPool is the right tool, all prerequisite resources, the pool manifest, and operational commands.

For CRD field reference, see [api/swiftguestpool.md](api/swiftguestpool.md).
For operational guide, see [swiftguestpool-guide.md](swiftguestpool-guide.md).

---

## 1. GPU Inference Fleet

### Problem

You run a model-serving workload across multiple GPUs. Each VM needs one A100-PCIe GPU, an Ubuntu image with CUDA drivers, and a consistent seed profile. When you update the CUDA image, VMs must roll forward one at a time so the inference service stays available.

### Why SwiftGuestPool

- Declarative replica count tied to GPU availability
- Rolling updates replace one VM at a time, keeping the rest serving traffic
- `spreadPolicy: Spread` distributes VMs across GPU nodes for fault tolerance
- Labels on replicas enable service discovery and load balancer targeting

### Manifests

```yaml
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: gpu-inference
spec:
  cpu: "16"
  memory: "32Gi"
  rootDisk:
    size: "80Gi"
    format: raw
---
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-noble-cuda
  namespace: ml-inference
spec:
  source:
    http:
      url: https://images.example.com/ubuntu-noble-cuda-12.4.img
  format: qcow2
---
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: inference-seed
  namespace: ml-inference
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: inference
    users:
    - name: inference
      sudo: ALL=(ALL) NOPASSWD:ALL
      ssh_authorized_keys:
      - ssh-ed25519 AAAA...
    runcmd:
    - systemctl start inference-server
---
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata:
  name: a100-pcie-single
  namespace: ml-inference
spec:
  count: 1
  model: "A100-PCIe"
  tier: pcie
  partitionMode: isolated
  pcieTopology:
    gpuDirectClique: 0
  hugepages: "1Gi"
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: inference-fleet
  namespace: ml-inference
spec:
  replicas: 4
  spreadPolicy: Spread
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 0
  template:
    metadata:
      labels:
        workload: inference
        model: resnet50
    spec:
      imageRef:
        name: ubuntu-noble-cuda
      gpuProfileRef:
        name: a100-pcie-single
      guestClassRef:
        name: gpu-inference
      seedProfileRef:
        name: inference-seed
      runPolicy: Running
```

### Operational commands

```bash
# Deploy
kubectl apply -f inference-fleet/

# Watch rollout
kubectl get sgpool inference-fleet -n ml-inference -w

# Check GPU allocation
kubectl get sgn -o custom-columns=NAME:.metadata.name,FREE:.status.freeGPUs

# Verify GPUs inside a replica
swiftctl ssh inference-fleet-0 -n ml-inference -- nvidia-smi

# Scale to match available GPUs
kubectl scale sgpool inference-fleet -n ml-inference --replicas=8

# Update image (triggers rolling update)
kubectl patch sgpool inference-fleet -n ml-inference \
  --type merge -p '{"spec":{"template":{"spec":{"imageRef":{"name":"ubuntu-noble-cuda-v2"}}}}}'
```

---

## 2. CI/CD Runner Pool

### Problem

Your CI/CD system needs a pool of clean VMs as build runners. Each pipeline gets a fresh VM. VMs are ephemeral -- no persistent state. When the runner image is updated, all runners should be replaced simultaneously to avoid version skew in the build environment.

### Why SwiftGuestPool

- Recreate strategy replaces all runners at once -- no mixed versions
- Scale up/down on demand (peak hours vs overnight)
- No PVCs needed -- fully ephemeral
- Labels enable CI system integration (runner registration)

### Manifests

```yaml
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: ci-runner
spec:
  cpu: "4"
  memory: "8Gi"
  rootDisk:
    size: "40Gi"
    format: raw
---
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-noble-ci
  namespace: ci
spec:
  source:
    http:
      url: https://images.example.com/ubuntu-noble-ci-tools.img
  format: qcow2
---
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: ci-runner-seed
  namespace: ci
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: ci-runner
    users:
    - name: runner
      sudo: ALL=(ALL) NOPASSWD:ALL
      ssh_authorized_keys:
      - ssh-ed25519 AAAA...
    packages:
    - docker.io
    - git
    - make
    runcmd:
    - systemctl enable --now docker
    - /opt/register-runner.sh
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: ci-runners
  namespace: ci
spec:
  replicas: 8
  updateStrategy:
    type: Recreate
  spreadPolicy: Spread
  template:
    metadata:
      labels:
        role: ci-runner
        team: platform
    spec:
      imageRef:
        name: ubuntu-noble-ci
      guestClassRef:
        name: ci-runner
      seedProfileRef:
        name: ci-runner-seed
      runPolicy: Running
```

### Scaling pattern

```bash
# Morning ramp-up
kubectl scale sgpool ci-runners -n ci --replicas=16

# Evening scale-down
kubectl scale sgpool ci-runners -n ci --replicas=2

# Weekend: scale to zero
kubectl scale sgpool ci-runners -n ci --replicas=0
```

---

## 3. VDI / Lab Environments

### Problem

You provide virtual desktop or lab environments to users. Each user needs their own VM with a persistent home directory that survives image updates and reboots. VMs should be spread across nodes for performance isolation.

### Why SwiftGuestPool

- `volumeClaimTemplates` gives each replica a persistent PVC for the home directory
- PVCs survive rolling updates -- user data is preserved when the base image changes
- Stable naming (`lab-pool-0`, `lab-pool-1`, ...) enables predictable user assignment
- Spread policy prevents noisy-neighbor effects

### Manifests

```yaml
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: vdi-desktop
spec:
  cpu: "4"
  memory: "8Gi"
  rootDisk:
    size: "40Gi"
    format: raw
---
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-noble-desktop
  namespace: lab
spec:
  source:
    http:
      url: https://images.example.com/ubuntu-noble-desktop.img
  format: qcow2
---
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: vdi-seed
  namespace: lab
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: lab-desktop
    users:
    - name: student
      sudo: ALL=(ALL) NOPASSWD:ALL
      lock_passwd: false
      passwd: $6$rounds=4096$...
    runcmd:
    - mkdir -p /mnt/home
    - mount /dev/vdb /mnt/home
    - ln -sf /mnt/home /home/student/persistent
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: lab-pool
  namespace: lab
spec:
  replicas: 20
  spreadPolicy: Spread
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 2
      maxSurge: 0
  volumeClaimTemplates:
  - metadata:
      name: home
    spec:
      accessModes: ["ReadWriteOnce"]
      storageClassName: longhorn
      resources:
        requests:
          storage: 50Gi
  template:
    metadata:
      labels:
        env: lab
        type: desktop
    spec:
      imageRef:
        name: ubuntu-noble-desktop
      dataDiskRef:
        name: home
      guestClassRef:
        name: vdi-desktop
      seedProfileRef:
        name: vdi-seed
      runPolicy: Running
```

### Operational commands

```bash
# Check all desktops
kubectl get sg -n lab -l env=lab -o wide

# SSH into a specific desktop
swiftctl ssh lab-pool-3 -n lab --user student

# Check PVC usage
kubectl get pvc -n lab -l swift.kubeswift.io/pool=lab-pool

# Scale for next semester
kubectl scale sgpool lab-pool -n lab --replicas=40
```

---

## 4. Telco NFV Scale-Out

### Problem

A telco network function (firewall, load balancer, or DPI engine) needs to scale horizontally. Each VM requires a management NIC for control plane traffic and a secondary data-plane NIC on a dedicated VLAN for packet processing.

### Why SwiftGuestPool

- Multi-NIC support via `interfaces` and Multus NetworkAttachmentDefinitions
- Replicas scale with traffic demand
- Rolling updates deploy new network function versions without traffic blackout
- Spread policy distributes processing across nodes

### Manifests

```yaml
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: nfv-medium
spec:
  cpu: "8"
  memory: "16Gi"
  rootDisk:
    size: "20Gi"
    format: raw
---
apiVersion: "k8s.cni.cncf.io/v1"
kind: NetworkAttachmentDefinition
metadata:
  name: data-plane-vlan100
  namespace: nfv
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "type": "macvlan",
      "master": "eno2",
      "mode": "bridge",
      "ipam": {
        "type": "static",
        "addresses": [{"address": "10.100.0.0/24"}]
      }
    }
---
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-noble-nfv
  namespace: nfv
spec:
  source:
    http:
      url: https://images.example.com/ubuntu-noble-nfv.img
  format: qcow2
---
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: nfv-seed
  namespace: nfv
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: nfv-node
    users:
    - name: nfv
      sudo: ALL=(ALL) NOPASSWD:ALL
      ssh_authorized_keys:
      - ssh-ed25519 AAAA...
    runcmd:
    - /opt/configure-dataplane.sh eth1
    - systemctl start packet-processor
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: nfv-firewall
  namespace: nfv
spec:
  replicas: 6
  spreadPolicy: Spread
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 1
  template:
    metadata:
      labels:
        function: firewall
        tier: data-plane
    spec:
      imageRef:
        name: ubuntu-noble-nfv
      guestClassRef:
        name: nfv-medium
      seedProfileRef:
        name: nfv-seed
      interfaces:
      - name: mgmt
      - name: data
        networkRef:
          name: data-plane-vlan100
      runPolicy: Running
```

### Operational commands

```bash
# Check fleet status
kubectl get sgpool nfv-firewall -n nfv

# List all firewall replicas with IPs
kubectl get sg -n nfv -l function=firewall \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,IP:.status.network.primaryIP,NODE:.status.nodeName

# Scale with traffic
kubectl scale sgpool nfv-firewall -n nfv --replicas=12

# Rolling update to new NFV image
kubectl patch sgpool nfv-firewall -n nfv \
  --type merge -p '{"spec":{"template":{"spec":{"imageRef":{"name":"ubuntu-noble-nfv-v2"}}}}}'
```

---

## 5. Batch / HPC Compute

### Problem

A batch processing or HPC workload needs a burst of identical compute VMs. Workers are ephemeral -- they process jobs from a queue and can be discarded when the batch completes. Fast provisioning and teardown matter more than update strategy.

### Why SwiftGuestPool

- Scale to N workers instantly, scale to zero when done
- Recreate strategy is fine -- workers are stateless
- No PVCs needed -- shared storage accessed via NFS or object storage
- Labels integrate with job schedulers (Slurm, HTCondor)

### Manifests

```yaml
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: hpc-worker
spec:
  cpu: "8"
  memory: "32Gi"
  rootDisk:
    size: "20Gi"
    format: raw
---
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-noble-hpc
  namespace: batch
spec:
  source:
    http:
      url: https://images.example.com/ubuntu-noble-hpc-openmpi.img
  format: qcow2
---
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: hpc-seed
  namespace: batch
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: hpc-worker
    users:
    - name: compute
      sudo: ALL=(ALL) NOPASSWD:ALL
      ssh_authorized_keys:
      - ssh-ed25519 AAAA...
    mounts:
    - ["nfs-server:/shared", "/mnt/shared", "nfs", "defaults", "0", "0"]
    runcmd:
    - /opt/register-worker.sh
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: batch-workers
  namespace: batch
spec:
  replicas: 32
  updateStrategy:
    type: Recreate
  spreadPolicy: Spread
  template:
    metadata:
      labels:
        role: hpc-worker
        batch-id: job-20260404
    spec:
      imageRef:
        name: ubuntu-noble-hpc
      guestClassRef:
        name: hpc-worker
      seedProfileRef:
        name: hpc-seed
      runPolicy: Running
```

### Operational commands

```bash
# Launch batch
kubectl apply -f batch-workers/

# Monitor workers coming up
kubectl get sgpool batch-workers -n batch -w

# Check worker distribution
kubectl get sg -n batch -l role=hpc-worker \
  -o custom-columns=NAME:.metadata.name,NODE:.status.nodeName,PHASE:.status.phase

# Batch complete -- tear down
kubectl scale sgpool batch-workers -n batch --replicas=0

# Or delete the pool entirely
kubectl delete sgpool batch-workers -n batch
```

---

## See also

- [SwiftGuestPool API Reference](api/swiftguestpool.md)
- [SwiftGuestPool Operational Guide](swiftguestpool-guide.md)
- [Multi-NIC Support](multi-nic.md)
- [GPU Passthrough Guide](gpu-passthrough.md)

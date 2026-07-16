# Validating HGX shared-NVSwitch allocation without HGX hardware

KubeSwift's Tier-2 HGX path (`tier: hgx-shared`) needs 8-GPU SXM baseboards with
NVSwitch and Fabric Manager. Most clusters, including our dev cluster, have none.
This fixture lets you exercise the allocation and pod/intent-build logic against
a realistic HGX inventory anyway, and draws an honest line at what still needs
real hardware.

## What each layer needs

| Layer | Hardware-free? |
|---|---|
| FM partition membership -> device allocation coupling (`selectPartitionGPUs`, #405) | yes -- this fixture |
| Fabric Manager version gate (#407) | yes -- this fixture |
| GPU launcher pod + QEMU RuntimeIntent (root-port-per-GPU, NUMA, hugepages, pinning, #404) | yes -- `make verify-qemu-topology` + unit tests |
| gpu-discovery Module-ID -> Index translation (#406) | yes -- unit tests (`cmd/gpu-discovery`) |
| VFIO bind, BAR mapping, NVLink/NVSwitch, `fmpm -a` activation, NCCL | **no -- real HGX only** |

Everything above the line is covered without hardware. The last row is the
irreducible gate: it involves real PCI devices, the NVSwitch fabric, and the
Fabric Manager partition handshake with the guest driver.

## The fake node

`test/gpu/fake-hgx-h100-status.json` is an 8-GPU HGX H100 `SwiftGPUNode` status
taken verbatim from the NVIDIA HGX Shared NVSwitch GPU Passthrough guide
(WP-12736-002): GPU BDFs `0f/10/41/44/86/87/b8/bb`, 4 NVSwitches, and the real
Fabric Manager partition table (one 8-GPU, two 4-GPU halves, four pairs, eight
singles).

The partition `gpuIndices` are in **device-Index space** -- what gpu-discovery
emits *after* translating the FM physical/Module IDs (which do not follow lspci
order) via `nvidia-smi -q` (#406). The guide's Module-ID -> Index map is
`{1:4, 2:6, 3:5, 4:7, 5:0, 6:2, 7:1, 8:3}`; e.g. the 4-GPU partition 1 holds
Module IDs `1,2,3,4`, which is device indices `4,6,5,7` (the NUMA-1 half).

## Walkthrough

```bash
export KUBECONFIG=... # a cluster with KubeSwift installed

# 1. Inject the fake node.
./test/gpu/inject-fake-hgx-node.sh
#   -> NAME=fake-hgx-0 PHASE=Ready GPUS=8 FREE=8 MODEL=H100-SXM VFIO=true
#   -> prints the partition id -> device-index table

# 2. Create the tenants (two 4-GPU guests + a third + a bad-version one).
kubectl apply -f test/gpu/fake-hgx-workload.yaml

# 3. Read the allocation result. The SwiftGPU controller allocates as soon as
#    the profile + node resolve -- independent of image readiness -- so this
#    populates even though the launcher pod stays Pending (fake-hgx-0 is not a
#    real kubelet) and imageRef need not exist.
kubectl get swiftguest hgx-tenant-a -o jsonpath='{.status.gpu}{"\n"}'
kubectl get swiftguest hgx-tenant-b -o jsonpath='{.status.gpu}{"\n"}'
```

Expected (verified on-cluster, controller `sha-13291e4`):

- **`hgx-tenant-a`** gets four devices that are exactly one FM partition's
  members -- `["0000:86:00.0","0000:b8:00.0","0000:87:00.0","0000:bb:00.0"]`
  (partition 1, NUMA 1), `partitionId: 1`, `numaNodes: [1]`, `hypervisor: qemu`.
- **`hgx-tenant-b`** gets the **disjoint** other half:
  `["0000:0f:00.0","0000:41:00.0","0000:10:00.0","0000:44:00.0"]` (partition 2,
  NUMA 0). Two tenants, two partitions, no overlap -- the shared-NVSwitch
  invariant. The node's `fabricManager.partitions[1].allocatedTo` and `[2]` now
  name the two guests.
- **`hgx-tenant-c`** stays `GPUAllocated=False, reason=NoCapacity` (only 8 GPUs;
  both 4-GPU partitions are taken).

```bash
kubectl get swiftguest -o custom-columns=\
NAME:.metadata.name,GPUALLOC:'.status.conditions[?(@.type=="GPUAllocated")].status',REASON:'.status.conditions[?(@.type=="GPUAllocated")].reason',DEVICES:.status.gpu.devices
```

### The Fabric Manager version gate (#407) -- a separate step

No-capacity wins over a version mismatch when the node has no free GPUs (correct
precedence), so the version-mismatch surface only appears on a node **with**
capacity. Reset to a fresh 8-free node and apply one guest on the bad-version
profile:

```bash
kubectl delete -f test/gpu/fake-hgx-workload.yaml
./test/gpu/inject-fake-hgx-node.sh   # freeGPUs back to 8
kubectl apply -f - <<'EOF'
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata: { name: hgx-badver }
spec:
  imageRef: { name: ubuntu-noble }
  guestClassRef: { name: default }
  gpuProfileRef: { name: hgx-shared-4-badver }   # requiredVersion 999.99.99
EOF
kubectl get swiftguest hgx-badver \
  -o jsonpath='{.status.conditions[?(@.type=="GPUAllocated")].message}{"\n"}'
```

`hgx-badver` stays `GPUAllocated=False, reason=NoCapacity` with a message that
**names** the mismatch, not a silent allocation:

```
no GPU node has sufficient capacity: candidate GPU node(s) run a Fabric Manager
version that does not match profile.spec.fabricManager.requiredVersion="999.99.99"
(for shared NVSwitch mode the host Fabric Manager version must exactly match the
guest driver version)
```

```bash
# Clean up.
kubectl delete swiftguest hgx-badver --ignore-not-found
kubectl delete -f test/gpu/fake-hgx-workload.yaml --ignore-not-found
./test/gpu/inject-fake-hgx-node.sh remove
```

## What this does NOT prove

The launcher pod pins to `fake-hgx-0` and stays `Pending` -- there is no real
kubelet, no VFIO devices, no NVSwitch, no Fabric Manager. Boot, VFIO bind, BAR
mapping, `fmpm -a` partition activation, NVLink, and NCCL are only exercised on a
real HGX node. This fixture proves the Kubernetes-side allocation and intent are
correct so that, when hardware lands, the remaining gap is exactly the hardware.

## Deployment note for real HGX

gpu-discovery translates FM partition membership using `nvidia-smi -q` (#406).
The shipped gpu-discovery DaemonSet is drop-ALL with a read-only `/sys` and does
**not** ship `nvidia-smi`/`fmpm`. On a real HGX node you must provide them to the
discovery pod (host-mount or the NVIDIA container runtime); without `nvidia-smi`
the partition indices fall back to identity and discovery logs a prominent
warning.

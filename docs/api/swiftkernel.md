# SwiftKernel

SwiftKernel defines a **kernel + initramfs OCI artifact** for direct kernel boot. The controller pulls the artifact to every node labeled `kubeswift.io/kernel-node=true`. SwiftGuest references SwiftKernel via `kernelRef` instead of `imageRef`.

**API:** `kernel.kubeswift.io/v1alpha1` · **Short name:** `sk`

## Spec

| Field | Required | Description |
|-------|----------|-------------|
| `ociRef.image` | Yes | OCI artifact reference (e.g. `ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0`) |
| `ociRef.pullSecret` | No | Name of a Secret for private registry auth |
| `kernelCmdline` | No | Default kernel command line; SwiftGuest `kernelCmdline` overrides this |
| `profile` | No | Profile name for documentation (e.g. `faas-minimal`) |

```yaml
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata:
  name: faas-minimal
  namespace: default
spec:
  ociRef:
    image: ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0
  kernelCmdline: "console=ttyS0 root=/dev/ram0 rdinit=/init"
  profile: faas-minimal
```

## Phases

| Phase | Description |
|-------|-------------|
| **Pending** | No pull Jobs started, or no labeled nodes found |
| **Pulling** | Pull Jobs running on one or more nodes |
| **Ready** | All labeled nodes have pulled successfully |
| **Failed** | A pull Job failed on a node (check `status.conditions` for details) |

## Status

| Field | Description |
|-------|-------------|
| `phase` | Pending, Pulling, Ready, Failed |
| `conditions` | Ready (True/False), Failed (True with node context) |
| `nodeStatuses` | Per-node pull status: `[{nodeName, phase}]` |
| `kernelDigest` | (reserved) Digest of pulled kernel artifact |
| `initramfsDigest` | (reserved) Digest of pulled initramfs artifact |

The local path where artifacts land is deterministic and not stored in status: `/var/lib/kubeswift/kernels/<namespace>-<name>/`.

## Per-node pull

The SwiftKernel controller creates one pull Job per labeled node. Each Job:

- Uses the `ghcr.io/oras-project/oras:v1.3.1` container image
- Has `nodeSelector: {"kubeswift.io/kernel-node": "true", "kubernetes.io/hostname": "<nodeName>"}`
- Mounts `/var/lib/kubeswift/kernels` as a hostPath volume
- Runs `oras pull` into `/var/lib/kubeswift/kernels/<namespace>-<name>/`

When a new node is labeled, the controller starts a pull Job for it on the next reconcile.

## Conditions

| Condition | Status | Reason | Meaning |
|-----------|--------|--------|---------|
| Ready | True | Ready | Kernel artifacts ready on all nodes |
| Ready | False | NoKernelNodes | No nodes labeled `kubeswift.io/kernel-node=true` found |
| Ready | False | Pulling | Kernel artifacts are being pulled to nodes |
| Failed | True | PullFailed | Pull job failed on a node (message includes node name) |

## Operator workflow

1. Label nodes: `kubectl label node <name> kubeswift.io/kernel-node=true`
2. Create SwiftKernel.
3. Wait for `phase=Ready`: `kubectl get swiftkernel faas-minimal -w`
4. Create SwiftGuest with `kernelRef.name: faas-minimal`.

**If pull fails:** Check pull Job logs: `kubectl logs job/swiftkernel-pull-faas-minimal-<nodename>`. Common causes: OCI image not found, registry auth missing, node disk full.

[SwiftGuest](swiftguest.md) · [Full SwiftKernel reference](../swiftkernel.md) · [Kernel boot quickstart](../kernel-boot-quickstart.md)

# Kernel Boot Quickstart

This guide walks through booting a kernel-based microVM with KubeSwift using the faas-minimal profile. The full process takes under a minute once the OCI artifact pull completes.

## Prerequisites

- KubeSwift installed (CRDs, controller-manager, swiftletd image deployed)
- At least one worker node with `/dev/kvm`
- `kubectl` configured

## Step 1: Label a node

The SwiftKernel controller only pulls artifacts to nodes with the `kubeswift.io/kernel-node` label. Pick a worker node:

```bash
kubectl label node <node-name> kubeswift.io/kernel-node=true
```

Verify:

```bash
kubectl get nodes -l kubeswift.io/kernel-node=true
```

Expected output:

```
NAME          STATUS   ROLES    AGE   VERSION
<node-name>   Ready    <none>   10d   v1.31.0
```

## Step 2: Create SwiftGuestClass

If you already have a `default` SwiftGuestClass, skip this step:

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
```

## Step 3: Apply SwiftKernel

```bash
kubectl apply -f config/samples/kernel-boot/swiftkernel-faas.yaml
```

This creates the `faas-minimal` SwiftKernel. The controller starts a pull Job on each labeled node:

```bash
kubectl get swiftkernel faas-minimal -w
```

Expected progression:

```
NAME           PROFILE        PHASE     AGE
faas-minimal   faas-minimal   Pending   0s
faas-minimal   faas-minimal   Pulling   2s
faas-minimal   faas-minimal   Ready     15s
```

If the phase stays Pending, verify that at least one node has the `kubeswift.io/kernel-node=true` label.

To check pull Job progress:

```bash
kubectl get jobs -l app.kubernetes.io/managed-by=swiftkernel
```

## Step 4: Apply SwiftGuest with kernelRef

```bash
kubectl apply -f config/samples/kernel-boot/swiftguest-faas.yaml
```

The sample creates a SwiftGuest with `kernelRef` pointing to the `faas-minimal` SwiftKernel:

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: faas-test
  namespace: default
spec:
  kernelRef:
    name: faas-minimal
  guestClassRef:
    name: default
  kernelCmdline: "console=ttyS0 root=/dev/ram0 rdinit=/init"
  runPolicy: Running
```

Watch the guest come up:

```bash
kubectl get swiftguest faas-test -w
```

Expected progression:

```
NAME        PHASE        AGE
faas-test   Scheduling   0s
faas-test   Running      5s
```

## Step 5: Connect to console

Once the guest is Running, attach to the serial console:

```bash
swiftctl console faas-test
```

You should see the faas-minimal init output:

```
KubeSwift faas-minimal ready
kernel: 6.6.44
/ #
```

Press **Ctrl+O** to detach from the console.

## Step 6: Clean up

```bash
kubectl delete swiftguest faas-test
kubectl delete swiftkernel faas-minimal
```

## Troubleshooting

**SwiftKernel stays Pending:** No nodes have the `kubeswift.io/kernel-node=true` label. Label a node and wait for the next reconcile.

**SwiftKernel stays Pulling:** The pull Job is still running. Check Job logs:

```bash
kubectl logs job/swiftkernel-pull-faas-minimal-<nodename>
```

Common causes: OCI image not found, registry authentication required (set `spec.ociRef.pullSecret`), slow network.

**SwiftGuest stays Scheduling:** The pod cannot be scheduled. Check pod events:

```bash
kubectl describe pod faas-test
```

The pod requires a node with both `kubeswift.io/kernel-node=true` and sufficient resources.

**Kernel panic in guest:** The kernel command line is wrong or the initramfs `/init` is missing. Verify with `swiftctl debug faas-test` to inspect the Cloud Hypervisor arguments.

[SwiftKernel reference](swiftkernel.md) · [SwiftKernel API](api/swiftkernel.md) · [Architecture](architecture.md)

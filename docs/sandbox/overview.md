# Running sandboxes

KubeSwift runs ephemeral, strongly-isolated microVMs that boot an **OCI image
as the VM root filesystem** — a `SwiftSandbox`. This page is the operator
entry point; the sample manifests are linked inline.

> **Status: cluster-validated end-to-end (2026-07-11).** An alpine microVM
> boots, runs its workload to a terminal `Completed`/`Failed` phase, and
> supports interactive exec/attach over vsock — validated on both `restricted`
> and `open` network modes. First ships in **v0.9.0**.

## What it is

SwiftSandbox is a third boot mode alongside SwiftGuest's disk boot and kernel
boot: a direct-kernel boot + a read-only ext4 built from an OCI image + a
tmpfs overlay + a bridge-initramfs that execs the image's entrypoint (the
Firecracker/Kata model — not SwiftGuest's qcow2-disk pipeline). A SwiftSandbox
is ephemeral: it runs the workload to completion and holds no PVC. Once
terminal, it stays around for inspection until deleted (or `spec.ttl` cleans
it up).

## When to use it

- CI runners — a clean, disposable VM per job
- AI-agent / code-interpreter execution — a real kernel boundary around
  generated or untrusted code, not just a container
- Serverless / short-lived compute
- Untrusted code and security research

For bursts of same-image sandboxes where the ~15s cold boot dominates, a
[warm pool](warm-pool.md) keeps pre-booted slots ready for sub-second checkout.

## Prerequisites

- A node labeled `kubeswift.io/kernel-node=true`
- A `Ready` `SwiftKernel` named `sandbox` (OCI artifact
  `ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.11`, pulled per node)

The sandbox kernel is not a plain `kernelRef` SwiftGuest kernel — its
bridge-initramfs needs the OCI rootfs disk that the SwiftSandbox controller
supplies at launch, so it only boots as a SwiftSandbox.

## Quickstart

```bash
kubectl apply -f config/samples/sandbox/swiftkernel-sandbox.yaml
kubectl get swiftkernel sandbox -w        # wait for Ready

kubectl apply -f config/samples/sandbox/swiftsandbox.yaml
kubectl get sbox -w                       # Pending -> Materializing -> Running -> Completed
```

Ready-to-edit manifests and notes:
[`config/samples/sandbox/`](../../config/samples/sandbox/).

## CRD field reference

### Spec

| Field | Type | Default | Description |
|---|---|---|---|
| `image` | string | — | OCI image to run as the root filesystem. Required. A digest reference (`repo@sha256:...`) is preferred for reproducibility; a tag is accepted. |
| `imagePullSecret` | string | — | docker-registry Secret (same namespace) for pulling `image` from a private registry. |
| `verifyKeySecretRef.name` | string | — | Secret (same namespace) holding a cosign public key at key `cosign.pub`. When set, the image is cosign-verified before it materializes; an unsigned or tampered image fails and never boots. See [Signed images](#signed-images-verify-before-boot). |
| `cpu` | int32 | `1` | vCPU count. |
| `memory` | Quantity | `512Mi` | Guest RAM. |
| `command` | []string | — | Overrides the image's ENTRYPOINT. Empty uses the image's own Entrypoint+Cmd. |
| `args` | []string | — | Appended to `command` (or the image's CMD when `command` is empty). |
| `env` | []EnvVar | — | Merged over the image's config env. |
| `workingDir` | string | — | Overrides the image's working directory. Honored on both the cold-boot path (the bridge runs the workload via the guest agent's chroot+chdir+exec) and the warm-pool checkout path. Must exist in the image. |
| `timeout` | Duration | none | Wall-clock run cap. Past `startedAt + timeout` the controller force-terminates the sandbox to `Failed` (`DeadlineExceeded`). |
| `ttl` | Duration | none | Once the sandbox has been terminal (`Completed`/`Failed`) for at least `ttl`, the controller deletes it and frees the node's rootfs-cache reference. |
| `rootfsMode` | enum | `block` | How the OCI rootfs is delivered: `block` (read-only ext4 disk) or `virtiofs` (the unpacked tree over virtio-fs, tag `sandboxroot` — skips `mkfs.ext4` and the ext4 size floor, shares the host page cache). Same RO-base + writable tmpfs-overlay either way. |
| `network.mode` | enum | `restricted` | `restricted`, `open`, or `none`. See [Network modes](#network-modes). |
| `kernelProfileRef.name` | string | `sandbox` | SwiftKernel to boot. |
| `nodeSelector` | map[string]string | — | Additional node constraints, merged with the required `kubeswift.io/kernel-node=true`. |

Everything in `spec` except `ttl` is immutable after create — recreate the
sandbox to change image, resources, command, or network.

### Status

| Field | Type | Description |
|---|---|---|
| `phase` | enum | `Pending`, `Materializing`, `Running`, `Completed`, `Failed`. |
| `conditions[]` | []Condition | `Resolved`, `RootfsReady`, `GuestRunning`. |
| `nodeName` | string | Node running the sandbox. |
| `podRef` | string | Launcher pod name. |
| `rootfs.digest` | string | Resolved image digest (`sha256:...`). |
| `rootfs.sizeBytes` | int64 | Materialized ext4 size. |
| `rootfs.cachePath` | string | Node-local rootfs artifact path. |
| `runtime.pid` | int64 | Hypervisor process PID (reported by swiftletd). |
| `runtime.hypervisor` | string | Always `cloud-hypervisor`. |
| `network.primaryIP` | string | Guest DHCP IP. Absent for `network.mode: none`. |
| `startedAt` | Time | When the guest began running. |
| `terminalAt` | Time | When the sandbox first reached `Completed`/`Failed` — the anchor for `spec.ttl`. |
| `exitCode` | int32 | The workload's real exit code: `0` → `Completed`, non-zero → `Failed`. |
| `message` | string | Human-readable status detail. |

`kubectl get sbox` prints Phase, Image, Node, IP, and Age.

## Network modes

| Mode | Ingress | Egress | Use for |
|---|---|---|---|
| `restricted` (default) | Denied — nothing reaches the sandbox | DNS and the public internet are allowed; `169.254.0.0/16` (cloud metadata) and RFC1918 cluster/pod/service CIDRs are blocked | Untrusted code |
| `open` | Denied | Unrestricted — the whole cluster and internet | Trusted workloads that must reach in-cluster services |
| `none` | No network at all | No network at all | Pure compute / detonation |

Ingress isolation is a NetworkPolicy shared by `restricted` and `open` (deny
all inbound). The `restricted` vs `open` difference is entirely **in-pod
iptables** on the VM's forwarded traffic, not a NetworkPolicy — a
NetworkPolicy that blocked cluster egress would also cut swiftletd's own
status reporting, since the VM's traffic and swiftletd's apiserver calls
share the pod IP after MASQUERADE.

A networked sandbox resolves cluster service names and external names alike
— the controller injects the namespace's search domains and `ndots:5`.
`restricted` still blocks *connecting* to cluster IPs; name resolution and
egress reachability are separate concerns.

## Signed images (verify before boot)

Set `spec.verifyKeySecretRef.name` to a Secret holding a cosign public key
(key `cosign.pub`) to require a valid signature before the sandbox boots:

```bash
cosign generate-key-pair                 # cosign.pub + cosign.key
cosign sign --key cosign.key <registry>/<image>@sha256:...
kubectl create secret generic cosign-pub --from-file=cosign.pub
```

```yaml
spec:
  image: <registry>/<image>@sha256:...
  verifyKeySecretRef:
    name: cosign-pub
```

The `sandbox-materialize` init container resolves the image digest and runs
`cosign verify <repo>@<digest>` against the key **before** it materializes a
single layer. A missing or invalid signature fails that init container, so the
sandbox goes `Failed` and never runs an unverified rootfs. cosign speaks HTTPS
only — a signed image must come from a TLS registry.

A `SwiftSandboxPool` takes the same `spec.verifyKeySecretRef`; every warm slot
is verified, so a pool never warms an unverified image. This mirrors
`SwiftImage`'s `spec.source.oci.verifyKeySecretRef` for golden VM disks.

## Interacting with a sandbox

```bash
swiftctl sandbox logs <name> [-f]
swiftctl sandbox exec <name> [-e KEY=VALUE] [-w DIR] [-i] [-t] -- <cmd> [args...]
swiftctl sandbox attach <name> [-- <cmd>]
```

- `logs` streams the workload's console (`-f` to follow).
- `exec` runs a command inside the sandbox's OCI rootfs over a host↔guest
  vsock channel (an in-guest agent). stdout/stderr stream back live and the
  command's exit code is propagated. `-e` (repeatable) sets environment
  variables, `-w` sets the working directory, `-i` forwards stdin, `-t`
  allocates an interactive TTY.
- `attach` is shorthand for `exec -it -- /bin/sh` (pass `-- <cmd>` to run
  something else). Terminal resizes propagate; exit with Ctrl-D or `exit`.

## Lifecycle

`Pending` (resolving the image and kernel profile) → `Materializing` (the
rootfs init container builds the ext4) → `Running` (guest up) → `Completed`
(workload exited `0`) or `Failed` (boot/materialize failure, non-zero exit,
or `spec.timeout` exceeded).

`status.exitCode` carries the workload's real exit code. `spec.timeout`
bounds a runaway run; `spec.ttl` cleans up a finished sandbox's record once
you're done inspecting it.

## Troubleshooting

- **Stuck `Materializing`** — check `kubectl describe pod <name>`. Unscheduled
  usually means no node carries `kubeswift.io/kernel-node=true`, or the
  `sandbox` SwiftKernel hasn't finished pulling to that node yet
  (`kubectl get swiftkernel sandbox`). A failing `sandbox-materialize` init
  container usually means the image pull failed — check `spec.image` and
  `spec.imagePullSecret` and inspect the init container's logs.
- **`tty` reports "not a tty" inside an attached session** — a chroot/devpts
  cosmetic quirk. The session is a real TTY; shells, `vi`, and `top` all
  behave interactively.
- **No `status.network.primaryIP`** — expected for `network.mode: none`; by
  design there is no network to report.

## See also

- [Warm pools (fast start)](warm-pool.md) — pre-booted slots for sub-second checkout
- [`config/samples/sandbox/`](../../config/samples/sandbox/) — sample manifests and notes
- [swiftctl reference](../swiftctl.md)
- [SwiftKernel reference](../swiftkernel.md)

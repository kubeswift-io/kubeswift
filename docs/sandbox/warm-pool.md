# Warm pools (fast start)

A `SwiftSandboxPool` keeps N pre-booted, workload-less microVMs ready for one
image. A `SwiftSandbox` that points at the pool (`spec.poolRef`) then **checks
out** a ready slot instead of paying the cold materialize + boot (~15s); see
[How checkout works](#how-checkout-works) for measured times. This page assumes
you've read
[Running sandboxes](overview.md).

> **Status: cluster-validated (2026-07-12).** Checkout claims a warm slot and
> runs the workload to `Completed`/`Failed`; the consumed slot is destroyed and
> the pool replenishes a fresh one; a miss falls back to a cold boot. First
> ships in **v0.10.0**.

## How checkout works

- The pool boots `minWarm` **idle** slots — each an independent microVM of the
  pool image with no workload, waiting. The slot idles in the boot bridge (not
  the image), so **distroless / shell-less images can be pooled**.
- A `SwiftSandbox` with `spec.poolRef` **claims** one warm slot and injects its
  `command`/`args`/`env` into the already-booted VM over vsock. The VM is
  running, so the workload starts immediately.
- **Dispatch.** The controller writes the workload onto the claimed slot's pod,
  and swiftletd in the slot watches its own pod, so it starts the workload as
  soon as the API server delivers that write. Measured on a three-node lab
  cluster over 50 checkouts (1 vCPU, 256Mi slots, a no-op command), from the
  slot being claimed: the workload dispatched at p50 49 ms and p95 78 ms, and
  completed at p50 74 ms and p95 108 ms. Earlier releases read the pod every
  2 s instead, which added 0 to 2 s to every checkout (dispatch at p50 903 ms
  and p95 1911 ms on the same cluster). If swiftletd cannot watch its pod (for
  example a launcher Role created by an older controller), it reads the pod
  every 2 s as before; its log then shows `action_loop_watch_unavailable`.
- **Consume-and-replenish.** A claimed slot is never returned to the pool — one
  workload never inherits another's slot. On checkout the pool boots a fresh
  warm slot to restore the count.
- **Cold fallback.** If no warm slot is free (or the sandbox has no `command`,
  so the image entrypoint must be resolved, or asks for a shape the pool's
  slots don't have — see [Checking out](#checking-out-from-a-pool)), the sandbox boots cold
  automatically. A checkout never *fails* just because the pool is empty; it
  just doesn't get the speedup.

Each warm slot is an independent boot, so there is no shared state or identity
collision between a slot and the workload that lands in it — no identity agent
is involved.

## When to use it

- Bursty arrival of **same-image** sandboxes — CI fan-out, an agent running
  many steps — where the ~15s cold-boot latency dominates the actual work.
- Not worth it for one-off or heterogeneous-image sandboxes: a pool only speeds
  up checkouts of *its* image.

## Prerequisites

Same as a plain sandbox — a `kubeswift.io/kernel-node=true` node and a `Ready`
`sandbox` `SwiftKernel`. See [Running sandboxes › Prerequisites](overview.md#prerequisites).

**A warm GPU pool** (`spec.gpuProfileRef` set) additionally needs a node that is
**both** `kubeswift.io/gpu-node=true` **and** `kubeswift.io/kernel-node=true`
(with `vfio-pci` loadable and `gpu-discovery` running), the `gpu-sandbox`
SwiftKernel instead of the base `sandbox` one, and a `SwiftGPUProfile` with
**`tier: pcie`** — a warm slot boots mode-3 (Cloud Hypervisor direct-kernel), so
`hgx-shared`/`hgx-full` are rejected. See [GPU sandboxes › Warm GPU pools](gpu-sandboxes.md#warm-gpu-pools-sub-second-inference-start).

## Quickstart

```bash
kubectl apply -f config/samples/sandbox/swiftsandboxpool.yaml
kubectl get sboxpool -w        # Pending -> Warming -> Ready (warmReplicas == minWarm)

# check out: a sandbox that references the pool
kubectl apply -f config/samples/sandbox/swiftsandbox-pooled.yaml
kubectl get sbox pooled-echo -w   # Running (checked out) -> Completed, in ~1-3s
```

`kubectl describe sbox pooled-echo` shows a `CheckedOut` event naming the slot
it claimed (a pool miss shows `PoolColdFallback` instead).

Ready-to-edit manifests: [`config/samples/sandbox/`](../../config/samples/sandbox/).

## CRD field reference

### Spec

| Field | Type | Default | Description |
|---|---|---|---|
| `image` | string | — | OCI image every slot boots. Required. All slots share one materialized rootfs. Digest reference preferred. |
| `imagePullSecret` | string | — | docker-registry Secret (same namespace) for a private `image` (and for `model.imageRef`, if set). |
| `verifyKeySecretRef.name` | string | — | Secret (same namespace) holding a cosign public key (`cosign.pub`). Every warm slot is cosign-verified before it materializes. |
| `rootfsMode` | enum | `block` | How each slot's OCI rootfs is delivered: `block` (read-only ext4 disk) or `virtiofs` (unpacked tree over virtio-fs). A claiming `SwiftSandbox` must request the same mode. |
| `cpu` | int32 | `1` | vCPUs per slot. |
| `memory` | Quantity | `512Mi` | RAM per slot. Held per warm slot — a pool of N slots holds N × `memory` idle. |
| `minWarm` | int32 | `1` | Warm slots to keep ready — the warm buffer the pool maintains. This is the scale-subresource target (`kubectl scale sboxpool`); see [Scaling](#scaling). |
| `maxWarm` | int32 | — | Cap on warm slots. The effective cap is `max(maxWarm, minWarm)` — set below `minWarm` and `minWarm` wins. |
| `network.mode` | enum | `restricted` | `restricted`, `open`, or `none` — same semantics as [SwiftSandbox](overview.md#network-modes). Applies to every slot. |
| `network.ports[]` / `network.ingress` | list / object | — | Ports every slot exposes, as on [SwiftSandbox](overview.md#exposing-ports). |
| `network.egress.allow[]` | list | — | Destinations every slot may reach under `restricted`, as on [SwiftSandbox](overview.md#allowing-specific-destinations-under-restricted). Services are resolved on every pool pass: a warm slot allowing an address its Service no longer has is replaced, and while a Service is missing the pool warms nothing (`Degraded`, reason naming it). |
| `kernelProfileRef.name` | string | `sandbox` (`gpu-sandbox` when `gpuProfileRef` is set) | SwiftKernel the slots boot. |
| `nodeSelector` | map[string]string | — | Extra node constraints, merged with the required `kubeswift.io/kernel-node=true`. |
| `gpuProfileRef.name` | string | — | Makes this a **warm GPU pool**: every slot holds a native SwiftGPU allocation against this `SwiftGPUProfile`, pre-booted. Trades an idle GPU per slot for latency — size `minWarm` ≤ your free GPU count. `tier: pcie` only; `hgx-shared`/`hgx-full` are rejected. See [GPU sandboxes › Warm GPU pools](gpu-sandboxes.md#warm-gpu-pools-sub-second-inference-start). |
| `model.imageRef` / `model.mountPath` | string / string (default `/model`) | — | Preloads a read-only, node-shared model artifact into every slot over virtio-fs (digest-keyed cache, cosign-verifiable). See [GPU sandboxes › Model preload](gpu-sandboxes.md#model-preload). |

### Status

| Field | Type | Description |
|---|---|---|
| `phase` | enum | `Pending` (resolving image/kernel), `Warming` (bringing slots up toward `minWarm`), `Ready` (buffer at target), `Degraded` (cannot reach `minWarm` — e.g. no schedulable node, or the slots' SwiftKernel is missing or not `Ready`: `Resolved=False` with reason `KernelNotFound` or `KernelNotReady`, and no slot is created until it is). |
| `warmReplicas` | int32 | Ready, unclaimed slots right now. |
| `claimedReplicas` | int32 | Slots currently checked out (each owned by its SwiftSandbox). |
| `rootfs.digest` / `rootfs.cachePath` | string | The shared materialized rootfs. |
| `conditions[]` | []Condition | `Resolved`, `Warm`. |
| `message` | string | Human-readable detail. |

`kubectl get sboxpool` prints Image, MinWarm, Warm, Claimed, Phase, Age.

## Checking out from a pool

Set `spec.poolRef.name` on a `SwiftSandbox` (same namespace as the pool). The
sandbox's `command`/`args`/`env` are what gets injected into the claimed slot:

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata:
  name: pooled-echo
spec:
  image: docker.io/library/alpine:3.20   # must match the pool's image
  poolRef:
    name: alpine-pool
  command: ["sh", "-c"]
  args: ["echo hello from a warm slot"]
```

- A slot is only handed to a sandbox it honors: the slot has already booted,
  and a checkout only injects a command. The sandbox's `image`, `network.mode`,
  `network.egress.allow`, `network.ports`, `network.ingress`, `verifyKeySecretRef`, `rootfsMode`, kernel, `cpu` and `memory` must equal the
  pool's, and every label in its `nodeSelector` must be one the pool's
  `nodeSelector` requires too. GPU and model belong to the slot: a pooled
  sandbox sets neither and inherits the pool's, including a GPU pool's
  `gpu-sandbox` kernel when it sets no `kernelProfileRef`. A sandbox with its
  own GPU, a different model or a `scratchDisk` asks for something no slot
  has. `artifacts` are not part of the shape: see
  [Artifacts on checkout](#artifacts-on-checkout). Anything else boots cold with its own settings, and the
  `PoolColdFallback` event names each difference, for example
  `cpu (pool 1, sandbox 2)`. Warm slots booted before a pool edit to any of
  these fields are replaced.
- `podMetadata` is applied to the slot's pod in the same write that claims
  it, so a Service selecting the sandbox by those labels picks the slot up only
  once it is that sandbox's. Warm slots carry none.
- Probes are not part of the slot shape: a checkout hands its
  `readinessProbe`/`livenessProbe` to the slot with the workload, and probing
  starts when the workload does. A slot of a pool with ports shows as not Ready
  while warm; its readiness gate is set once a sandbox claims it.
- The workload **must** have a `command` to check out — with no command the
  image entrypoint has to be resolved, which only the cold path knows, so a
  command-less pooled sandbox cold-falls-back.
- `env` **and** `workingDir` are honored on a checkout — the workload is
  injected over the vsock exec channel. The pool image's own config env is
  merged in too (resolved once at materialize, no per-checkout pull), so the
  workload sees **image env + your `spec.env`**, same as a cold sandbox.
- `status.podRef` points at the claimed slot pod (`<pool>-slot-<x>`), not at a
  pod named after the sandbox. `status.exitCode` carries the workload's real
  exit code just like a cold sandbox.
- `swiftctl sandbox logs`/`exec`/`attach <name>` work on a checked-out sandbox
  — they target the claimed slot transparently (via `status.podRef`).

## Artifacts on checkout

A sandbox with `spec.artifacts` takes a warm slot like any other: pools stay
generic, a slot carries no artifact, and the VM is not rebooted. Needs a slot
kernel whose bridge has the `warm-mounts` feature (`kernels/sandbox:6.6.15`,
`kernels/gpu-sandbox:6.6.4` or later); a slot without it is not claimed for
artifacts, and the sandbox boots cold (the `PoolColdFallback` event says so).

**Lifecycle.**

1. Before claiming a slot, the controller resolves each `ref` to a digest
   with the sandbox's own pull Secret (`pullSecretRef`, else
   `imagePullSecret`). That request is the authorization check; no slot is
   held while it runs. `status.artifacts` records the digests.
2. The controller claims a slot and writes the workload with each artifact's
   digest, layout, mount path, and the SHA-256 fingerprint of its cosign key.
3. swiftletd, in the slot, finds each digest in the node's artifact cache and
   binds it read-only into the slot's staging share, a read-only virtio-fs
   share every warm slot boots with, empty.
4. The guest agent binds each artifact read-only at its `mountPath` in the
   sandbox root, then starts the workload. One request, no polling.

**Cache.** The node cache (`/var/lib/kubeswift/sandbox-artifacts`) is shared
by every sandbox on the node and keyed by digest. Entries are written only by
the materializer (a privileged init container, or a fetch pod), published by
an atomic rename, and readable by every guest user but writable by none
(directories 0555, files 0444). Launchers mount the cache read-only.

**A cache miss.** When a digest is not on the slot's node, or not yet verified
with the sandbox's key there, swiftletd reports `ArtifactMissing`. The
controller runs one fetch pod on that node, `<sandbox>-artifacts`: the same
materializer, pull Secret and key as a cold sandbox's init containers. It
then dispatches the workload again. The slot stays booted throughout. A miss
costs a pod start and the pull; a hit costs no pod.

**Verification.** With `verifyKeySecretRef`, the materializer cosign-checks
the digest with that key before caching it, and records the check next to
the entry, named by the SHA-256 of the key bytes it used. A signature over an
immutable digest with a given key is a fixed fact, so a checkout needs that
record for exactly its own key and runs no cosign. An entry another tenant
verified with another key does not count: it is a miss, and the fetch pod
verifies with this sandbox's key. A cold sandbox still verifies on every
start.

**Authorization.** Every checkout is authorized with the checking-out
sandbox's own credentials; that an entry is already on the node grants
nothing. The controller keeps a successful answer for a digest reference
(`repo@sha256:...`) for 5 minutes, per set of credentials, so a checkout
within that time asks no registry. A tag is re-resolved after 30 seconds: it
can move. Revoking credentials at the registry takes up to 5 minutes to stop
new sandboxes from checking out a digest those credentials could read. The
cache is in memory: a controller restart asks the registry again.

**Failure conditions.**

| Event | Result |
|---|---|
| Registry refuses the ref (400, 401, 403, 404) or the pull Secret is missing | `Failed`, `ArtifactResolveFailed`; no slot claimed |
| Registry unreachable or timing out | `Pending`, `RegistryUnavailable`, retried with backoff; no slot claimed |
| Fetch fails (pull, signature, size limit) | `Failed`, `ArtifactMaterializeFailed`, with the materializer's message |
| Missing again after a fetch | `Failed`, `ArtifactMissing` |
| The slot's kernel lacks `warm-mounts` (found at dispatch) | `Failed`, `KernelUnsupported` |

**Security boundary.** The guest sees only the staging share, and in its
sandbox root only its own artifacts. Everything is read-only three times:
the cache mount, the bind into the staging share, and the guest's bind
(also `nosuid`, `nodev`); virtiofsd serves the share with `--readonly`.
Registry credentials reach only the controller and the fetch pod, never the
slot or the guest. An unpacked artifact is capped at 10 GiB extracted, an OCI
layout at 10 GiB of blobs (`sandbox-materialize --max-bytes`).

## Node placement

Warm slots carry a soft topology-spread constraint (`MaxSkew: 1` over
`kubernetes.io/hostname`, `ScheduleAnyway`), so they land one-per-node across
the kernel-nodes rather than piling onto one. Warming is node-local (the rootfs
materializes on the slot's node), so a checkout that lands on any node is more
likely to find a warm slot *there*. The constraint is soft — warming never
blocks just because one node is momentarily full.

## Scaling

The warm buffer is a scale subresource on `spec.minWarm`, so it scales like a
`SwiftGuestPool`:

```bash
kubectl scale sboxpool <name> --replicas=10   # sets minWarm
```

and an HPA can target the pool. The natural signal is the checkout cold-fallback
rate (`kubeswift_sandbox_checkouts_total{result="cold"}`): grow the buffer when
checkouts miss, shrink when quiet. An HPA with `minReplicas: 0` drains a quiet
pool to zero and re-warms on demand — that is how you get scale-to-zero on a
quiet pool.

## Observability

`kubeswift_sandbox_checkouts_total{result}` counts checkouts by outcome:
`hit` (claimed a warm slot — the fast path) vs `cold` (no warm slot, or no
command — fell back to a cold boot). The **hit ratio is the pool's headline
signal**: a persistent `cold` rate means `minWarm` is too low for the arrival
rate. Pool health is `kubectl get sboxpool` (phase + `warmReplicas`).

## Limitations (v1)

- Keep the pooled sandbox's `image` the same as the pool's `image`; the checkout
  runs your command inside the slot's already-booted rootfs, so a different
  `image` on the sandbox is ignored for a hit (and only applies if it
  cold-falls-back).

## See also

- [Running sandboxes](overview.md) — the SwiftSandbox operator guide
- [`config/samples/sandbox/`](../../config/samples/sandbox/) — sample manifests
- [swiftctl reference](../swiftctl.md)

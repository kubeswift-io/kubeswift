# Warm pools (fast start)

A `SwiftSandboxPool` keeps N pre-booted, workload-less microVMs ready for one
image. A `SwiftSandbox` that points at the pool (`spec.poolRef`) then **checks
out** a ready slot in sub-second time instead of paying the cold
materialize + boot (~15s). This page assumes you've read
[Running sandboxes](overview.md).

> **Status: cluster-validated (2026-07-12).** Checkout claims a warm slot and
> runs the workload to `Completed`/`Failed`; the consumed slot is destroyed and
> the pool replenishes a fresh one; a miss falls back to a cold boot. First
> ships in **v0.9.x**.

## How checkout works

- The pool boots `minWarm` **idle** slots — each an independent microVM of the
  pool image with no workload, waiting.
- A `SwiftSandbox` with `spec.poolRef` **claims** one warm slot and injects its
  `command`/`args`/`env` into the already-booted VM over vsock. The VM is
  running, so the workload starts immediately.
- **Consume-and-replenish.** A claimed slot is never returned to the pool — one
  workload never inherits another's slot. On checkout the pool boots a fresh
  warm slot to restore the count.
- **Cold fallback.** If no warm slot is free (or the sandbox has no `command`,
  so the image entrypoint must be resolved), the sandbox boots cold
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
| `imagePullSecret` | string | — | docker-registry Secret (same namespace) for a private `image`. |
| `cpu` | int32 | `1` | vCPUs per slot. |
| `memory` | Quantity | `512Mi` | RAM per slot. |
| `minWarm` | int32 | `0` | Warm slots to keep ready. The warm buffer the pool maintains. |
| `maxWarm` | int32 | — | Cap on warm slots. The effective cap is `max(maxWarm, minWarm)` — set below `minWarm` and `minWarm` wins. |
| `idleTTL` | Duration | — | **Accepted but not honored in v1** — the pool holds `minWarm` and does not scale idle slots down. Tracked for a follow-up. |
| `network.mode` | enum | `restricted` | `restricted`, `open`, or `none` — same semantics as [SwiftSandbox](overview.md#network-modes). Applies to every slot. |
| `kernelProfileRef.name` | string | `sandbox` | SwiftKernel the slots boot. |
| `nodeSelector` | map[string]string | — | Extra node constraints, merged with the required `kubeswift.io/kernel-node=true`. |

### Status

| Field | Type | Description |
|---|---|---|
| `phase` | enum | `Pending` (resolving image/kernel), `Warming` (bringing slots up toward `minWarm`), `Ready` (buffer at target), `Degraded` (cannot reach `minWarm` — e.g. no schedulable node). |
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
  image: docker.io/library/alpine:3.20   # should match the pool's image
  poolRef:
    name: alpine-pool
  command: ["sh", "-c"]
  args: ["echo hello from a warm slot"]
```

- The workload **must** have a `command` to check out — with no command the
  image entrypoint has to be resolved, which only the cold path knows, so a
  command-less pooled sandbox cold-falls-back.
- Unlike the cold path, `env` **and** `workingDir` are honored on a checkout —
  the workload is injected over the same vsock exec channel `swiftctl sandbox
  exec` uses, which sets both.
- `status.podRef` points at the claimed slot pod (`<pool>-slot-<x>`), not at a
  pod named after the sandbox. `status.exitCode` carries the workload's real
  exit code just like a cold sandbox.
- `swiftctl sandbox logs`/`exec`/`attach <name>` work on a checked-out sandbox
  — they target the claimed slot transparently (via `status.podRef`).

## Node placement

Warm slots carry a soft topology-spread constraint (`MaxSkew: 1` over
`kubernetes.io/hostname`, `ScheduleAnyway`), so they land one-per-node across
the kernel-nodes rather than piling onto one. Warming is node-local (the rootfs
materializes on the slot's node), so a checkout that lands on any node is more
likely to find a warm slot *there*. The constraint is soft — warming never
blocks just because one node is momentarily full.

## Observability

`kubeswift_sandbox_checkouts_total{result}` counts checkouts by outcome:
`hit` (claimed a warm slot — the fast path) vs `cold` (no warm slot, or no
command — fell back to a cold boot). The **hit ratio is the pool's headline
signal**: a persistent `cold` rate means `minWarm` is too low for the arrival
rate. Pool health is `kubectl get sboxpool` (phase + `warmReplicas`).

## Limitations (v1)

- **`idleTTL` scale-down is not honored** — the pool holds `minWarm` warm slots;
  it does not shrink an idle pool. Set `minWarm` to what you want held.
- **The injected workload's env is `spec.env` only** — the pool image's own env
  is not merged for the injected command (a warm slot already booted the image).
- Keep the pooled sandbox's `image` the same as the pool's `image`; the checkout
  runs your command inside the slot's already-booted rootfs, so a different
  `image` on the sandbox is ignored for a hit (and only applies if it
  cold-falls-back).

## See also

- [Running sandboxes](overview.md) — the SwiftSandbox operator guide
- [`config/samples/sandbox/`](../../config/samples/sandbox/) — sample manifests
- [swiftctl reference](../swiftctl.md)

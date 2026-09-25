# Phase 1 r4: upgrade to the candidate `9604992`

Run 2026-09-25, 19:22–19:54 UTC. Node names are generalised as in `phase0.md`.

| Cluster | Verdict |
|---|---|
| ntx | **PASS** |
| sov | **PASS** |
| dev | **Upgrade applied, rollout FAILED: an environment failure on worker-2.** Multus there is being OOMKilled, so no new pod on worker-2 gets a network. The stop condition applies: **nothing more was run on dev. Phase 2 r4 has not started.** See "dev" below |

## Build check

| Time (UTC) | Missing |
|---|---|
| 19:22:51 | everything |
| 19:28:03 | chart + 7 images |
| 19:33:19 | chart, `swiftletd`, `migration-stunnel`, `kubeswift-gateway` |
| 19:38:38 | **none** |

Main also carries `e3b2437 release: v0.15.0 (#670)`. This candidate sits after
the v0.15.0 release.

## Pre-check (#678), on every cluster

**Stopped-but-Running guests:** there were **none on any cluster**. I checked
twice: at 19:22:48, and again right before each upgrade (dev 19:39:32, ntx
19:50:52, sov 19:51:46). All three gates were clean.

| | dev | ntx | sov |
|---|---|---|---|
| Guests (`runPolicy` / phase / boot) | `gpu-cells/innercp` Always / Running / **disk** | `capi-udn/ks-udn-cp-54klw` (unset) / Running / **disk**; `capi-udn/ks-udn-md0-…` (unset) / Running / disk; `default/sample` Running / Failed (round-1 N1 leftover); `val-n2/snapshot-local-source` Running / Failed (round-1 N2 leftover) | none |
| `kubeswift-system` labels | `kubernetes.io/metadata.name`, `name` only (no pod-security labels) | same | same |
| SwiftKernels | `default/gpu-sandbox`, `default/sandbox`, `field-testing/ft-faas`, `field-testing/gpu-sandbox`: all Ready | `field-testing/ft-faas` Ready | none |
| Jobs labelled `app.kubernetes.io/component=swiftkernel-pull` | none | none | none |
| Existing `swiftkernel-pull-*` Jobs | 8 unhashed: `swiftkernel-pull-{gpu-sandbox,sandbox}-<worker-{1,2}>` (default), `swiftkernel-pull-{ft-faas,gpu-sandbox}-<worker-{1,2}>` (field-testing) | 2: `swiftkernel-pull-ft-faas-<worker-{1,2}>` | none |
| Terminating namespaces | `val-d2`, `val-d3` | `val-n2r` | none |

**Neither baseline guest boots from a kernel.** Both `innercp` and the ntx
control-plane guest boot from disk.

## Commands (dev, then ntx, then sov)

- The CRDs from a worktree at `9604992`: 15 `serverside-applied`, 0 errors.
  - `swiftguests` and `swiftsandboxes` changed.
  - The `swiftguests` printer columns are now: `Phase, Node, Guest IP, Pod IP,
    IP Scope, Hypervisor, OS, Service, Egress, Age`.
- `helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version
  0.0.0-dev.9604992 --reuse-values`, with `--set <c>.image.tag=sha-9604992` for
  all nine components. A `--dry-run` first exited 0 everywhere.
- `kubectl rollout status` for every kubeswift Deployment and DaemonSet.

## ntx: PASS

- Helm **28 → 29**, upgraded at 19:50:56, controller rolled out 19:51:16. It
  showed "Starting workers" (13) about 30 s later, after taking the leader lease.
- `--metrics-secure=false`. VAPs as before: 3, no exec gate. All images are
  `sha-9604992`. 0 error lines.

Post-upgrade watcher (3 s):

```text
19:51:34 kernels: field-testing/ft-faas=Pulling
19:51:34 pulljobs: field-testing/swiftkernel-pull-ft-faas-<worker-1>-4745a71a[ft-faas]:Running  …-<worker-2>-0a00d444[ft-faas]:Running
         (the two old unhashed Jobs are gone)
19:51:34 cleanup pod in kubeswift-system: swift-snap-cleanup-snapshot-local-mem-1977c46af3 (Pending on worker-2)
19:51:41 kernels: field-testing/ft-faas=Ready; both new Jobs Complete; the cleanup pod gone
19:51:44 terminating namespaces: (none)     <- val-n2r finished deleting
guests: no phase change throughout
```

## sov: PASS

- Helm **34 → 35**, upgraded at 19:51:53, rolled out 19:52:16.
- 13 controllers, 0 errors, `--metrics-secure=false`, all images `sha-9604992`.
- VAPs: the same 4. No SwiftKernels, and no terminating namespaces.

## dev: upgrade applied, rollout failed (environment)

- **The upgrade:** Helm **46 → 47**, at 19:39:39.
  - `controller-manager`, `kubeswift-gateway` and `kubeswift-ui` rolled out.
    The controller shows 13 controllers, and **`--metrics-secure=true` survived**
    (`"Serving metrics server" … "secure"=true`).
  - VAPs: the same 4. The controller's only error line is a routine optimistic-lock
    conflict on the pre-existing `field-testing/ft-gpu-pool` at 19:41:02.
- **The DaemonSets `gpu-discovery` and `kubeswift-dra-driver` did not roll out.**
  `kubectl rollout status` timed out after 300 s on both. They are still 0/1
  Ready at 19:54.

**Cause.** On worker-2 (the GPU node, the only one those DaemonSets select)
every new pod fails with:

```text
FailedCreatePodSandBox … plugin type="multus-shim" name="multus-cni-network" failed (add): CmdAdd (shim): timed out waiting for the condition
```

because `kube-system/kube-multus-ds` on worker-2 is in `CrashLoopBackOff`:

```text
restarts 8, lastState: OOMKilled (exit 137)   image multus-cni:v4.2.2-thick   resources: requests=limits={cpu: 100m, memory: 50Mi}
previous log: "multus-daemon started" … then several "ADD starting CNI request" at the same second, then killed
```

- **Why worker-2 alone.** The upgrade started several pods there at once: the
  two DaemonSet pods, plus the #677 kernel re-pull Jobs for three kernels. Each
  one retries sandbox creation. The thick Multus daemon, capped at 50Mi, is
  killed under that burst.
- **Earlier traffic was fine.** Round 3's migrations to worker-2 succeeded, the
  last at 12:08.
- **This is a lab environment limit, not kubeswift code.** But the upgrade
  triggers such a burst on every node that holds kernels and GPU DaemonSets,
  which may be worth a note.

**What did work on dev** (post-upgrade watcher):

```text
19:40:27 kernels: all four Pulling; new Jobs swiftkernel-pull-<kernel>-<node>-<8 hex> labelled kubeswift.io/swiftkernel=<kernel>; the 8 unhashed Jobs gone
19:40:27 cleanup pods in kubeswift-system: swift-snap-cleanup-snapshot-local-mem-231028574f (cp-1), …-3bc9ff571b (worker-1): Running
19:40:31 the worker-1 pull Jobs Complete; the cleanup pods gone
19:40:35 val-d3 gone;  19:40:38 val-d2 gone          <- #675 cleared both stuck namespaces in ~10 s
19:40:38 field-testing/gpu-sandbox Ready  (its worker-2 Job had completed before Multus fell over)
```

**What did not work on dev.**
- `default/gpu-sandbox`, `default/sandbox` and `field-testing/ft-faas` are
  still **`Pulling`** at 19:54: their worker-2 Jobs' pods are stuck in
  `ContainerCreating` for the same network reason.
- `innercp` (on worker-2) was not disturbed: **same UID `10bd28b8-…`, 0 restarts,
  `Running`** throughout.
- The pre-existing `field-testing/ft-gpu-pool` still has its failed slot pod
  from 2026-09-24 21:41 (`Cannot open initramfs file`, the known #659/#660).
  It is `Degraded` now because of a Docker Hub pull rate limit on `alpine:3`.
  Neither is new.

**Per the stop condition, nothing more was run on dev.** Phase 2 r4 needs a
working worker-2: V6 targets it, and the rollout itself is part of Phase 1.
Unblocking it is William's call, for example raising the Multus DaemonSet's
memory limit or restarting it once the burst has passed. After that the
kubeswift DaemonSet pods and the three pull Jobs should proceed by themselves,
and I can finish Phase 1 on dev and run Phase 2.

## Baselines

| Guest | After the upgrade |
|---|---|
| dev `gpu-cells/innercp` | uid `10bd28b8-…`, 0 restarts, Running (disk boot) |
| ntx `capi-udn/ks-udn-cp-54klw` | uid `a5420720-…`, 0 restarts, Running (disk boot). See `phase3-r4.md` for the one expected status write |

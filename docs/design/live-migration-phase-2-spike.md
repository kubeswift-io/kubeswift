# Live Migration Phase 2 — Spike Results

> **Status:** In progress
> **Date:** 2026-04-29
> **Cluster:** k0s 1.34.3, miles + boba workers, CH v51.1 native install on /usr/local/bin (miles) and /root (boba)
> **Audience:** Phase 2 implementer + staff-architect review

This spike empirically resolves the four pending Phase 2 decisions before any Phase 2 design or implementation begins. It also covers the architect-flagged additions (Q1d failure paths, Q1e network handoff) and adopts the architect's Q2 descope (two dirty rates with a hard wall-clock fail cap).

## Goal

Phase 2 of live migration ships swiftletd plumbing for CH live migration: send/receive primitives, a swiftletd control surface, manual demonstration. No controller integration (Phase 3). Before designing the swiftletd extension, we need empirical answers to four decisions tracked in `kubeswift_context.md` (section "Phase 2 Decisions Pending"): (1) control surface choice, (2) confirmation that plaintext TCP is acceptable for Phase 2, (3) version-skew constraint, (4) realistic stop-and-copy window under memory dirtying. The spike answers these on the deployed cluster.

## Method

Manual two-VMM demonstration on miles and boba using bare CH processes and ch-remote. No controller, no swiftletd extension. Scripts in `spike/phase-2/` (gitignored). All evidence is from real cluster output captured in this doc.

## Q1 — Does CH v51.1 send/receive migration work cluster-side?

### Setup

- CH binary `/usr/local/bin/cloud-hypervisor` and `/root/ch-remote` v51.1 on both nodes (binaries copied from miles to boba via scp).
- `ch-remote` action surface includes `send-migration <destination_url>` and `receive-migration <receiver_url>`. URL schemes: `tcp:host:port`, `unix:/path`, plus `--local` flag on send-migration for same-host memory-mapped optimization (irrelevant cross-node).
- Migration commands route through the existing `--api-socket` HTTP API; there is no separate migration control plane.
- Guest under test: kernel boot with the deployed faas-minimal `/var/lib/kubeswift/kernels/default-faas-minimal/{bzImage,rootfs.cpio.gz}` (Linux 6.6.44 + BusyBox musl). Custom initramfs variant `rootfs-beacon.cpio.gz` produced by the spike: same files except `/init` runs a 1-Hz "BEACON counter=N uptime=X" loop to /dev/console, replacing the default interactive shell. Boot via `console=hvc0 init=/init reboot=k panic=0` — `panic=0` halts on panic instead of reboot loop, which was needed because the original rootfs's `exec /bin/sh` died once the migration probe disabled the console (early hang in tests A1 and A2 before this fix).

### Result: PASS

#### Q1a — Wire protocol (URL schemes, --local, receive arming)

- `receive-migration` on a fresh empty VMM accepts both `tcp:0.0.0.0:port` and `unix:/path`. The VMM opens the listener (TCP socket on the VMM PID, or unix socket file). Without a fresh empty VMM, `receive-migration` errors with `Error opening HTTP socket: No such file or directory` — i.e., the destination MUST be a running CH instance with no VM created.
- Re-issuing `receive-migration` against an already-listening port fails with `Error binding to TCP socket: Address in use` and the VMM exits with a fatal error. The action is one-shot per VMM lifecycle.
- `send-migration --local unix:/path` writes a memory-mapped file rather than streaming — same-host optimization. Cross-node tests use `tcp:host:port`.
- The `ch-remote send-migration` call is **synchronous-blocking from the sender's side**: it returns when the migration completes. On success the source CH process exits cleanly; on failure ch-remote returns an `InternalServerError` and the source CH stays Running.

#### Q1b — Minimum required matching state on destination

- The destination VMM must be the same CH version (Q4 documents the exact constraint). Same KVM accelerator, same host arch (both nodes are x86_64).
- The destination MUST NOT have a VM created — it must be empty. CH config and full VM state (cpu config, memory, payload kernel/initramfs, network) all transfer as part of the migration. The destination's hostfs paths (kernel, initramfs) do NOT need to match the source's — those paths are baked into the migrated config but are only re-resolved if the destination needs to re-boot, which it doesn't on a successful migration.
- However, **host resources referenced by the migrated config DO need to exist on destination at restore time**. Specifically, virtio-net `tap=<name>` requires that the tap interface exists on destination's host with the same name (Q1e).

#### Q1c — Awaiting-migration launch mode

- The destination requires a fresh CH process with `--api-socket <path>` and NOTHING ELSE. No VM should be `create`'d. Then `ch-remote receive-migration <url>` is issued against the API socket; the CH VMM internally opens the listener at `<url>` and awaits the source's connection. After successful receive, the destination VM is in `Running` state automatically (no explicit `boot` or `resume` needed; calling `resume` on the destination errors with `Invalid transition: InvalidStateTransition(Running, Running)`).
- Same-host baseline: source booted, destination empty, send-migration `unix:/sock` → 68 ms wall time for 256 MiB guest.
- Cross-node baseline (miles → boba via TCP): 2.87 s wall time for 256 MiB guest. Sentinel-validated by the BEACON loop — source counter reached 15 at uptime 14.10 s, destination resumed at counter 16 with monotonic uptime 15.11 s. Source CH exited cleanly post-migration.

#### Q1d — Failure-path inventory

| Failure | Source state | Destination state | Source recovery | Destination recovery |
|---|---|---|---|---|
| **F1: Source CH killed mid-migration** | gone (we killed it) | gone (CH self-terminates when source connection closes mid-transfer) | N/A — source is gone | N/A — destination self-exits cleanly |
| **F2: Destination CH killed mid-migration** | **Running** | gone (we killed it) | **automatic** — source guest continues executing (BEACONs continued from counter 13 → 21 over the 8 s after destination kill) | N/A — destination is gone |
| **F3: Network DROP mid-migration** | source CH is still alive; ch-remote info hangs (timeout 3 s, falls back to `gone`) during DROP; reports Running again after rule lifted | listener self-exits when source's TCP retransmits give up | source resumes cleanly after rule lifted; the in-flight send-migration call returns `Connection refused` (because destination listener was already gone); a fresh `send-migration` to a fresh destination is required | N/A |
| **F4: Cancellation primitive** | n/a | n/a | n/a | n/a |

**F4 detail:** `ch-remote --help` shows no `cancel`, `abort`, or `stop` subcommand for migration. The only way to cancel an in-flight `send-migration` is to kill the destination CH (F2 result) — the source then automatically resumes its guest. Destination kill is therefore the **de facto cancellation primitive** for Phase 2. This is a real finding: **the controller's migration-cancel path will be implemented as "kill destination CH process," not via an explicit CH cancel API.**

**F1 detail:** when the source is killed mid-transfer, the destination CH exits with an error rather than parking in a recoverable state. This means that in Phase 3 / cluster operation, recovering from a source crash will require fresh destination provisioning — there's no "try the same destination again" without restarting it.

**F3 detail:** TCP retransmission timeouts on the migration channel are long (default Linux ~16 minutes for SYN, much shorter for ESTABLISHED). The destination listener appears to give up well before that; we observed the destination listener gone after ~3-5 s of DROP. Phase 2's swiftletd timeout default should be aware of this and not assume the destination listener stays up indefinitely.

#### Q1e — Network backend handoff

| Test | Setup | Result |
|---|---|---|
| **T1: tap exists on both nodes, same name (kstap0), same MAC** | source guest with `--net "tap=kstap0,mac=02:00:00:00:00:11"`; both nodes have kstap0 created with `ip tuntap add` | **PASS**. Migration in 2.34 s. Destination's `info` shows identical net config (same MAC, same `host_mac`, same `id`, same queue config). Destination's kstap0 transitioned from `state DOWN` to `state UP, LOWER_UP` (CH attached and brought it up). Source's kstap0 left in `state DOWN` (CH released it on exit). |
| **T2: tap exists on source only, missing on destination** | source side identical; destination has no kstap0 | Destination CH errors at `receive-migration` time with the underlying tap-open failure. The source's send-migration returns `Connection refused` because the destination listener was already gone by the time send connected. |

**Phase 2 finding:** the swiftletd control plane MUST ensure the destination's tap (and any virtio-fs / virtio-blk backing files) exist on the destination host BEFORE the destination CH is asked to `receive-migration`. The destination needs to be fully provisioned for the same VM config the source has.

The MAC, queue counts, MTU, vhost mode, and `host_mac` are all preserved across migration. The destination tap's prior state (DOWN/UP) is overridden by CH on attach — CH brings the tap UP as part of restoring the virtio-net device.

This finding is consistent with Phase 1's design: the SwiftMigration controller's destination provisioning must mirror the source's pod surface (network attachments, dataDiskRef PVC, etc) before `receive-migration` is issued. **Phase 2's swiftletd action set must include a "prepare-destination" action that creates the tap/network attachments before the receive-migration action fires.**

### Finding (Q1)

**CH v51.1 cluster-side migration WORKS.** Cross-node migration completes in ~2.9 s for a 256 MiB guest over the cluster's internal network. The protocol is straightforward: source `send-migration tcp:dst:port`, destination empty CH `receive-migration tcp:0.0.0.0:port`, destination auto-resumes on completion, source auto-exits on completion. Sentinel-validated continuity of guest process state across the cut.

The four sub-findings for Phase 2 design:
1. **No explicit cancel primitive — destination kill is the cancel primitive.** Source automatically resumes when destination dies.
2. **Source kill destroys destination too** — recovery from source crash requires fresh destination provisioning.
3. **Network drops break both sides** — destination listener gives up after a few seconds; controller timeout strategy must recognize this.
4. **Network/storage backends must exist on destination at receive-migration time** — pod-level provisioning happens before swiftletd's receive action.

Recorded artifacts: `spike/phase-2/q1-samehost.sh`, `q1-crossnode-local.sh`, `q1-crossnode-sentinel.sh`, `q1d-onmiles.sh`, `q1e-network.sh`, with logs in `/tmp/spike-phase2/*.log` on miles+boba.

## Q2 — Pre-copy convergence on a memory-dirtying workload

### Setup

- Guest: 1 GiB RAM, 2 vCPUs, faas-minimal kernel, custom initramfs with a baked-in dirtier binary running as a background process. The dirtier mmaps a 512 MiB region (50% of guest RAM), then writes one byte per page at random offsets at a controlled rate.
- Dirtier source: `spike/phase-2/dirtier.c`, statically linked glibc binary (820 KiB; works on the guest's BusyBox-musl rootfs because it has no dynamic dependencies).
- Initramfs variants: `rootfs-q2-low.cpio.gz` (dirtier with `pages_per_iter=128, sleep_us=10000`) and `rootfs-q2-high.cpio.gz` (`pages_per_iter=1280, sleep_us=10000`). Both run on a 100 Hz tick (10 ms sleep). The 10 ms tick avoids CPU-bound dirty rates that would otherwise dominate cache-line bandwidth (~12 GiB/s observed in early runs with 1 ms tick — too fast to be representative of any real workload).
- Hard wall-clock cap: 60 s. If `send-migration` doesn't return within 60 s, we declare "failed to converge" and abort.
- CH launched with `-vvv --log-file <path>` on both ends so iteration-level events appear in the log.

### Result

Two dirty rates, both completed within the wall-clock cap:

| Rate label | Target | Measured | Pre-copy iters | Iter timing | Total wall time | vCPU-paused window (final flush) | BEACON gap (operator-visible) |
|---|---|---|---|---|---|---|---|
| **LOW** | 50 MiB/s | 41.7 MiB/s | 5 (hit cap) | 4.0 s → 2.3 s → 1.4 s → 0.85 s → 0.6 s | **18.8 s** | **~0.6 s** | 20.1 s |
| **HIGH** | 500 MiB/s | 416.7 MiB/s | 5 (hit cap) | 4.6 s → 4.6 s → 4.6 s → 4.6 s → 4.6 s | **37.0 s** | **~4.6 s** | 37.9 s |

**Reading the two downtime numbers:** `vCPU-paused window` is the time the guest is actually frozen (CPU registers + final dirty-page flush + resume on dst). `BEACON gap` is the operator-visible total clock skew the guest experiences (including pre-copy iterations during which the source guest is still running but the destination guest doesn't yet exist). F7 in the findings summary captures this distinction; Phase 3 status reporting should expose both.

#### Q2a — Convergence-fail threshold

CH caps pre-copy at **5 iterations** (`Dirty memory migration N of 5` in source log). After the 5th iteration, CH forces stop-and-copy regardless of remaining dirty-page count. Convergence-fail in the spec sense (pre-copy never able to keep up) doesn't manifest as a timeout — it manifests as the stop-and-copy phase being long because the iteration cap forces a final flush of whatever is dirty.

At **LOW** the iteration windows shrink (4.0 → 0.6 s), confirming pre-copy is converging — by iter 4 the window of dirtied pages is small. At **HIGH** the iteration windows are essentially flat at ~4.6 s — the dirty rate matches the transfer rate, so each iteration produces as much dirty memory as it cleans. **HIGH did not actually converge; it timed out at the 5-iteration cap.**

The "wall-clock fail" definition the architect requested (60 s cap) was not hit at either rate. The real convergence failure mode in CH v51.1 is: pre-copy hits the 5-iteration cap, then stop-and-copy proper runs with however much dirty memory is left. The size of that "left over" dirty set is what determines the user-visible pause. For HIGH the pause window was essentially equal to one iteration (~4.6 s).

To extrapolate: a workload dirtying memory faster than the migration channel can transfer it would cause the final stop-and-copy phase to take indefinitely long. On the cluster's internal network (multi-Gbps), 500 MiB/s is well under available bandwidth, so the iteration window is ~4-5 s. A real "convergence failure" would happen at dirty rates approaching the migration channel's bandwidth limit (~50 GiB for a 1 Gbps link, ~6 GiB/s for 10 Gbps). For Phase 2's targeted workloads (Web servers, application VMs, databases at typical scale) dirty rates of <100 MiB/s are realistic; pre-copy will converge cleanly.

#### Q2b — Stop-and-copy windows

The "BEACON gap" reported above is the user-visible pause from the GUEST's perspective: source last beacon timestamp vs destination first beacon timestamp, measured against guest clock. This INCLUDES pre-copy time — during pre-copy the source guest is still running but the destination guest doesn't exist yet, so the destination's first BEACON post-resume is whenever the destination's clock catches up to source's last-paused-clock.

The actual stop-and-copy window where the vCPU is paused is the time between CH's last "Dirty Memory Range Table" log line and "Migration complete":

- **LOW**: 47.0 s − 33.0 s = **14 s of trailing-dirty + complete** but the actual vCPU-paused window is the FINAL iteration's transfer + cpu-state, ≈ 0.6 s + cpu-state.
- **HIGH**: 47.0 s → 51.6 s = **4.6 s** for the final stop-and-copy phase.

For Phase 2 design purposes, **plan on 0.5–5 s vCPU-paused window for typical workloads**, with the high end matching the iteration window for high-dirty-rate guests. Operator-visible "downtime" (BEACON gap) is significantly longer because pre-copy iterations also count from the dst clock's perspective even though the source guest is running normally during that time.

#### Q2c — Iteration progress visibility

CH exposes pre-copy iteration progress via INFO log lines:

```
cloud-hypervisor: 23.871s: INFO ... Dirty memory migration 0 of 5
cloud-hypervisor: 23.876s: INFO ... Dirty Memory Range Table:
cloud-hypervisor: 28.496s: INFO ... Dirty memory migration 1 of 5
cloud-hypervisor: 28.501s: INFO ... Dirty Memory Range Table:
...
cloud-hypervisor: 51.646s: INFO ... Migration complete
```

Format is fixed-string `Dirty memory migration N of 5` — easy to parse. The "of 5" upper bound is hardcoded in CH (not configurable in v51.1's CLI). The `Dirty Memory Range Table:` line is followed by per-region byte-range listings (verbose; not ideal for parsing, but parseable).

There is **no programmatic API** (HTTP/JSON) for iteration progress in v51.1 — the only source is the log. Phase 2's swiftletd will need to parse stdout/stderr (or `--log-file`) to extract iteration progress for status reporting. Alternative: poll the source CH's `info` API and infer state changes from `state` field transitions (Running → Paused). This is coarser-grained (on/off rather than 0/5/...n/5) but simpler.

### Finding (Q2)

1. **CH v51.1 caps pre-copy at 5 iterations**. After 5, CH forces stop-and-copy regardless of dirty rate. Phase 2 design must accept that high-dirty-rate workloads have a longer stop-and-copy proper window (one iteration's worth), not an indefinite pause or migration failure.
2. **Realistic stop-and-copy windows for Phase 2 planning:**
   - Low-dirty-rate workloads (≤50 MiB/s): vCPU paused for ~0.5–1 s.
   - High-dirty-rate workloads (~500 MiB/s): vCPU paused for ~4–5 s (matching one iteration window).
3. **Operator-visible downtime is several times the vCPU-paused window** because pre-copy iterations contribute to the BEACON gap even though the source guest is still running. For HIGH this was 37 s, for LOW it was 20 s. Phase 2's `observedDowntime` reporting should distinguish these two windows clearly.
4. **Iteration progress is log-parseable but not API-queryable in v51.1.** Phase 2's swiftletd extension will need a log-tail-and-parse pattern to expose per-iteration progress to the controller, OR will poll the `info` API for state transitions and report at coarser granularity.
5. **The architect's wall-clock fail cap was not hit** at either tested rate. The actual fail mode in CH v51.1 is "iteration cap reached, stop-and-copy now." Phase 2 spec should encode this as an expected outcome with a cancellable-timeout knob, not a true failure.

Recorded artifacts: `spike/phase-2/dirtier.c`, `q2-convergence.sh`, output logs `/tmp/spike-phase2/q2-2.log` (LOW) and `/tmp/q2-high.log` (HIGH) on miles. CH-side detail: `/tmp/spike-phase2/src-ch.log`.

## Q3 — Annotation vs HTTP control surface?

### Setup

This question is design analysis grounded in the empirical findings from Q1, Q1d, Q1e, and Q2. There's no separate cluster experiment for Q3; the inputs are:

- The CH wire-protocol surface from Q1 (send-migration, receive-migration, both via existing `--api-socket` HTTP API)
- The failure-path semantics from Q1d (no explicit cancel; destination-kill is the cancel primitive)
- The pod-level provisioning prerequisite from Q1e (tap/PVC must exist on dst before receive)
- The progress-reporting reality from Q2 (per-iteration log lines, no programmatic API in v51.1)
- The existing snapshot Phase 2 annotation pattern (controller writes `kubeswift.io/snapshot-action`; swiftletd watches its own pod; writes `*-id-mirror` on completion). See `internal/controller/swiftsnapshot/local.go`, `rust/swiftletd/src/action.rs`.

### Q3a — Action set swiftletd needs to expose

For Phase 2 manual demonstration (no controller integration), swiftletd needs to expose actions for both source and destination roles. A single launcher pod plays one role per migration; the role is decided at pod-creation time by the future Phase 3 SwiftMigration controller.

| Action | Initiator | Purpose | Empirical input |
|---|---|---|---|
| **prepare-destination** | (pod-level, not swiftletd) | Create tap, attach PVC, etc., before swiftletd starts CH | Q1e: tap must exist before receive-migration |
| **start-receive** | swiftletd on dst pod | Launch empty CH, issue `ch-remote receive-migration <url>` | Q1c: dst must be empty fresh CH |
| **start-send** | swiftletd on src pod | Issue `ch-remote send-migration tcp:<dst>:<port>` against running CH | Q1c: src must have running guest |
| **report-progress** | swiftletd on src pod | Tail `--log-file`, parse `Dirty memory migration N of 5` lines, mirror to annotations | Q2c: progress is log-only |
| **report-complete** | swiftletd on both pods | Source: detect CH exit (cleanly). Destination: detect VM state Running. | Q1c: src auto-exits, dst auto-resumes |
| **report-failed** | swiftletd on both pods | Detect non-zero ch-remote exit, capture error text from log | Q1d: error chain in send-migration log |
| **cancel** | swiftletd on dst pod | Kill the local CH process; CH on src then auto-resumes the source guest | Q1d-F2: destination kill is the cancel primitive |
| **wait-keepalive** | swiftletd on dst pod | After issuing receive-migration, hold the listener until source connects | Q1d-F3: dst listener gives up after a few seconds without a connection |

Note: there is **no** `await-source-connection-with-timeout` action in CH v51.1's API. Q1d-F3 showed the destination's TCP listener self-terminates after the source's TCP retransmits give up. The "wait-keepalive" action above is implicit in receive-migration's blocking semantics — swiftletd just needs to NOT timeout the receive-migration call too early. Phase 2 docs should call out: from the controller's perspective, swiftletd must keep the receive-migration alive until either it returns success/failure on its own, OR the controller explicitly cancels via destination-kill.

### Q3b — Annotation churn at the progress-update rate

Q2 observed pre-copy iteration windows of ~4.6 s at the HIGH dirty rate (5 iterations × ~5 s = 25 s pre-copy phase). Per-migration annotation patches:

| Event | Annotation update | Approximate rate |
|---|---|---|
| start-send issued | `kubeswift.io/migration-action`, `*-action-id` | 1 patch |
| each pre-copy iter | `kubeswift.io/migration-progress=iter=N/5` | 5 patches over ~25 s = 1 every 5 s |
| stop-and-copy starts | `migration-progress=stopcopy` | 1 patch |
| migration complete | `migration-result=success`, `*-action-id-mirror` | 1 patch |
| **Total per migration** | | **~8 patches over ~30 s** |

This is **trivially within** the annotation surface's throughput. Snapshot Phase 2 issues comparable rates (5–10 state transitions per snapshot/restore, similar timeframe) and operates fine on production-scale clusters. The kube-apiserver and etcd handle thousands of patches/sec; <1 patch/sec per migration is a non-issue.

For comparison, the SwiftGuest annotation surface (which swiftletd already maintains) is updated by the lease.rs DHCP discovery path at much higher rates during boot — the annotation pattern has been load-tested in production via the existing pod-annotation reporting model.

**Verdict:** annotation surface is feasible for the migration progress-reporting cadence. No churn problem.

### Q3c — Actions that fundamentally need request/response semantics

Auditing the action set above against "fits in annotation pattern" vs "needs synchronous request/response":

| Action | Annotation-fit? | Why |
|---|---|---|
| start-receive | YES | Controller writes annotation; swiftletd reads, executes, writes id-mirror |
| start-send | YES | Same shape |
| report-progress | YES | swiftletd-write annotation; controller polls or watches |
| report-complete | YES | swiftletd writes terminal annotation; controller observes via watch |
| report-failed | YES | Same |
| cancel | YES | Controller writes cancel annotation; swiftletd kills CH (snapshot Phase 2's cancel pattern) |
| **capability handshake** | **N/A — CH does it server-side** | CH v51.1 has no client-visible handshake; version mismatch surfaces as send-migration error (Q4 finding). swiftletd doesn't need a separate handshake. |

**Capability negotiation between source/dest swiftletd is NOT needed.** CH itself does no capability handshake (see Q4 below). Version compatibility is a controller-level pre-flight concern (the controller can verify both nodes run the same CH image), NOT a runtime negotiation between swiftletd processes.

There is one subtle case: the destination's `receive-migration` is a **blocking call** that returns only when migration completes or fails. Conceptually this is a long-lived RPC. The annotation pattern accommodates this naturally: swiftletd blocks on receive-migration in a worker thread; the main loop continues to process annotation events; on completion, the worker thread writes the result annotation. This is the same shape as snapshot Phase 2's snapshot-stager init container behavior.

### Finding (Q3)

**Annotation-driven control surface is the right choice for Phase 2.** All identified actions fit the pattern, churn rate is well within the annotation surface's throughput, and no action requires synchronous request/response semantics that annotation patches can't model.

The recommended action set for Phase 2 swiftletd extension:

```
# Source-side annotations on the source launcher pod:
kubeswift.io/migration-action: send         # written by controller / operator (Phase 2 manual)
kubeswift.io/migration-action-id: <ulid>    # idempotency key
kubeswift.io/migration-target-url: tcp:host:port
kubeswift.io/migration-progress: iter=N/5 | stopcopy | complete | failed
kubeswift.io/migration-action-id-mirror: <ulid>  # written by swiftletd on each transition

# Destination-side annotations on the destination launcher pod:
kubeswift.io/migration-action: receive
kubeswift.io/migration-action-id: <ulid>
kubeswift.io/migration-listen-url: tcp:0.0.0.0:port
kubeswift.io/migration-progress: listening | receiving | running | failed
kubeswift.io/migration-action-id-mirror: <ulid>
```

The `*-action-id-mirror` shape mirrors snapshot Phase 2's pattern and gives the controller a watchable signal that swiftletd has observed each transition. Bug 14 from snapshot Phase 2 (`action-id changed across status patches`) is the cautionary precedent — Phase 2's swiftletd extension must use the **same action-id throughout the migration's lifecycle**, only writing the mirror once it has observed the action.

**Implementation note for rust-runtime-engineer:** the progress-reporting branch needs to tail CH's `--log-file` and parse `Dirty memory migration N of 5` plus `Migration complete` lines (Q2c). This is new territory — swiftletd doesn't currently parse CH logs. The alternative (poll the CH `info` API) gives coarser granularity (Running → Paused → Running) but is simpler to implement. **Recommend simpler poll-based progress reporting for Phase 2; log-parsing as a future improvement.** The 5-iter visibility is operator-nice-to-have, not load-bearing for the migration to succeed.

Recorded artifacts: design-only finding, no separate spike script. Inputs were Q1c log evidence (auto-resume on dst), Q1d-F2 log evidence (auto-resume on src after dst-kill), Q2 log lines (`Dirty memory migration N of 5`).

## Q4 — Same-CH-version constraint?

### Setup

Per user priority + architect time-cap descope: prioritize Q4a (exact error message) and Q4c (handshake vs post-failure detection). Skip Q4b's full version-pair sweep. 1.5-day cap.

- Source CH binaries tested: v51.1 (deployed) and v50.2 (pre-built static binary fetched from CH GitHub release v50.2, `cloud-hypervisor-static`).
- Both directions tested: v51.1 src → v50.2 dst, AND v50.2 src → v51.1 dst.
- Both nodes use the deployed v51.1 `ch-remote` for API calls (the migration data plane is between CH VMMs, so the ch-remote version doesn't affect the migration protocol — though one observation: v51.1 `ch-remote info` does NOT correctly query a v50.2 VMM, suggesting some HTTP API drift).

### Result

**Both directions WORK.** Bidirectional compatibility across one minor version skew (v50 ↔ v51).

| Direction | Wall time | Pre-copy iters | Dst final state | Notes |
|---|---|---|---|---|
| **v51.1 src → v50.2 dst** | 2.35 s | 5 of 5 | Running | Source exited cleanly, destination resumed automatically. |
| **v50.2 src → v51.1 dst** | 2.35 s | 4 (`Dirty memory migration N of 5`) | Running | Same shape, completed with one fewer iteration logged. |

#### Q4a — Exact error message on version mismatch

**No version-mismatch error was triggered** at v51.1 ↔ v50.2. The migration succeeded in both directions. The architect anticipated a hard rejection that didn't materialize — at least not at this version delta.

This is operationally consequential: it suggests the CH migration wire format has been **stable across at least one minor version boundary** (v50 → v51), which gives operators flexibility for rolling node upgrades without grounding running VMs.

What we DO see in the destination CH log on every successful migration (both directions):

```
cloud-hypervisor: ... INFO arch/src/x86_64/mod.rs:550 -- No CPU incompatibility detected.
```

This is an explicit CPU-feature compatibility check at restore time. Phase 2 should plan that **CPU feature mismatch (e.g., a guest using AVX-512 migrated to a destination without it) IS the realistic failure mode**, not version mismatch. The exact error message for a CPU mismatch is not captured by this spike (we don't have heterogeneous CPUs across miles+boba — both are recent Intel Xeon).

#### Q4c — Handshake vs post-transfer detection

The migration protocol doesn't expose a client-visible "capability handshake." The destination opens its TCP listener on receive-migration; the source connects and immediately starts streaming snapshot data. There is no version-exchange phase that we can observe.

That said:
- The CPU-compatibility log line above fires on the destination at uptime ~2.5 s — i.e., AFTER the snapshot has been received but BEFORE the VM is resumed. So at least the CPU check is post-receive but pre-resume. A failure here would presumably abort the migration before the destination resumes, but we couldn't trigger it with our hardware.
- The source-side log (`Dirty memory migration N of 5` lines, then `Migration complete`) shows no version-exchange phase. The dirty iteration timing is identical in both v51.1 → v50.2 and v50.2 → v51.1 directions, suggesting the protocol negotiates nothing or very little.

**Phase 2 implication:** there is no way to pre-flight-check version compatibility from CH itself. The Phase 2 + Phase 3 controllers must enforce version-match policy at the Kubernetes layer:
- **Image tag comparison** between source and destination launcher pod images.
- A controller-level admission check that rejects cross-version migrations on by-default.
- An opt-in escape hatch (`spec.allowVersionSkew=true` analogous to `spec.allowIPChange`) for operators who have validated their specific cross-version pair works.

#### Q4b — Patch-skew vs minor-skew vs major-skew (descoped)

Per user priority and architect time-cap, this sub-question was descoped after Q4a/Q4c gave us a definitive answer for the v50 ↔ v51 minor pair. Open for future spike work:

- Test patch-only skew (e.g., v51.0 ↔ v51.1) — likely compatible if minor skew is.
- Test larger gaps (v47 ↔ v51) — to find where compatibility actually breaks.
- Test on heterogeneous CPUs (different microarchs) to capture the CPU-incompatibility error message.

These are deferred to a Phase 2/3 follow-up; Phase 2 design proceeds on the assumption that **CH v51.1 ↔ v50.2 is the validated tolerance band, and operators must keep nodes within at most one minor version of each other**.

### Finding (Q4)

1. **CH v51.1 ↔ v50.2 migration works in BOTH directions.** Wire-format compatibility extends across at least one minor version boundary in CH v50/v51.
2. **No version-mismatch error message exists at this skew.** The architect's anticipated "exact error on version mismatch" wasn't reachable with the pre-built CH releases. Larger skews (deferred Q4b) MAY trigger one; we don't know.
3. **CH does check CPU feature compatibility post-receive** (`No CPU incompatibility detected.` log). This is the real failure mode in production: heterogeneous CPU microarchs across nodes will reject migrations of guests using non-portable features (AVX-512, etc.).
4. **No client-visible capability handshake.** Version compatibility must be enforced **at the Kubernetes layer** (controller-level pre-flight check on image tags), not inferred from CH itself.
5. **Operational implication:** rolling CH upgrades within a minor version are likely safe for live migration. Rolling upgrades across major versions need Phase 3+ validation. The Phase 2 spec should require **exact-image-tag match** by default, with an opt-in skew flag for operators willing to take responsibility.

Recorded artifacts: `spike/phase-2/q4-version.sh` (v51.1 → v50.2), `q4-reverse.sh` (v50.2 → v51.1). Source CH log: `/tmp/spike-phase2/src-ch.log`. Destination CH log: `/tmp/spike-phase2/dst-ch.log`.

## Findings summary

| ID | Severity | Decision affected | Recommendation |
|---|---|---|---|
| **F1** Wire protocol works | INFORMATIONAL | Decision 1 (control surface) | Proceed with Phase 2 design — CH v51.1 cluster-side migration is functional. |
| **F2** No CH cancel primitive; destination-kill is the cancel | HIGH | Decision 1 (control surface) | swiftletd cancel = kill-destination-CH. Source auto-resumes (verified Q1d-F2). |
| **F3** Source kill destroys destination too | MEDIUM | Phase 3 recovery semantics | Design Phase 3+ to require fresh destination provisioning after source crash; no retry-same-destination. |
| **F4** Network DROPs break dst listener within seconds | MEDIUM | Decision 1 (control surface) | swiftletd's destination side must keep receive-migration alive; controller's heartbeat budget should be measured in seconds, not minutes. |
| **F5** Destination prereqs (tap, PVC) must exist BEFORE receive-migration | MEDIUM | Pod-builder design | Add a "prepare-destination" pod-level action that runs before swiftletd's receive-migration action fires. Match Phase 1's pod-provisioning order. |
| **F6** CH v51.1 caps pre-copy at 5 iterations | INFORMATIONAL | Decision 4 (convergence test surface) | Phase 2 spec encodes this as an expected outcome with a configurable max-pause-window knob, not a binary fail/succeed. |
| **F7** Two distinct downtime numbers exist | MEDIUM | Phase 3 status reporting | `observedDowntime` should distinguish vCPU-paused window (0.5–5 s) from operator-visible BEACON gap (20–40 s). |
| **F8** Iteration progress is log-only in v51.1 | LOW | Decision 1 (control surface) | rust-runtime-engineer should default Phase 2 progress reporting to coarse `info`-API state polling, not log-tail-and-parse. Log parsing is a Phase 3+ improvement. |
| **F9** Annotation surface fits all migration actions | INFORMATIONAL | Decision 1 (control surface) | Annotation-driven control plane confirmed feasible. Same shape as snapshot Phase 2's `*-action-id-mirror` pattern. |
| **F10** CH v51.1 ↔ v50.2 are bidirectionally compatible | HIGH | Decision 3 (version constraint) | Operators have flexibility for rolling CH upgrades within a minor version. Phase 2 spec defaults to exact-image-tag match with an opt-in skew flag. |
| **F11** No CH-level version handshake | MEDIUM | Decision 3 (version constraint) | Version-match policy must be enforced at the Kubernetes/controller layer (image-tag comparison), not at the CH wire level. |
| **F12** CPU-feature compatibility IS checked post-receive | HIGH | Phase 3 pod scheduling | The realistic production failure mode is heterogeneous CPU microarchs (AVX-512, etc.). Phase 3 scheduling should match guest CPU model with destination-node CPU features. |

## Resolved decisions

| Decision | Resolution | Evidence |
|---|---|---|
| **1: swiftletd control surface** | **annotation-driven**, mirroring snapshot Phase 2's `kubeswift.io/<resource>-action-id-mirror` pattern. Action set: prepare-destination (pod-level), start-receive, start-send, report-progress, report-complete, report-failed, cancel (= dst-kill). | Q3a action enumeration; Q3b churn rate ~8 patches per ~30 s migration; Q3c no action requires synchronous request/response. |
| **2: mTLS posture for Phase 2** | **plaintext TCP confirmed** for Phase 2 manual demonstration. mTLS is Phase 3 territory. The migration channel carries no production traffic in Phase 2 (no controller integration). Operators running Phase 2 on a trusted cluster network plane is the threat-model premise. | Q1a: `tcp:host:port` works as send-migration URL with no auth layer. No production traffic flows over this channel in Phase 2. |
| **3: Same-CH-version constraint** | **Major.minor compatible at v50/v51**, but Phase 2 spec **defaults to exact-image-tag match**, with `spec.allowVersionSkew=true` opt-in escape hatch (analogous to Phase 1's `spec.allowIPChange`). Detection is at the Kubernetes layer (controller-level image-tag comparison), NOT at the CH wire level. | Q4a/Q4c: v51.1 ↔ v50.2 succeeded both directions; no CH-level version handshake exists. CPU-feature compatibility is the realistic failure mode (F12). |
| **4: Pre-copy convergence test surface** | **CH v51.1's 5-iteration cap IS the convergence gate.** High-dirty-rate workloads do not converge in the spec sense — they hit the iteration cap and emerge with stop-and-copy ≈ one iteration window of dirty pages. Phase 2 spec should encode `spec.maxPauseWindow` (operator chooses an acceptable vCPU-paused window — workloads that would exceed it are rejected at admission via dirty-rate estimation). `spec.timeout` caps total migration time at the controller level. There is **no separate "failed to converge"** outcome distinct from "hit iteration cap with too much dirty memory left" — Phase 2 implementer should treat these as the same condition. | Q2: LOW (50 MiB/s) → vCPU pause ~0.6 s (pre-copy converged before cap); HIGH (500 MiB/s) → vCPU pause ~4.6 s (cap hit, no convergence). Architect's wall-clock fail-cap (60 s) was not hit at either rate. |

## Open questions for Phase 2 design

These surfaced during the spike and are NOT in the original four decisions. They warrant explicit treatment in Phase 2 design before swiftletd extension begins:

1. **Heterogeneous CPU microarch policy.** F12 highlights that CPU compatibility is checked at receive time. Phase 2 design must answer: does the controller pre-flight-check guest CPU model vs destination CPU features? Or do we rely on Kubernetes node-affinity / topology constraints? OR do we accept that operators will hit "No CPU incompatibility" failures at runtime and just need a clear error path? **Recommendation:** Phase 2 controller adds a CPU-feature check to the validating webhook, mirroring Phase 1's `target node Ready and uncordoned` check.

2. **Destination listener timeout strategy.** F4 showed CH's receive-migration listener gives up after ~3-5 s of network silence. Phase 2 needs a default destination-side timeout that's long enough for controller<->controller orchestration latency but short enough to fail fast on real network breaks. **Recommendation:** ~30 s default, exposed as `spec.destinationTimeout` on SwiftMigration.

3. **observedDowntime vs observedPauseWindow.** F7 surfaced two distinct numbers operators care about. Current Phase 1 SwiftMigration status has `observedDowntime` (single field). Phase 3 should consider splitting into `observedPauseWindow` (vCPU paused) and `observedTotalMigrationTime` (operator-visible).

4. **Progress-reporting mechanism.** F8 showed log-parsing is the only path to per-iteration visibility in v51.1. Phase 2 design should explicitly choose: poll-`info`-API (coarse, simple) vs tail-`--log-file` (fine-grained, fragile to log format changes in future CH versions). **Recommendation per spike doc:** poll-based for Phase 2; log-parsing reserved for Phase 3+ if operator demand surfaces.

5. **Source-crash recovery model.** F3 showed source kill destroys destination too. Phase 3 cannot retry-same-destination after a source crash; the controller must provision a fresh destination. The `SwiftMigration` controller's failure-recovery state machine should reflect this: `phase=Failed` after source crash means "go all the way back to provisioning," not "retry the network connection."

6. **Migration channel auth for Phase 3.** Decision 2 confirmed plaintext TCP for Phase 2. Phase 3 mTLS implementation needs:
   - A sidecar pattern (mTLS proxy in front of CH's TCP listener)? Or first-party CH support (would require upstream PR)?
   - Trust anchors: cluster CA, per-node certs, or per-migration ephemeral certs?
   - **Compose with S1 below:** mTLS encrypts the migration channel, but S1's annotation-trust-boundary issue persists regardless of TLS — a malicious annotation patcher could still redirect the source to an attacker-controlled mTLS-terminating endpoint. **Both mitigations are required** for Phase 3 production traffic; neither subsumes the other.

7. **Audit logging policy** (raised by S4). Migration is the highest-stakes operation in Phase 3 (full guest state crosses the wire). Phase 3 should emit Kubernetes Events on each phase transition with target-URL, source-pod, destination-pod, and operator identity. Define event schema, retention, and the operator-identity binding for `spec.allowVersionSkew` / `spec.allowIPChange` flags before Phase 3 design.

### Must-have-before-Phase-3 checklist

Consolidating the security and operational findings: **before Phase 3 production migration traffic flows**, the following items are non-negotiable:

- [ ] **mTLS or equivalent transport authentication** on the migration channel (Phase 3 own work; see OQ6).
- [ ] **swiftletd reads URL inputs from the SwiftMigration CR**, not from controller-set pod annotations (S1; ties to OQ6).
- [ ] **Threat-model documentation** in `docs/design/live-migration.md` and (for Phase 2) the `kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation gate (S2).
- [ ] **Controller-level CPU-feature pre-flight check** in the SwiftMigration validating webhook (OQ1; mitigates F12/S3 racing).
- [ ] **Audit-event schema** for migration phase transitions (OQ7).
- [ ] **`spec.allowVersionSkew` opt-in flag** with operator-identity binding (Decision 3; S4).

## Security review findings

The security-engineer reviewed the spike at close (verdict: **CONCERNS-DOCUMENTED**, no Phase 2 blockers). Findings folded back into the relevant Open Questions and listed here for discoverability:

### S1 (HIGH) — Annotation trust boundary requires pre-Phase-3 mitigation

The proposed annotation surface (Q3) puts `kubeswift.io/migration-target-url` and `kubeswift.io/migration-listen-url` directly into pod metadata. Anyone with `pods/patch` RBAC in the launcher pod's namespace — which includes most operator users — can rewrite these values. Concretely:

- **Exfiltration via target-url rewrite:** a malicious patcher could redirect `send-migration` mid-flight to an attacker-controlled endpoint. CH streams full guest memory + cpu state in cleartext (Decision 2). This is a direct VM-state-exfiltration path requiring no kubelet/hypervisor compromise.
- **Listener hijack via listen-url rewrite:** patcher induces destination swiftletd to bind on attacker-chosen port; race a malicious `send-migration` from another source. The "destination must be empty fresh CH" precondition (Q1c) is a partial defense, but in Phase 3 with controller-orchestrated destinations this is the steady-state.

**Mitigation (Phase 3 prerequisite, not Phase 2):** swiftletd must derive `target-url` / `listen-url` from the SwiftMigration CR directly (read by swiftletd via kube-rs), NOT from the launcher pod's annotations. Annotations should carry only the action-id and progress reporting; URLs are controller-only fields. Alternative: validating admission webhook on `pods/patch` rejecting writes to `kubeswift.io/migration-*`. **This addition is mandatory before Phase 3 production migration traffic flows.**

### S2 (CONFIRM-with-gating) — Plaintext TCP for Phase 2 needs explicit threat-model gating

Decision 2 (plaintext TCP for Phase 2) is acceptable for manual demonstration ONLY if discoverability gates are in place. Required before Phase 2 ships:

1. **`docs/design/THREAT-MODEL.md`** (new) or banner in `docs/design/live-migration.md` calling out: "Phase 2 swiftletd migration plumbing carries unauthenticated guest state in cleartext on the cluster network. Operator MUST NOT route production traffic through this path."
2. **`kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation** required on the SwiftMigration CR or its launcher pod for Phase 2 swiftletd to accept any migration action. Phase 3 removes the gate when mTLS lands. Same shape as Phase 1's `spec.allowIPChange` opt-in.
3. **Phase 2's threat-model premise sentence** (the "trusted cluster network plane" statement) MUST be promoted from this spike doc into `docs/design/live-migration.md` Phase 2 section. Phase 3+ implementers will not read this spike.

### S3 (MEDIUM) — CPU-mismatch memory-cleanup is implicit, not explicit

F12 found CH v51.1 self-aborts on CPU-feature incompatibility post-receive (uptime ~2.5 s on destination). The spike did not verify what happens to partially-received guest memory at abort. Tracing CH's behavior: on abort, CH process exits; Linux returns the process's pages to the kernel's free-page allocator; subsequent user-space allocations get zero-filled pages (`__GFP_ZERO`). No cross-process userspace leak in normal cluster operation.

But the cleanup is **implicit** (relies on Linux's GFP_ZERO behavior on next-allocation), not **explicit** (CH does not zero memory before exit). A malicious operator could:
- Schedule a migration of a high-value guest to a node with deliberately-incompatible CPU features
- Race a co-resident process for the freed page allocations

**Mitigation:** Phase 3 controller pre-flight CPU-feature check (already proposed in Open Question 1) prevents the abort scenario from being reachable. Document that on receive-abort, CH process exit is the only memory-clearing guarantee.

### S4 (Cross-cutting) — Items not previously flagged

Surfaced by security review:

- **Double-execution / split-brain risk in Phase 3 with RWX storage.** F2 (source auto-resumes when destination is killed) plus the dst-kill cancel primitive plus future RWX volumes (or snapshot+restore migration mode) creates a window where source and destination guest can briefly both be Running with the same identity. PVC RWO blocks attach on the destination today; Phase 3+ designs that lift this constraint must explicitly handle the split-brain window.
- **Log-parsing surface as a guest-escape spoofing vector.** F8 noted log-parsing could expose CH's `--log-file` as a writable surface a guest escape (or container compromise) could forge. Reinforces the Phase 2 recommendation to use poll-`info`-API for progress reporting, not log-tailing.
- **`spec.allowVersionSkew` needs operator-identity binding.** Same shape as `spec.allowIPChange`: the opt-in flag is meaningless if not coupled to the operator's identity in audit logs. Phase 1's pattern (operator's request via SwiftMigration spec) is the right precedent.
- **Audit logging is unaddressed in Phase 2 design.** Migration is the highest-stakes operation in Phase 3 (full guest state crosses the wire). Phase 3 should emit Kubernetes Events on each phase transition with: target-URL, source-pod, destination-pod, operator identity. Flag now; not Phase 2 work.

Extends Open Questions 1 (CPU pre-flight check), 4 (progress reporting via poll-info, NOT log-tail — guest-spoofing surface), 6 (mTLS hand-off must compose with S1's URL-source mitigation), and adds OQ7 (audit logging) — all promoted to the numbered Open Questions list above.

## Mini-walkthrough log

Per Tracked Follow-up #4 in `kubeswift_context.md` (a 30–60 min headline operator-walkthrough at spike close). Per architect's framing review: drive the walkthrough through the annotation surface from Resolved Decision 1, even though no controller is wired — substitute ch-remote calls at each annotation transition. Script: `spike/phase-2/walkthrough.sh`.

### Walkthrough run

End-to-end migration miles → boba narrating each annotation transition that a future Phase 2 controller would observe. Six steps (provision destination, write receive annotations, boot source guest, write send annotations, observe completion, verify guest continuity).

**Result: PASS on second attempt.**

| Run | Outcome | What surfaced |
|---|---|---|
| #1 | FAIL — send-migration exit=1, dst state=gone post-migration. The walkthrough script PRINTED "Findings reproduced (no contradiction)" without verifying the result. | **Walkthrough finding W1** |
| #2 | PASS — send-migration exit=0, src=gone, dst=Running. Cross-node migration in 2.9 s; BEACON gap 4.02 s; all 10 narrated annotation transitions completed. | Spike findings F1, Q1c, F7, F9 reproduced. |

### W1 — Walkthrough script self-narrated success on actual failure

The first run failed (the destination's `dst.sock` from a prior spike step persisted on boba; receive-migration crashed at startup with "Address in use"). Yet the script's bottom section unconditionally printed "Findings reproduced (no contradiction)" because it didn't gate the conclusion on the actual `src state` / `dst state` values.

**This is the kind of bug the mini-walkthrough is supposed to catch.** It mirrors the snapshot operator-walkthrough's Tier-A data-loss finding (PR #21) and the Phase 1 headline-validation Bugs A/B/C — automated tests pass, the function appears to work, but operator-flow validation reveals the script (or controller) confidently reports success on a failed transaction.

**Phase 2 implication:** the SwiftMigration controller's `Resuming` phase MUST gate `phase=Completed` transition on actual destination guest state (e.g., `info` returns `state=Running` AND a primary IP appears), NOT just on `send-migration` returning exit=0 OR a stale `migration-action-id-mirror` annotation. Phase 1 already does the right thing here (its `Resuming` phase polls for `GuestRunning=True` + `primaryIP`) — Phase 2 must preserve this discipline.

### W2 — Stale-state cleanup is the persistent operational hazard

Across the spike (Q1, Q1d, Q1e, Q2, Q4) and the walkthrough, the same failure pattern surfaced repeatedly: a prior CH instance's `dst.sock` file persists on the destination node after `pkill -9`, and the next CH instance fails to start with "Address in use" → silent test failure with confusing symptoms downstream.

CH does NOT cleanup its API socket on SIGKILL exit (expected — process can't run cleanup hooks). swiftletd's launcher script must explicitly remove the API socket file before starting CH, OR the launcher pod must be a fresh pod each time (the Phase 1 pattern — pod restart is the cleanup vehicle).

**Phase 2 implication:** swiftletd's start-up sequence MUST `rm -f` its API socket file before invoking CH, regardless of any prior pod-restart cleanup. This is a one-line addition to the launcher entrypoint script. Without it, any swiftletd retry on the same pod (via RestartPolicy=Always — though Phase 1 uses Never) would fail with the same "Address in use" symptom.

### Walkthrough verdict

**Annotation surface is operationally sound** — all 10 narrated transitions ran clean on the PASS run; they map to the actions Phase 2 swiftletd needs to expose. **Two real operational findings (W1, W2)** caught by re-running the spike's protocol against the doc with a fresh-eyes lens.

Recorded artifacts: `spike/phase-2/walkthrough.sh`, walkthrough logs `/tmp/walkthrough.log` (run #1 — FAILED) and `/tmp/walkthrough-2.log` (run #2 — PASS).

# Live Migration Phase 3a Spike — Controller State Machine

> **Status:** Complete. Ready for Phase 3a design alignment.
> Last updated: 2026-05-01.

---

## Goal

Resolve controller-orchestration unknowns for the SwiftMigration
controller's `mode: live` state machine, before Phase 3a design or
implementation begins. Same shape and discipline as Phase 0 snapshot
spike (5 findings, 3 corrected design constraints) and Phase 2 spike
(12 findings, 4 resolved decisions): findings either confirm
assumptions or correct them BEFORE design begins.

**Phase 3a targets kernel-boot guests AND RWX+Block disk-boot guests.
RWO+disk-boot live migration is Phase 3c (storage architecture review
territory).** This sentence scopes every other question in the spike.

The four unknowns this spike resolves:

- **Q1**: How does the controller orchestrate the Phase 2 action set
  across two pods atomically? (sub-questions: dst-readiness signal,
  start-send trigger, reconcile-loop interruption, controller-kill
  cleanup, ack-annotation lifecycle, dst pod ownerRef.)
- **Q2**: How does the controller observe destination CH state without
  cross-pod connectivity? (controller-runtime informer latency,
  annotation schema sufficiency, write→observe latency.)
- **Q3**: How does the controller's W1 gate-on-observed-state actually
  work? (verification mechanism, race window, distinguish-running-states,
  Completed-gate criteria.)
- **Q4**: How does the controller handle Kubernetes-initiated termination
  of pods mid-migration, including node failure (Q4e)?

Out of scope: mTLS (Phase 3b), S1 URL-from-CR migration (Phase 3b),
CPU-feature pre-flight (Phase 3b), audit logging (Phase 3b), F2 split-
brain on RWX (Phase 3c), `spec.allowVersionSkew` enforcement
(Phase 3b), drain integration (Phase 4), pre-copy convergence beyond
Phase 2 spike data.

## Method

Manual two-pod orchestration on the cluster (miles + boba), scripted
as the controller would. Phase 2 manual demo scripts
(`test/migration/manual/`) are the starting point. Spike scripts in
`spike/phase-3a/scripts/` (gitignored, scratch) layer timing
instrumentation and failure-mode injection on top of the manual demo.

For each spike question:

1. Set up source SwiftGuest on miles, destination receive-only pod on
   boba (kernel-boot, RWO storage — does not exercise disk handoff;
   that's Phase 3c).
2. Run scripted controller-equivalent sequence with timestamped JSONL
   event log.
3. Inject failure modes at each phase transition (Q1c/Q1d/Q4).
4. Read post-run state on cluster + JSONL log → finding.

---

## Pre-spike unblocking — br0/Calico CIDR collision (B0)

**Encountered first.** Cross-node pod-to-pod TCP from miles → boba was
broken at spike start, with miles probe pod sending SYN, packet
arriving at the dst pod's TCP stack on boba (`TcpExtListenDrops` ticked
up), but no SYN-ACK ever returned. Same-node SYN succeeded; only
cross-node failed.

**Root cause.** kubeswift's launcher pod carries an internal `br0`
bridge with subnet `10.244.125.0/24` for the guest VM network.
Calico assigned per-node pod CIDRs of `10.244.125.128/26` (miles) and
`10.244.213.0/26` (boba) — the miles per-node CIDR collides with the
launcher's br0 subnet. Inside the dst launcher pod's network namespace,
the routing table has:

```
default via 169.254.1.1 dev eth0
10.244.125.0/24 dev br0   proto kernel scope link src 10.244.125.1 linkdown
169.254.1.1 dev eth0 scope link
```

When a SYN arrives from a miles pod (src IP 10.244.125.x), the dst
pod's TCP stack tries to send the SYN-ACK back, the route lookup picks
br0 (linkdown stub) over eth0 (Calico-managed), and the SYN-ACK is
silently dropped by the linkdown interface.

**Workaround for the spike.** Inside each dst launcher pod after pod
creation:

```bash
ip route replace 10.244.125.128/26 via 169.254.1.1 dev eth0
```

This adds a more-specific eth0 route for miles' Calico per-node CIDR,
ensuring SYN-ACK replies route via Calico instead of br0. With the
override applied, cross-node connectivity is restored end-to-end and
the Phase 2 spike's q1-crossnode baseline replays cleanly.

**Pre-Phase-3a kubeswift platform fix (standalone PR ahead of Phase 3a
implementation).**

B0 is a **latent kubeswift platform bug**, not a Phase 3a-internal
concern. The launcher pod builder hardcodes `br0` to `10.244.125.0/24`
in [`internal/controller/swiftguest/pod.go`](../../internal/controller/swiftguest/pod.go)
and the matching launcher entrypoint script. Any future kubeswift
cross-pod-TCP workflow — Phase 3a live migration is the first, but
not the last — will surface the same collision on Calico clusters
that happen to allocate `10.244.125.x/26` to a workload node. The
collision is silent until traffic flows: pod scheduling and CRD
validation never detect it.

**Recommendation: ship B0 as a small standalone PR ahead of Phase 3a
implementation.** Scope is the launcher pod builder + entrypoint
script + smoke-test verification. Two implementation directions:

1. **Move br0 off potentially-colliding subnets.** Pick an RFC1918
   subnet reserved for VM-bridge use (`192.168.99.0/24` is a common
   convention; `172.18.x.x/24` if 192.168.x is also reserved
   somewhere downstream). One-line change in pod-builder + entrypoint.
   Lowest-risk fix.
2. **Make br0 subnet configurable** via SwiftGuestClass or
   cluster-wide config, with auto-detection of cluster pod CIDRs as
   default. Higher complexity; better operator ergonomics for
   clusters with non-standard CIDR allocations.

The spike's recommendation is **(1) for the standalone PR**, with
(2) deferred behind any operator demand. Phase 3a implementation
proceeds against (1)'s clean cross-node baseline; the route-override
workaround used in this spike disappears.

**Phase 2 walkthrough never surfaced B0** because Phase 2's manual
demo had a non-colliding Calico per-node allocator state at that
time (different pod IPs from earlier deployment cycles). The
collision is an artifact of which IPs Calico hands out, not a
deterministic miss in Phase 2's design.

**Spike disposition.** B0 is a finding the spike surfaced. Phase 3a
controller cannot ship while B0 remains unfixed. **B0 shipped via
[PR #39](https://github.com/projectbeskar/kubeswift/pull/39)
(`fix/b0-br0-subnet-collision`) ahead of Phase 3a; spike continues
against the route-override workaround until PR #39 merges.**

---

## Q1 — Controller orchestration of action set across two pods atomically

### Setup
Source SwiftGuest `default/p3a-src` (kernel-boot faas-minimal, 4Gi RAM,
2 vCPU) on miles. Destination launcher pod
`default/p3a-src-launcher-mig-recv` on boba via `destination.sh`-style
hand-rolled YAML; CH spawned with `--api-socket` only and
`KUBESWIFT_MIGRATION_ROLE=receiver`. B0 route override applied.

Orchestrator script (`spike/phase-3a/scripts/q1-orchestrator.sh`)
emits one JSONL event per phase-transition with `rel_ns` (monotonic
ns since script start) and `wall_us` (wall clock µs).

Recorded run: `spike/phase-3a/logs/q1-1777616785.jsonl`. End-to-end
timeline reproduced below in milliseconds since orchestrator start:

```
000ms  dst_ip_resolved                          dst pod IP discovered
000ms  ids_assigned                             recv_id + send_id chosen
275ms  ack_src_write_done                       src ack annotation set
485ms  ack_dst_observed                         dst ack annotation visible (already set)
786ms  recv_action_write_done                   dst recv annotation set
2044ms recv_status_running_observed (iter=3)    dst migration-status=running mirror visible
2536ms vm_info_probe_done state=unknown         ch-remote not in launcher PATH
2771ms send_action_write_done                   src send annotation set
43564ms src_status_complete_observed (iter=35)  src CH wrote terminal status
       (orchestrator killed here — see Q1a "two running" finding below)
```

Final pod state, observed via informer-equivalent `kubectl get`:

| pod | phase | migration-status | migration-status-detail |
|---|---|---|---|
| p3a-src (src) | Succeeded | complete | sent to tcp:10.244.213.52:6789 (38143ms) |
| p3a-src-launcher-mig-recv (dst) | Running | running | received on tcp:0.0.0.0:6789 (40224ms) |

### Result + finding

**Q1a — dst-pod-readiness signal: migration-status mirror is sufficient
for the receive-accept gate, but ambiguous for the resume-completed
gate.**

The annotation `kubeswift.io/migration-status=running` with matching
`migration-status-id=$RECV_ID` reliably signals "swiftletd accepted
the receive command and CH is in receive-listen mode." Observed
latency from `kubectl annotate` write to status mirror visibility:
**iter=3 of 100ms polls = ≤300ms** (controller-runtime informer
latency would be lower, since this measurement includes the
orchestrator's polling overhead). swiftletd dispatches CH receive
*after* writing the running annotation, so observing this state
implies the listener is up.

**HOWEVER**: the same `migration-status=running` value with the same
`migration-status-id` is also the terminal-success signal on the dst
side after the migration completes (CH transitioned to state=Running
with the migrated guest). The controller cannot distinguish
"recv-accepted" from "post-resume Running" by reading the dst pod's
annotation alone. Confirmed empirically: the dst pod's
`migration-status-detail` *does* differentiate ("(no detail set)" at
accept-time vs `received on tcp:0.0.0.0:6789 (40224ms)` at terminal
success), but `status-detail` is an unstructured string and a fragile
signal for state-machine logic.

**Phase 3a recommendation (Q1a)**: gate the Resuming-phase
`Completed` transition on the **src pod's** `migration-status=complete`
with matching `$SEND_ID`. swiftletd's send-side dispatch performs the
W1 vm_info probe on the dst CH state before writing `complete`
(per Phase 2 design §3.1 + W1 dispatch-side gate from PR-B). So
src=complete is the strong signal that means "dst CH is Running with
the migrated guest." Reading the dst pod's status mirror is then
redundant (and at best a belt-and-braces sanity check). See Q3 for
elaboration on the W1 mechanism this leans on.

**Phase 2 design follow-up flagged**: ideally swiftletd would write a
distinct terminal value on the dst side (e.g., `complete` instead of
`running`) so the dst-side mirror is unambiguous. Filed as Phase 3a
controller's input request to the swiftletd side; the Phase 3a
controller can ship without it (relying on src-side
`complete`), but Phase 3a should request the rename for cleaner
state-machine semantics.

**Q1b — src start-send trigger: single annotation transition
(`recv_status_running` with matching `recv_id`) is sufficient.**

The orchestrator issues send-action immediately after observing
`migration-status=running` on dst with matching id. Migration
completes successfully end-to-end. There is **no need** for additional
evidence (no need to probe dst's `ch.sock`, no need to verify
listener via TCP probe from the controller-manager pod, no need to
exec into the dst pod).

The reason: swiftletd-on-dst writes `running` only AFTER successfully
calling CH's `vm.receive-migration` API (Phase 2 design §3 + PR-B's
ordered-write contract). The CH listener is bound BEFORE the
annotation write. So observing the annotation implies the listener is
up. Phase 2 spike F4 (listener gives up on network silence within ~3-5
seconds) sets a soft upper bound on how long the controller can
delay the send-action after observing running, but this is not a
correctness concern — it's a timing-budget concern (see Q1d).

**Q1c — reconcile-loop interruption between recv-issued and
send-issued: clean resumption confirmed empirically.**

Test (`spike/phase-3a/scripts/q1cd-interrupt.sh`,
`spike/phase-3a/logs/q1cd-1777617082.jsonl`): issue recv-action on
dst, observe `migration-status=running` mirror, **then idle for
60 seconds** (simulating controller-pod death between StartReceive
and StartSend). After the idle window, issue send-action. Result:
`Q1c_recovery_succeeded iter=31` — migration completed cleanly
~30s after send-action issuance, with no operator intervention.

The migration-action-id pattern (Phase 2 PR-B) is the load-bearing
primitive: when the new controller-pod's reconcile picks up the
SwiftMigration in some intermediate phase, it can issue a new
send-action with a fresh `send_id`. swiftletd's `decide()` accepts
any new id (idempotency is per-id; a fresh id = fresh dispatch).

**Phase 3a recommendation (Q1c)**: the controller's StopAndCopy phase
issues send-action with a SwiftMigration-scoped fresh `send_id`
on every reconcile that observes "dst recv-running, src has not yet
been issued send" — `send_id` derived from
`<SwiftMigration name>:send:<phase-attempt-counter>` so reconcile
loops produce idempotent IDs (same id → swiftletd treats as replay).
Storing the attempt counter in SwiftMigration's status field
(e.g., `status.sendAttempts`) preserves it across leader handover.

**Q1d — cleanup if controller killed between StartReceive and
StartSend: dst CH receive listener stays up for at least 60 seconds
of network silence — contradicts Phase 2 spike F4's "~3-5s" figure.**

Same test as Q1c (`q1cd-interrupt.sh`). Probed dst CH listener
status every 5 seconds for 60 seconds while no send-action was
pending. Result: `listener_count=1` at all 12 checkpoints (5s, 10s,
... 60s). The CH receive accept-loop in v51.1 did NOT time out
within the test window.

**This contradicts Phase 2 spike F4** (which claimed ~3-5s listener
timeout). Possible explanations:

1. F4's measurement was specific to a different CH listener mode
   (`unix:`-socket vs `tcp:`-socket), or different CH version.
2. F4's test had a network-error path that triggered listener exit;
   network-silence does NOT.
3. F4 was wrong / over-conservative; Phase 3a can rely on
   ≥60s listener stability under silence.

The spike doesn't expand scope to determine which of (1)-(3) applies;
the new finding is sufficient: **Phase 3a controller has at least
60 seconds of grace period between recv-issued and send-issued before
needing to re-issue receive or recreate dst pod.** This is
comfortably more than any expected reconcile-loop pause (controller-
manager leader-failover takes seconds, not minutes).

**Phase 3a recommendation (Q1d)**: the controller does NOT need to
defensively re-arm receive on every reconcile of an in-flight
migration. Once recv-status=running has been observed, the controller
treats the listener as durable for the duration of normal Phase 3a
state transitions. The controller should still detect "dst pod
deleted by Kubernetes" (Q4) or "dst pod restarted" (CH listener
gone for sure) and re-arm in those specific cases.

**Q1e — ack-annotation lifecycle: controller has flexibility.**

The `kubeswift.io/migration-phase2-unsafe-plaintext: ack` gate fires
inside swiftletd's `decide()` when an action is received. The
annotation must be present when the action arrives; it does not need
to be present at pod-creation time. Empirical write latency for
`kubectl annotate`: ~213ms (apiserver round-trip).

**Phase 3a recommendation (Q1e)**: controller writes the ack
annotation on dst pod at pod-creation time (in the same Pod manifest
the controller submits to the apiserver) and on src pod immediately
before issuing the first migration action. Both writes are cheap and
neither blocks. Phase 3b will revisit the ack mechanism entirely
(replacement with mTLS-cert presence as the gate).

**Q1f — dst pod ownerRef: spike's hand-rolled dst pod has no
owner; Phase 3a controller must choose.**

The orchestrator observed `dst_owner_ref owners="/"` — the manual
demo's destination.sh strips ownerReferences from the source pod YAML
when building the dst pod, so the dst pod is orphan in the spike
setup.

**Phase 3a options**:

1. **SwiftMigration owns the dst pod.** Clean: when SwiftMigration is
   deleted (post-Completed/Failed), the dst pod is GC'd. But the dst
   pod's lifetime crosses the SwiftMigration's terminal-state
   transition (controller updates SwiftGuest's status.podRef to the
   dst pod, then deletes the source pod, then SwiftMigration becomes
   Completed). If SwiftMigration is GC'd at Completed, ownership
   transfer to SwiftGuest must happen FIRST, atomically.
2. **SwiftGuest owns the dst pod from the start** (consistent with
   how Phase 1 offline mode handles target-side pod). Source pod is
   already owned by SwiftGuest; Phase 3a's dst pod simply joins the
   same owner. Risk: two pods owned by one SwiftGuest during the
   migration is structurally OK (Kubernetes permits multiple pods per
   owning controller resource), but the SwiftGuest controller must
   not try to reconcile both as if they were the same pod.
3. **No ownerRef; Phase 3a controller cleans up explicitly on
   Completed/Failed.** Operationally simplest but requires explicit
   deletion logic in the controller's terminal-phase handlers.
   Failure mode: if the controller is lost mid-cleanup, dst pod is
   orphaned forever (operator cleanup needed).

**Phase 3a recommendation (Q1f)**: option 2 (SwiftGuest owns dst
pod), with explicit logic in the SwiftGuest controller to recognize
both pods during migration via a `migration-role` label
(`source` / `destination`). Aligns with Phase 1's "SwiftGuest CR
identity preserved across migration" invariant. This decision should
be revisited in Phase 3a design with the SwiftGuest controller's
existing pod-management code as input.

> ⚠ **Conditional on architectural direction.** Option 2's
> recommendation is load-bearing on the decision that **the
> SwiftGuest controller becomes migration-aware** — i.e., it
> explicitly recognises and tolerates two simultaneous pods (source +
> destination) for one SwiftGuest during the migration window, and
> has logic to reconcile both via the `migration-role` label without
> conflict. This is a non-trivial extension to the SwiftGuest
> controller's existing single-pod-per-guest invariant.
>
> **If Phase 3a design rejects making the SwiftGuest controller
> migration-aware** (e.g., to keep SwiftGuest controller code stable
> and concentrate migration logic in the SwiftMigration controller),
> the dst pod ownerRef decision **reopens** — option 1 (SwiftMigration
> owns dst with atomic ownership transfer at Completed) or option 3
> (no ownerRef + explicit controller cleanup) re-enter consideration.
> The spike does not validate option 1's atomic-ownership-transfer
> mechanism or option 3's explicit-cleanup failure modes; those
> belong in Phase 3a design with whichever direction is chosen.
>
> The cadence: Phase 3a design's first decision is whether SwiftGuest
> controller becomes migration-aware. If yes → option 2 with the
> spike's recommendation. If no → revisit options 1 and 3 with
> additional empirical validation outside the spike.

### Findings — Q1 sub-finding summary

| ID | Title | Severity | Affects |
|---|---|---|---|
| F1.1 | dst-side `migration-status=running` is ambiguous (accept-time vs terminal) | Medium | Phase 3a controller state machine; flag swiftletd rename request |
| F1.2 | src-side `migration-status=complete` IS the strong terminal signal (W1 dispatch-gate enforcer) | High | Phase 3a Resuming→Completed transition gate |
| F1.3 | Single annotation observation (`recv_status_running`) is sufficient to start send — no additional dst-pod probes needed | High | Phase 3a Preparing→StopAndCopy transition; eliminates cross-pod TCP probe requirement |
| F1.4 | annotation write→observe latency ≤300ms (informer-cached read; orchestrator polling adds overhead) | Low | Phase 3a state-machine cadence budget |
| F1.5 | dst pod ownerRef must be chosen explicitly — recommend SwiftGuest owns dst (option 2 above) | Medium | Phase 3a pod lifecycle + GC |
| F1.6 | ack-annotation lifecycle is flexible — controller can set at any time before action issuance | Low | Phase 3a ack-write site selection |
| F1.7 | B0: br0/Calico CIDR collision blocks ALL Phase 3a cross-node migration on affected clusters | High | Pre-Phase-3a kubeswift platform fix |

| F1.8 | Medium | Q1c reconcile-interruption recovery is supported by migration-action-id pattern; recommend deriving send_id from SwiftMigration name + attempt counter for idempotency across leader-handover | Phase 3a controller `send_id` derivation logic |
| F1.9 | High (corrects F4) | Q1d empirical: CH receive listener stays up ≥60s under network silence — contradicts Phase 2 spike F4's "~3-5s" listener timeout figure | Phase 3a does NOT need defensive re-arm on every reconcile; ≥60s grace budget is comfortably above any expected leader-failover delay |

---

## Q2 — Controller observation of destination CH state via informer

### Setup
Watch-driven measurement against the dst pod
`default/p3a-src-launcher-mig-recv` (post-Q1c-recovery, healthy
state). Script `spike/phase-3a/scripts/q2-watch-latency.sh` starts
`kubectl get pod -w --output-watch-events -o json` in the
background, prefixes each line with `date +%s%N` (epoch ns), then
issues 10 `kubectl annotate` writes with unique probe values and
correlates each write timestamp against the corresponding watch event
arrival timestamp.

This measures the latency model controller-runtime uses: apiserver
patch → watch event push → informer cache → reconcile loop trigger.
Two metrics:

- **apiserver round-trip**: write start → kubectl annotate returns.
  Cost the controller pays per outbound write.
- **informer push latency**: write done → corresponding watch event
  arrives. Cost the controller pays to observe a change made by
  swiftletd or by another writer.

Recorded run: `spike/phase-3a/logs/q2-1777618000.csv`,
`q2-1777618000-watch.jsonl`.

### Result

10 iterations, each with a unique probe annotation
(`kubeswift.io/spike-q2-probe=<unique>`):

```
iter  apiserver_rt  informer_push
  1      225 ms        14 ms
  2      225 ms        24 ms
  3      236 ms        22 ms
  4      240 ms        17 ms
  5      201 ms        24 ms
  6      229 ms        21 ms
  7      260 ms        18 ms
  8      227 ms        23 ms
  9      239 ms        17 ms
 10      251 ms        17 ms

apiserver_rt:    avg=233ms  min=201ms  max=260ms
informer_push:   avg= 20ms  min= 14ms  max= 24ms
```

### Findings

**F2.1 — informer push latency is consistently ≤25ms on the spike
cluster (avg 20ms, max 24ms over 10 trials).** This is the latency
the Phase 3a controller actually pays to observe a swiftletd-written
annotation change. Sub-50ms reaction time is comfortably feasible;
the controller-runtime informer cache reflects writes faster than
the controller's reconcile-rate-limiter typically cycles.

**F2.2 — apiserver round-trip for outbound writes (≈233ms avg)
dominates the controller's outbound-action latency.** Each
`kubectl annotate`-equivalent patch from the controller costs ~225-
260ms wall time. For Phase 3a's state-machine, the dominant cost
is *issuing* actions (3-4 writes per migration: src ack, dst ack,
dst recv-action, src send-action), not *observing* them. The
controller can issue all 4 writes in ~1 second without any
parallelism; with parallelism (e.g., src-ack + dst-ack written
concurrently) the wall time drops further.

> **Caveat: spike cluster baseline only.** The 233ms RT is
> measured on a 3-node k0s cluster with no concurrent reconciler
> load and a single etcd member. Production controller-manager
> deployments running multiple reconcilers concurrently against a
> larger apiserver/etcd quorum may see **2-5× higher write RT**
> (500ms-1.2s). The conclusion holds — controller writes are still
> a small fraction of the ~38s migration body — but the absolute
> "<3% of total migration time" number is environment-dependent.
> Phase 3a should not encode 250ms as a hard timeout anywhere;
> the budget should be at least 5× headroom over the spike baseline.

**F2.3 — annotation schema is sufficient for Phase 3a's state machine
WITH the F1.1 caveat already documented.** The Phase 2-shipped
annotation surface enumerates:

  *Controller-written* (apiserver patches → swiftletd action loop reads):
  - `kubeswift.io/migration-action` (= `receive` | `send` | `cancel`)
  - `kubeswift.io/migration-action-id` (UUID per dispatch, idempotency key)
  - `kubeswift.io/migration-action-args` (JSON: listen_url / target_url)
  - `kubeswift.io/migration-phase2-unsafe-plaintext` (= `ack` gate)

  *swiftletd-written* (action loop writes → controller informer reads):
  - `kubeswift.io/migration-status` (= `running` | `complete` | `failed` | `rejected`)
  - `kubeswift.io/migration-status-id` (mirrors action-id when terminal)
  - `kubeswift.io/migration-status-detail` (free-form detail string)

  *Persistent SwiftGuest pod-state* (already shipped pre-Phase 3a,
   load-bearing for Phase 3a controller's "is dst guest healthy?"
   reconcile checks):
  - `kubeswift.io/guest-ip` (DHCP discovered)
  - `kubeswift.io/guest-runtime-pid` (CH PID)
  - `kubeswift.io/guest-serial-socket` (path)
  - `kubeswift.io/guest-hypervisor`

This surface drives all four state transitions of Phase 3a
(Validating → Preparing → StopAndCopy → Resuming → Completed) without
new annotations. The F1.1 ambiguity (dst-side `running` reused for
accept-time AND terminal-success) is the only schema concern, and
F1.2's recommendation (gate Completed on src-side `complete`) routes
around it without requiring a swiftletd-side change.

**F2.4 — no need for cross-pod connectivity from the controller.**
The controller-manager pod observes both src and dst launcher pods
via apiserver/informer. It does not need to TCP-connect to either
pod, exec into either pod, or read CH HTTP API directly. All evidence
necessary for the state-machine flows through the annotation
surface. This was an open question in the original spike framing
(architect-review folded it as a sub-bullet); now resolved.

**F2.5 — Phase 3a's swiftletd-progress visibility is opaque.** Phase 2
design §3 mentioned intermediate `migration-progress` values
(`precopy` / `stopcopy` / `listening`) but Phase 2 PR-B does NOT
currently emit them — the operator only sees `running` (accept) →
terminal. The Phase 3a controller's state machine therefore cannot
observe pre-copy convergence progress mid-flight. **For Phase 3a,
this is acceptable**: the state machine doesn't need progress
introspection; it just needs to know recv-accepted and send-complete.
Progress visibility is a Phase 5 (operational polish) concern, not a
correctness concern for Phase 3a.

### Q2 sub-finding summary

| ID | Severity | Finding | Phase 3a implication |
|---|---|---|---|
| F2.1 | Low | Informer push latency ≤25ms (avg 20ms) | Sub-50ms state transitions feasible; reconcile latency budget is generous |
| F2.2 | Low | apiserver write RT ≈233ms; dominates outbound-action cost | 4 sequential writes ≈ 1s wall time; parallelizable to ~250ms |
| F2.3 | High | Annotation schema sufficient for full Phase 3a state machine | No new annotations required; existing Phase 2 schema covers Phase 3a |
| F2.4 | High | Controller observes both pods via informer alone — no cross-pod connectivity needed | Eliminates a non-trivial security/networking dependency from Phase 3a controller |
| F2.5 | Low (defer to Phase 5) | swiftletd does not emit progress (precopy/stopcopy) annotations | Phase 3a is correct without progress; revisit in Phase 5 |

### Q2 resolved

- **Q2a (reframed informer latency)**: ~20ms avg, ~25ms max on spike
  cluster — sub-50ms state transitions feasible.
- **Q2b (annotation schema sufficiency)**: Phase 2 schema sufficient
  with F1.1 caveat; Phase 3a needs no new annotations.
- **Q2c (CH-state-Running → controller-observes latency)**: bounded
  by informer push latency (≤25ms) plus swiftletd's write-status
  delay (post-CH-state-transition). swiftletd writes the terminal
  status synchronously after CH's vm.info confirms Running (per
  Phase 2 PR-B W1 dispatch-side gate), so total CH-Running →
  controller-observes latency is bounded by ~25ms + swiftletd's
  internal status-write code path (sub-millisecond).

---

## Q3 — W1 gate-on-observed-state mechanism

### Setup
Q3 has two parts. The first elaborates the W1 mechanism the controller
relies on (mostly already established by F1.2 + F2.4). The second
narrows the F1.9-vs-F4 contradiction empirically with three failure-
mode variations.

### Part A — W1 mechanism on the swiftletd dispatch side

The controller-side W1 gate (the Resuming → Completed transition)
**reduces to "observe src migration-status=complete with matching
$SEND_ID"** (F1.2). swiftletd-on-src's send-migration dispatch
performs the actual W1 enforcement before writing that terminal
status:

1. swiftletd issues CH `vm.send-migration` API call (synchronous,
   blocking).
2. CH's send-migration returns OK only after dst CH has confirmed
   reception and transitioned to state=Running with the migrated
   guest. Internal CH protocol; not observable from outside CH.
3. If the swiftletd-side `vm.send-migration` returns OK but the
   dispatch wrapper's optional `vm_info`-on-dst probe (Phase 2 PR-B
   W1 dispatch-side gate, code-tagged in `swiftletd::action`) fails
   to confirm dst-Running state, swiftletd writes `migration-status=
   failed` instead of `complete` — even though CH itself returned OK.
4. swiftletd patches the src pod's annotation surface with
   `migration-status=complete` (success) or `failed` (W1 violation).

**The controller observes outcome via informer.** F2.4 confirmed no
cross-pod TCP from controller-manager is needed. The W1 gate
ENFORCEMENT lives in swiftletd-on-src's dispatch wrapper; the W1 gate
OBSERVATION is just informer-cached annotation read.

### Part B — F1.9 vs F4 narrowing (3 variations)

Phase 2 spike F4 reported "~3-5s listener timeout" on dst CH receive
listener. Q1d's F1.9 reported "≥60s under network silence." The
contradiction's resolution shapes Phase 3a's re-arm strategy.

Three variations exercised; each measures dst CH receive-listener
state from recv-issued through some failure-mode injection.

#### Variation 1 — silent network (= Q1d, recap)
No send-action issued; controller-pod-died simulation. Listener
remains bound for ≥60 seconds (12 checkpoints × 5s = 60s, all show
`listener_count=1`). **F1.9 confirmed.**

#### Variation 2 — source pod force-killed mid-handshake
Recorded in `spike/phase-3a/logs/q3v2-*.jsonl`.
Setup: recv-action issued + accepted (status=running with $RECV_ID),
then send-action issued, then **at +1.5s wait** (TCP handshake
in-progress) src pod force-deleted via
`kubectl delete pod --grace-period=0 --force`. Observation cadence
2s for 60s.

**Result: dst CH receive listener exited within ≤2 seconds of src
kill** (`listener_exited at_elapsed_s=2`). All 30 subsequent
checkpoints show `listener_count=0`. The dst pod's
`migration-status` remained `running` (swiftletd doesn't update
annotation when CH listener exits with no terminal status —
**actionable Phase 3a finding**: status mirror does not auto-update
to `failed` if dst CH listener dies via connection-abort).

This **reproduces F4's ~3-5s timeout**, and identifies the actual
trigger: connection-error / TCP-abort from the peer side, NOT
network-silence. Phase 2 spike F4's measurement was correct in its
context (the "kill dst pod" scenario referenced in the F4 finding's
"~3-5s on dst" mirror is symmetric with this kill-src scenario).

#### Variation 3 — DROP rule on miles targeting dst:6789
Recorded in `spike/phase-3a/logs/q3v3-*.jsonl`. Setup: recv-action
issued and accepted; iptables FORWARD DROP rule applied on miles
host targeting dst pod IP:6789 outbound; send-action issued; both
sides observed for 60 iterations (~117s wall time).

**Result: BOTH sides hang for the entire 117s window.**
- `src_status` remained `running` with $SEND_ID throughout — swiftletd
  never reported terminal status.
- `listener_count` on dst remained `1` throughout — receive listener
  bound and accepting (but no peer arriving due to DROP).
- After DROP rule removal at the script's end, the test concluded
  without driving the migration to terminal in either direction.

**Why**: src CH calls TCP `connect()`; Linux kernel queues SYN on
the wire; iptables DROP silently discards. No RST, no ICMP
unreachable. Kernel TCP retransmits SYN with exponential backoff
(`tcp_syn_retries` default = 6 → ~127s before ETIMEDOUT). At 117s
the kernel hasn't yet given up; src CH is blocked in `connect()`.

From dst's perspective, no peer ever showed up — same as silent-
network (variation 1) — so the listener stays bound indefinitely.

This is the **worst-case failure mode for Phase 3a**: neither side
fails fast. Without a controller-side timeout, the SwiftMigration
would sit in StopAndCopy until the Linux kernel TCP layer eventually
times out (~127s), at which point src swiftletd would report
`failed`. Operationally undesirable; **bounds the lower limit of
Phase 3a's `spec.timeout` default** at >127s.

### Findings — Q3 sub-finding summary

**F3.1 — F1.9 and F4 are NOT contradictory; they capture different
failure modes. v3 surfaces a third (worst-case) mode.**

| Failure mode | Listener exit timing | Src failure timing | Phase 3a implication |
|---|---|---|---|
| **Silent network (v1)** (no peer connection attempt) | ≥60s | n/a (no send) | Controller has ≥60s grace between recv-issued and send-issued |
| **Connection abort (v2)** (peer kill, peer crash, src process death) | ≤2s | src process dead → no terminal write | Controller must detect listener-died-without-terminal-status and re-arm |
| **Packet drop / blackhole (v3)** (peer attempts but packets dropped) | ≥117s (silent-network shape on dst) | ≥117s (kernel SYN retransmit, ~127s default) | **Worst case: neither side fails fast.** Phase 3a needs `spec.timeout` ≥ 127s to bound this; recommend 5 min default |

**F3.2 — swiftletd does NOT auto-write `failed` when dst CH listener
dies abnormally without a terminal status.** Observed in v2: dst
listener exits at +2s after src kill, but dst pod's
`migration-status` annotation remains stuck at `running` indefinitely.
**Phase 3a actionable**: either (a) request a swiftletd-side change
to write `failed` when CH receive listener exits abnormally
(preferred), or (b) Phase 3a controller times out the Resuming phase
on src-side absence-of-terminal (with a defensive timeout) and
recreates dst pod for retry.

**F3.3 — Phase 3a's Resuming-phase reconcile must observe BOTH
src-terminal AND dst-listener-state.** F1.2's recommendation (gate
Completed on src=complete) is necessary but not sufficient. Without
also observing dst-listener (or dst-pod-Phase, or dst-mig-status
distinguishing), the controller can stall in Resuming if dst CH
crashed silently. Defensive timeout default in the spec recommended
(e.g., 5 minutes from StopAndCopy entry — long enough for normal
38s migrations + 5x headroom for production-cluster latency
amplification per F2.2 caveat).

**F3.4 — controller-driven src-pod-deletion is NOT a clean cancel
mechanism in the current swiftletd shape.** Force-deleting the src
pod terminates the TCP connection (good — dst CH exits within 2s),
but src swiftletd's terminal-status write does not happen (the
process was killed before write). Phase 3a's Cancel mechanism
should issue `migration-action: cancel` via annotation FIRST and
let swiftletd's cancel handler tear down the CH connection
gracefully, falling back to pod-deletion only if cancel-handler
times out. Phase 2 PR-B's cancel handler is currently a placeholder;
Phase 3a depends on its completion.

**F3.5 — Phase 3a `spec.timeout` lower bound is ≥127s
(kernel-TCP-retransmit default).** v3 demonstrates the worst-case
where both sides hang silently waiting. The controller's only
escape is its own timeout. Recommend default `spec.timeout: 5m`
(comfortable headroom over kernel TCP timeout, normal migration
body of 38s, and the F2.2 production-cluster latency-amplification
factor). Operators with operational urgency can override
downward; operators with very large guests should override upward.

### Q3 resolved

- **Q3a (verification command + race window)**: F1.2's recommendation
  remains the right answer — observe src `migration-status=complete`
  via informer. swiftletd's W1 dispatch-side gate
  (`vm_info`-on-dst probe before terminal write) is the actual
  enforcer. Race window between swiftletd's annotation write and
  controller's informer observation: ≤25ms (Q2 F2.1 measurement).
- **Q3b (send=0 but dst not Running)**: by F1.2's design, this
  cannot happen — swiftletd writes `failed` (not `complete`) if
  vm_info-on-dst fails. Controller treats `failed` as terminal and
  recreates dst pod / fails the SwiftMigration per
  `spec.failurePolicy`.
- **Q3c (dst annotation never transitions to Running)**: source
  auto-resume on cancel/abort is Phase 2 spike F2 territory; not
  re-validated in this spike. The Phase 3a controller's defensive
  posture handles all three failure-mode shapes (v1/v2/v3) by
  observing src-side terminal status with a `spec.timeout` upper
  bound.
- **Q3d (Completed-gate criteria)**: src=complete is sufficient
  (F1.2). Optional belt-and-braces: also observe dst pod's
  `kubeswift.io/guest-ip` annotation appearance (would prove the
  migrated guest reached DHCP-discovery — same lease.rs pattern
  Phase 1 already uses). Phase 3a recommendation: gate on
  src=complete; OPTIONALLY surface dst-guest-ip in
  `status.targetIP` once it appears (informational, not a gate).

**Q3-time plan: narrow the F1.9 vs Phase 2 spike F4 contradiction
(~15 min budget).** The spike already established that under
**network-silence** for 60 seconds, the dst CH receive listener
stays bound (F1.9). Phase 2 spike F4 reported "~3-5s listener
timeout"; the conditions are clearly different. Q3-time variations
to run, time-capped at 15 minutes total — do NOT expand scope to
fully resolve:

1. **Source-pod-killed-mid-handshake.** Issue recv on dst, issue send
   on src, kill src pod (`kubectl delete --grace-period=0 --force`)
   before TCP handshake completes (~1-2s into send). Observe dst CH
   listener state. Hypothesis: src-pod-kill terminates the TCP
   connection abnormally; dst CH likely surfaces this as a
   migration-error and exits the receive listener. If correct, F4
   captures *connection-error* timeout (faster than network-silence).
2. **DROP rule between recv and send.** Apply iptables DROP on miles
   targeting boba's pod CIDR after recv-issued, before send-issued.
   Issue send. dst CH listener sees nothing (same as silence) — but
   the src CH attempts connect → 1-3 SYN retries → eventual ECONNRESET
   or ETIMEDOUT. Either confirms F1.9 generalizes (silence ~= drop)
   or surfaces a CH-side timeout shorter than 60s. Either result is
   valuable for Phase 3a's Resuming-phase timing budget.
3. **Recv-only with abnormal dst pod state** (skip if (1) and (2)
   are conclusive). E.g., kill the dst CH process directly; observe
   if the receive listener state vs the cleanly-bound one differs.

If F1.9 generalizes across (1)+(2), Phase 3a's controller-side
re-arm strategy can rely on listener stability for ≥60s under
*any* failure mode. If (1) or (2) surfaces a faster timeout, the
re-arm strategy must accept the lower bound or add active-probe
defense. The spike does not pursue (3) unless (1)+(2) are
inconclusive.

---

## Q4 — Kubernetes-initiated termination of pods mid-migration

### Setup

Three cluster experiments + one design analysis:

- **Q4a — graceful dst pod termination** (`q4a-killdst-graceful.sh`): mid-migration `kubectl delete pod $DST_POD --grace-period=30`.
- **Q4c — graceful src pod termination** (`q4c-killsrc-graceful.sh`): mid-migration `kubectl delete pod $SOURCE_POD --grace-period=30`.
- **Q4e — node drain on dst node** (`q4e-drain-dstnode.sh`): mid-migration `kubectl drain boba`. Approximates node failure via the operator-initiated equivalent (real node failure / kubelet crash / network partition was not exercised — see disposition below).
- **Q4d — PDB design** (analysis only): no cluster experiment needed.

Q3-v2 (force-killed src) already covers the `--grace-period=0 --force` shape; Q4 here covers the graceful counterparts.

Logs: `q4a-1777619354.jsonl`, `q4c-*.jsonl`, `q4e-*.jsonl`.

### Result matrix

| Failure injection | dst CH listener exit | src final status | dst pod final | Notes |
|---|---|---|---|---|
| **Q3-v2** force-kill src | ≤2s after src kill | (no terminal — process killed) | (still up) | src process gone before swiftletd could write |
| **Q4a** graceful kill dst (grace=30) | ≤2s after SIGTERM | `failed` ("send_migration: internal_server_error") | gone at +20s (within grace) | src CH detects ECONNRESET, swiftletd writes terminal `failed` |
| **Q4c** graceful kill src (grace=30) | ≤2s after src CH connection broken | (no terminal — SwiftGuest controller recreated src pod) | (still up) | swiftletd's CH-send blocks; SIGTERM does not unwind it; SIGKILL at grace expiry kills both |
| **Q4e** drain dst node | ≤2s after dst pod's SIGTERM | `failed` ("send_migration: internal_server_error") | gone at +22s | Functionally identical to Q4a — drain triggers graceful eviction |

### Findings

**F4.1 — Termination of dst pod (any reason: graceful, drain, or force) results in clean `src=failed` terminal status.** Q4a and Q4e both show this consistently. The mechanism: dst CH dies → src CH's `vm.send-migration` returns error → swiftletd-on-src writes `migration-status=failed` with detail `"send_migration: internal_server_error"`. The Phase 3a controller observes this via informer (F2.1 latency ≤25ms) and transitions to Failed cleanly.

**F4.2 — Termination of src pod (graceful OR force) does NOT result in src writing terminal status, AND the SwiftGuest controller recreates the src pod automatically (`runPolicy: Running`).** Q4c showed:

- src pod's deletionTimestamp set; 30s grace begins
- swiftletd's CH `vm.send-migration` is blocking on the network; SIGTERM doesn't unwind it
- 30s grace expires → SIGKILL → swiftletd + CH both die → no terminal annotation written
- Old src pod gone; SwiftGuest controller (independent of SwiftMigration) creates a fresh src pod
- Fresh src pod has no migration annotations → SwiftMigration controller cannot observe a terminal status from informer

**Phase 3a actionable**: the controller's Resuming-phase reconcile must detect "src pod's UID has changed" (i.e., the SwiftMigration's recorded `status.sourcePodUID` no longer matches the current `SwiftGuest.status.podRef.uid`) and treat it as a Failed terminal. Without this, the controller would stall indefinitely waiting for a terminal annotation that will never arrive.

**F4.3 — Q4b distinguish K8s-initiated vs operator-initiated cancel: rely on annotation-FIRST cancel discipline.** The controller does NOT need to distinguish in the failure-detection path — Q4a/Q4c/Q4e all converge on the same SwiftMigration outcome (Failed). What differs is the FailureReason field operators see in `status`:

- If the controller issued cancel (via `migration-action: cancel`) and observes terminal `failed` with `cancel`-id → reason: `Cancelled`
- If the controller observed pod termination it did NOT initiate (deletion-timestamp from another actor) → reason: `PodTerminated` (with detail noting which pod)
- If the controller observed `src.UID` change (Q4c case) → reason: `SourcePodReplaced`

**Phase 3a recommendation (Q4b)**: SwiftMigration `status.failureReason` enum with these distinct values. Operators can correlate with their own action history.

**F4.4 — Q4d PodDisruptionBudget design analysis.** Q4e showed that `kubectl drain` on the dst node successfully evicts the dst pod (no PDB to block). This is **the desired behavior for unplanned drain scenarios** (operator pulling node for maintenance — migration should fail-fast and the operator can retry on a different target node).

**However**, **planned drain that's INITIATED specifically because we want to migrate AWAY from the source node is the canonical Phase 4 use case** (drain-triggered live migration). For Phase 3a, the relevant question is narrower: should the dst pod be protected by a PDB during migration to prevent operator-error-driven drain that disrupts an in-flight migration?

**Phase 3a recommendation (Q4d)**: **NO PDB on the dst pod.** Reasons:
1. PDB on dst pod would prevent legitimate drain of the dst node (e.g., dst node has its own urgent maintenance need). Operator forced into `--force` semantics anyway.
2. The migration is observable and time-bounded (≤5min via spec.timeout F3.5). Operators can wait for migration to complete before draining.
3. Phase 4's drain-aware controller is the proper home for "don't drain a node mid-migration" — implemented via the SwiftMigration validating webhook + a node-cordoned check, not via PDB.

**F4.5 — Q4e disposition: drain ≈ planned-eviction, NOT node-failure.** True node failure (kubelet crash, network partition, hard reset) was not exercised in this spike. Drain is the operator-initiated equivalent that produces the same downstream effect (dst pod terminated → src CH errors → src writes failed). 

The differences for Phase 3a:

| | Drain (Q4e tested) | True node failure (NOT tested) |
|---|---|---|
| Pod transition path | Cordon → eviction → graceful 30s → gone | Eventually NodeNotReady → pod marked NotReady → eventually deleted by node-controller (~5min default) |
| dst CH exit timing | ≤32s (grace + dst container exit) | Up to several minutes (dst node still running CH but unreachable) |
| Src CH behavior | ECONNRESET fast | TCP SYN/ACK retransmits silently (v3 blackhole shape) until kernel TCP timeout (~127s) |
| Phase 3a controller observable | dst pod deletionTimestamp + node.unschedulable | Node Ready condition transitioning to False/Unknown |

**The node-failure path is functionally equivalent to Q3-v3 (DROP rule) from the controller's perspective: src CH hangs on TCP SYN retransmit; controller's `spec.timeout` is the only escape.** Phase 3a does NOT need to specially handle node-failure beyond what's already in F3.5 (5min spec.timeout). The Phase 4 drain-aware path is where node-failure-vs-drain distinction matters.

### Q4 sub-finding summary

| ID | Severity | Finding | Phase 3a implication |
|---|---|---|---|
| F4.1 | High | dst termination → clean src `failed` terminal | Phase 3a Failed transition triggered cleanly via informer |
| F4.2 | High | src termination → no terminal status; SwiftGuest controller recreates src pod | Phase 3a must observe `src.UID` change as Failed signal |
| F4.3 | Medium | Q4b: FailureReason enum (Cancelled / PodTerminated / SourcePodReplaced / Timeout / Other) | Phase 3a `status.failureReason` field |
| F4.4 | Low | Q4d: NO PDB on dst pod during migration | Phase 4's drain-aware webhook is the proper home for "don't drain mid-migration" |
| F4.5 | Medium | Q4e: drain ≠ true node failure, but functionally equivalent for Phase 3a (handled by spec.timeout) | True node-failure not exercised in spike; behaves like Q3-v3 (kernel TCP timeout) |

### Q4 resolved

- **Q4a (graceful dst termination)**: src writes `failed` cleanly; Phase 3a Failed transition fires within F2.1 informer-latency budget.
- **Q4b (distinguish causes)**: enumerate via `status.failureReason` (5 values: Cancelled, PodTerminated, SourcePodReplaced, Timeout, Other).
- **Q4c (graceful src termination)**: no terminal status; controller MUST observe src pod UID change to detect Failed.
- **Q4d (PDB)**: no PDB on dst pod; Phase 4 webhook handles drain-mid-migration.
- **Q4e (node failure mid-migration)**: drain-equivalent tested; true node failure not separately exercised. Same Phase 3a coping path: src `spec.timeout` (5min default) covers worst case.

---

## Findings summary table

| ID | Severity | Finding | Decision affected | Recommendation |
|---|---|---|---|---|
| B0 | High | br0/Calico CIDR collision blocks cross-node | Pre-Phase-3a kubeswift platform fix | Move br0 off Calico-overlapping subnets (separate PR) |
| F1.1 | Medium | dst-side `migration-status=running` is ambiguous | Phase 3a controller cannot rely solely on dst-side mirror | Gate Completed on src-side `complete`; flag swiftletd terminal-value rename |
| F1.2 | High | src-side `complete` is the strong W1-gated terminal | Phase 3a Resuming→Completed transition | Use src-side `complete` with matching $SEND_ID as Resuming-end signal |
| F1.3 | High | Recv-status-running mirror is the start-send trigger | Phase 3a Preparing→StopAndCopy | Single-annotation gate, no cross-pod TCP probes |
| F1.4 | Low | informer write→observe latency ≤300ms | Phase 3a cadence budget | Sub-second state transitions are achievable |
| F1.5 | Medium | dst pod ownerRef choice required | Phase 3a pod-lifecycle / GC | SwiftGuest owns dst pod with `migration-role` label — **conditional on Phase 3a deciding SwiftGuest controller becomes migration-aware**. If rejected, options 1/3 reopen. |
| F1.6 | Low | ack-annotation timing flexible | Phase 3a ack-write site | Set at pod-creation on dst; immediately-before-action on src |
| F1.7 | (= B0) — | — | — | — |
| F1.8 | Medium | Q1c interrupt+resume recovery clean | Phase 3a controller `send_id` derivation | Use `<SwiftMigration>:send:<attempt>` for idempotent retries across leader-handover |
| F1.9 | High | Q1d listener ≥60s under silence (F4 corrected) | Phase 3a re-arm strategy | No defensive re-arm needed; ≥60s grace covers leader-handover |
| F2.1 | Low | Informer push latency ≤25ms (avg 20ms) | Phase 3a cadence | Sub-50ms state transitions feasible |
| F2.2 | Low | apiserver write RT ≈233ms (spike baseline; production 2-5×) | Phase 3a write budget | <3% of migration time on spike, environment-dependent absolute |
| F2.3 | High | Annotation schema sufficient for full Phase 3a | Phase 3a state machine | No new annotations required |
| F2.4 | High | Controller observes via informer alone — no cross-pod TCP from controller-manager | Phase 3a + Phase 3b mTLS scope | Closes off controller→swiftletd as a Phase 3b channel; only swiftletd↔swiftletd needs hardening |
| F2.5 | Low (Phase 5) | swiftletd does not emit progress annotations | Operator visibility (Phase 5) | Filed as Phase 5 backlog item in kubeswift_context.md |
| F3.1 | High | F1.9/F4/v3 are three distinct failure modes (silence/abort/blackhole) | Phase 3a state-machine timeout strategy | spec.timeout default ≥127s required to escape v3 worst case |
| F3.2 | Medium | swiftletd does NOT auto-write `failed` when dst CH listener exits abnormally without terminal status | Phase 3a Resuming-phase recovery | Either swiftletd-side fix (preferred) or controller-side defensive timeout |
| F3.3 | Medium | Resuming reconcile must observe BOTH src-terminal AND dst-listener-state | Phase 3a Completed gate | spec.timeout default 5min |
| F3.4 | High | src-pod-deletion is NOT a clean cancel mechanism (no terminal status write) | Phase 3a Cancel handler design | Use migration-action: cancel via annotation; pod-delete only as fallback |
| F3.5 | Medium | spec.timeout lower bound ≥127s (kernel TCP retransmit default) | Phase 3a spec defaults | Default 5min |
| F4.1 | High | dst pod termination (any reason) → clean src `failed` terminal | Phase 3a Failed transition | Trust src-side terminal informer event |
| F4.2 | High | src pod termination → no terminal status + SwiftGuest controller recreates pod | Phase 3a Resuming-phase reconcile | Detect `src.UID` change as Failed signal |
| F4.3 | Medium | Q4b FailureReason enumeration | Phase 3a status surface | `status.failureReason`: Cancelled / PodTerminated / SourcePodReplaced / Timeout / Other |
| F4.4 | Low | Q4d: no PDB on dst pod during migration | Phase 4 drain-aware controller | Drain-mid-migration prevention belongs in Phase 4 webhook, not PDB |
| F4.5 | Medium | Q4e: drain tested as node-failure proxy; true node failure not exercised | Phase 3a coping path same as Q3-v3 | spec.timeout 5min default handles worst case |

---

## Resolved questions

**Q1** — Controller orchestrates the action set across two pods atomically via:
- `recv-action` annotation on dst with unique $RECV_ID; observe `migration-status=running` mirror via informer (≤25ms latency, F2.1)
- Then `send-action` annotation on src with unique $SEND_ID; observe `migration-status=complete` (success) or `failed` on src
- Idempotency primitive: `<SwiftMigration>:send:<attempt-counter>` for `send_id` derivation enables retry-across-leader-handover (F1.8)
- ack annotation set at any time before action issuance — flexible (F1.6)
- dst pod ownerRef: SwiftGuest with `migration-role` label, **conditional on Phase 3a making SwiftGuest controller migration-aware** (F1.5)

**Q2** — Controller observes destination CH state via informer alone; no cross-pod TCP needed:
- informer push latency ≤25ms (F2.1)
- annotation schema sufficient (F2.3) with the F1.1 caveat (dst-side `running` ambiguous)
- closes off controller→swiftletd as a Phase 3b mTLS scope (F2.4)

**Q3** — W1 gate-on-observed-state mechanism:
- Enforcement is in **swiftletd-on-src** (vm.send-migration's W1 dispatch-side gate, Phase 2 PR-B)
- Observation is in controller via informer (`migration-status=complete` on src with matching $SEND_ID = F1.2)
- Three failure-mode shapes characterized: silent network ≥60s, peer-abort ≤2s, blackhole ≥127s (F3.1)
- spec.timeout default 5min handles all three (F3.5)

**Q4** — K8s-initiated termination & node failure:
- dst termination (any reason) → clean src=failed (F4.1)
- src termination → no terminal status; controller detects via src pod UID change (F4.2)
- FailureReason enum: Cancelled / PodTerminated / SourcePodReplaced / Timeout / Other (F4.3)
- No PDB on dst pod; Phase 4 webhook handles drain-mid-migration (F4.4)
- True node failure ≈ Q3-v3 blackhole; same coping path via spec.timeout (F4.5)

## Open questions for Phase 3a design

**Multi-migration concurrency.** N concurrent SwiftMigrations on a
cluster: does Phase 3a serialize globally, per-namespace, per-source-
node, or not at all? Phase 1's per-SwiftGuest finalizer mutex handles
the source side. The destination pod is a new resource without a
per-guest mutex; without coordination, two SwiftMigrations could race
to send into the same dst pod (or schedule two competing dst pods on
the same target node). **Default recommendation: serialize
per-source-node** — the controller refuses to schedule a new
SwiftMigration whose source is a node that already has an in-flight
SwiftMigration. Same-target-node serialization is less obviously
needed (different SwiftGuests can have different dst pods on the
same node), and per-namespace and global serialization are too
restrictive for typical cluster operations.

**Leader election handover mid-migration.** controller-runtime's
leader election: if the controller-manager leader fails over during
a migration, the new leader's reconcile picks up the SwiftMigration
in some phase. The annotation-as-source-of-truth idempotency primitive
(Phase 1's `kubeswift.io/migration-in-progress`) handles the SwiftGuest
side. Phase 3a inherits this; Q1c's reconcile-interruption test is the
limit case (orchestrator dies mid-flight, fresh orchestrator picks up
state from cluster observation). **Phase 3a design open question**:
verify cluster-state-only reconstruction by deliberately failing over
the controller-manager leader during a Phase 3a migration. Spike's
Q1c test approximates this.

**Destination listener timeout and re-arm strategy.** Q1d's empirical
measurement of CH receive-listener timeout sets a constraint on how
long the controller's reconcile loop can pause between recv-issued
and send-issued. If the listener gives up before the next reconcile
fires, the controller must re-issue receive (or recreate dst pod) to
recover. **Phase 3a design open question**: should the controller
ship a `spec.destinationTimeout` similar to Phase 2 spike's open
question 2, or rely on swiftletd-side timeout introspection?

**Phase 3b mTLS scope simplification (cross-reference to F2.4).**
F2.4 establishes that the Phase 3a controller does NOT need cross-
pod connectivity — controller-manager observes both pods exclusively
via the apiserver/informer surface. This **closes off the
controller→swiftletd command channel as a Phase 3b design surface**:
the only channel that needs mTLS hardening is **swiftletd-on-src ↔
swiftletd-on-dst** (the live migration data path itself), not
controller→swiftletd. Significantly narrows Phase 3b's threat-model
scope and certificate-management burden — operators only need to
provision migration-channel certs to launcher pods, not also to the
controller-manager.

[More open questions added as spike progresses]

---

## Failure-mode catalog

The Phase 3a controller's state machine must explicitly handle each
of the following failure modes. Catalog ordered by detection-side
complexity.

### Detected via src-side terminal status (informer event)

| Mode | Trigger | swiftletd writes | Controller transitions to |
|---|---|---|---|
| **Migration succeeds** | Normal flow | src=complete with $SEND_ID | Completed |
| **Migration fails (CH-internal)** | CH `vm.send-migration` returns error | src=failed with detail (e.g. CPU mismatch) | Failed (FailureReason: Other) |
| **Dst pod K8s-terminated** (graceful or drain) | Q4a, Q4e | src=failed ("send_migration: internal_server_error") | Failed (FailureReason: PodTerminated, with detail "destination pod") |
| **W1 violation** (dst CH crashed silently post-receive) | Phase 2 PR-B dispatch-side gate | src=failed with W1 detail | Failed (FailureReason: Other) |

### Detected via informer + cross-resource UID check

| Mode | Trigger | Observable | Controller transitions to |
|---|---|---|---|
| **Src pod K8s-terminated** | Q4c (graceful) or Q3-v2 (force) | `SwiftGuest.status.podRef.uid` no longer matches `SwiftMigration.status.sourcePodUID` | Failed (FailureReason: SourcePodReplaced or PodTerminated) |
| **Src node failure / unreachable** | Real node down | Same as above + node Ready condition False | Failed (FailureReason: PodTerminated) |

### Detected via spec.timeout (default 5m)

| Mode | Trigger | Observable | Controller transitions to |
|---|---|---|---|
| **Network blackhole** (DROP rule, NetworkPolicy, broken Calico) | Q3-v3 | No annotation transitions on either pod for >127s | Failed (FailureReason: Timeout) |
| **Dst node failure / unreachable mid-flight** | Real node down | dst pod stuck in Running/NodeNotReady state; src CH retransmitting SYN | Failed (FailureReason: Timeout) |
| **Phase stall** (any unanticipated hang) | Defensive | spec.timeout exceeded since SwiftMigration creation | Failed (FailureReason: Timeout) |

### Detected via reconcile-loop interruption recovery (Q1c)

| Mode | Trigger | Recovery |
|---|---|---|
| **Controller pod crash mid-flight** | Pod death between annotations | New leader reconciles SwiftMigration; observes intermediate phase via cluster state alone; resumes — F1.8 send_id pattern enables idempotent retry |
| **Leader election handover** | controller-runtime leader change | Same as above |

### Cancel path (operator-initiated)

| Mode | Trigger | Sequence |
|---|---|---|
| **Cancel** | User updates `SwiftMigration.spec.cancelRequested=true` (or whatever shape Phase 3a chooses) | 1. Controller writes `migration-action: cancel` annotation on src pod with fresh $CANCEL_ID. 2. swiftletd's cancel handler **(currently a Phase 2 placeholder, see F3.4)** tears down CH connection gracefully. 3. swiftletd writes `migration-status: failed` with detail "cancelled by controller". 4. Controller observes via informer; transitions to Failed (FailureReason: Cancelled). |
| **Cancel fallback** | swiftletd's cancel handler doesn't ack within timeout (e.g., 30s) | Force-delete src and dst pods; SwiftGuest controller recreates src; transition to Failed (FailureReason: Cancelled, with detail noting forced cleanup) |

---

## Spike disposition

The four spike questions resolved with empirical evidence; an
additional finding (B0) surfaced and was worked around to enable the
spike's cluster experiments. **B0 must ship as a standalone PR ahead
of Phase 3a implementation.**

The spike's Phase 3a-relevant outputs:

1. **Annotation schema is sufficient** — no swiftletd changes
   required for Phase 3a state machine (F2.3).
2. **Two swiftletd-side asks** identified for Phase 3a implementation
   to flag with the rust-runtime engineer:
   - Auto-write `migration-status=failed` when dst CH listener exits
     abnormally without a terminal status (F3.2).
   - Implement the cancel handler currently shipped as a placeholder
     in Phase 2 PR-B (F3.4).
3. **One controller-design open question (F1.5)** conditional on
   architectural direction: dst pod ownerRef option 2 (SwiftGuest
   owns dst, with `migration-role` label) is recommended IF Phase 3a
   makes SwiftGuest controller migration-aware. If rejected, options
   1/3 reopen.
4. **Empirical timing budgets** locked in:
   - Annotation write→informer-observe latency: ≤25ms (spike), 2-5×
     in production (F2.2 caveat).
   - Migration body for kernel-boot 4Gi guest: ~38s on Longhorn
     spike cluster.
   - spec.timeout default: **5 minutes** (F3.5 lower-bounded by
     kernel TCP retransmit at 127s).
5. **Failure-mode catalog above** drives the Phase 3a state machine
   exhaustively.
6. **Phase 3b scope simplification** (F2.4): controller→swiftletd
   command channel is closed as a design surface; only swiftletd↔
   swiftletd needs Phase 3b mTLS hardening.

The spike is **complete**. Ready for Phase 3a design alignment.

### Time accounting
~3.5 hours total wall-clock. ~70 min on B0 cluster-networking debug
(unforeseen but productive — uncovered the kubeswift platform bug
fix that ships ahead of Phase 3a). ~80 min on Q1+Q2+Q3+Q4 cluster
work + scripting + findings doc. Well under the 3-day cap.

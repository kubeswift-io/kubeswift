# Live Migration Phase 3c — mTLS Migration Transport

> Phase 3c hardens the migration **data channel** that Phase 3a/3b ship.
> Live mode already moves a running guest's CPU + memory state cross-node
> over the default pod network in **plaintext**, gated only by the
> operator-acknowledged `kubeswift.io/migration-phase2-unsafe-plaintext:
> ack` annotation. Phase 3c puts that channel inside a
> mutually-authenticated TLS tunnel and retires the plaintext gate.
>
> **Status: DRAFT (design-locked on the central decision).** Spike-settled
> sections (1, 2, 4, 5, 6) are firm. Section 3 (cert identity model) is
> **DECIDED: Option B — per-node identity + SAN pinning** (operator steer,
> 2026-05-30). The remaining open items are implementation-level
> (§3.B provisioning mechanism, §10).
>
> Spike findings (the empirical basis for this doc):
> [`live-migration-phase-3c-mtls-spike.md`](live-migration-phase-3c-mtls-spike.md).
> Spike planning + trust-model framing:
> [`live-migration-phase-3c-spike-prep.md`](live-migration-phase-3c-spike-prep.md).
> Phase 3a controller design (offline + state machine):
> [`live-migration-phase-3a.md`](live-migration-phase-3a.md).
> Phase 3b live-mode design:
> [`live-migration-phase-3b.md`](live-migration-phase-3b.md).
>
> Last updated: 2026-05-30.

---

## 0. How to read this document

Phase 3c does **not** change Cloud Hypervisor and does **not** change
swiftletd's migration data path. The spike proved a third-party
`stunnel` sidecar can carry CH's `vm.send-migration` /
`vm.receive-migration` traffic over mTLS while CH and swiftletd keep
speaking **plaintext to `127.0.0.1` only**. Phase 3c is therefore a
**controller + pod-shape + cert-provisioning** change, plus the
companion security workstreams (S1 URLs-from-CR, ack-gate retirement,
audit events) that the spike confirmed must compose with the transport.

Read the spike findings doc first. This doc is the implementation
contract; the spike doc is the evidence.

### Notation

| Code | Meaning | Source |
|---|---|---|
| `W-3c-1..4` | Phase 3c spike wiring finding | spike findings doc §Q4 |
| `Q1..Q4` | Phase 3c spike question (correctness / perf / enforcement / wiring) | spike findings doc |
| `S1` | Security finding: migration URLs must come from the SwiftMigration CR, not pod annotations | Phase 2 spike + `THREAT-MODEL.md` |
| `W26` | Walkthrough lesson: name a load-bearing property before a "cleaner" refactor regresses it | `kubeswift_context.md` |

---

## 1. Goal and Non-goals

### Goal

Every byte of guest CPU/memory state that crosses the pod network during
a `mode: live` SwiftMigration travels inside a mutually-authenticated TLS
channel. A peer that does not present a cert chaining to the migration CA
**and** matching the expected identity is rejected at the TLS handshake —
**zero plaintext guest bytes ever reach the wire and zero bytes reach the
peer's CH** (Q3 negative-test bar). The `migration-phase2-unsafe-plaintext:
ack` escape hatch is retired on the production path.

Phase 3c ships for the **same two workload classes** as Phase 3a/3b:

- Kernel-boot guests (`spec.kernelRef`).
- Disk-boot guests (`spec.imageRef`) on RWX+Block `longhorn-migratable`
  storage.

### Non-goals

- **No CH change. No swiftletd migration-data-path change.** CH dials/
  listens on `127.0.0.1`; the sidecar owns the cross-pod hop. The
  swiftletd `listen_url`/`target_url` opacity contract (Phase 2) is
  preserved — swiftletd is handed a localhost URL and does not know TLS
  exists.
- **No dedicated migration network.** Spike Q2 showed the TLS tunnel runs
  at ~99% of the plaintext pod-network throughput; the default pod
  network is sufficient (Phase 3b Q4 conclusion stands).
- **No VFIO/SR-IOV cross-node migration.** Still rejected (upstream CH
  constraint #2251); unchanged by Phase 3c.
- **Not a re-litigation of live-mode mechanics.** Pre-copy iterations,
  cutover, the state machine, downtime characteristics — all Phase 3a/3b,
  unchanged. Phase 3c wraps the channel; it does not touch the dance.

---

## 2. Settled by the spike

These are firm; the design builds on them without re-deciding.

### 2.1 Architecture — stunnel sidecar, CH on localhost

```
        SOURCE POD (node = src)                   DESTINATION POD (node = dst)
 ┌───────────────────────────────┐         ┌───────────────────────────────┐
 │ swiftletd + CH                 │         │ swiftletd + CH (receiver mode) │
 │  vm.send-migration             │         │  vm.receive-migration          │
 │  target_url=tcp:127.0.0.1:6790 │         │  listen_url=tcp:127.0.0.1:6790 │
 │            │ plaintext         │         │            ▲ plaintext         │
 │            ▼ (localhost only)  │         │            │ (localhost only)  │
 │ ┌───────────────────────────┐ │  mTLS   │ ┌───────────────────────────┐ │
 │ │ stunnel  CLIENT           │ │ :6789   │ │ stunnel  SERVER           │ │
 │ │ accept 127.0.0.1:6790     │ │═════════▶│ │ accept 0.0.0.0:6789       │ │
 │ │ connect <dst-ip>:6789     │ │ pod net │ │ connect 127.0.0.1:6790    │ │
 │ │ verify peer cert + SAN    │ │         │ │ verify peer cert + SAN    │ │
 │ └───────────────────────────┘ │         │ └───────────────────────────┘ │
 └───────────────────────────────┘         └───────────────────────────────┘
```

### 2.2 Port plan

| Leg | Endpoint | Scope |
|---|---|---|
| Cross-pod TLS | dst `0.0.0.0:6789` (stunnel server) | pod network |
| CH↔stunnel, dst | `127.0.0.1:6790` (CH listens; stunnel connects) | localhost only |
| CH↔stunnel, src | `127.0.0.1:6790` (stunnel accepts; CH connects) | localhost only |

Controller change at the two URL-build sites in
[`stopandcopy_live.go`](../../internal/controller/swiftmigration/stopandcopy_live.go)
(~lines 335/380): repoint `listen_url` / `target_url` from today's
cross-pod `tcp:0.0.0.0:6789` / `tcp:<dst-ip>:6789` to the **localhost**
`tcp:127.0.0.1:6790`, and hand `:6789` to the sidecar. `migrationListenPort`
(currently `6789`, line 35) splits into a TLS port (`6789`) and a
localhost plaintext port (`6790`).

### 2.3 Performance (Q2 — PASS, ~1% overhead)

Through the TLS tunnel: 4326154986 bytes / 38.675s = **111.86 MB/s**, vs
~112.75 MB/s raw (Phase 3b Q4). TLS framing/encryption (AES-NI) costs
~1%. The operator sizing formula is unchanged:
`pauseWindow ≈ (guest_RAM × 1.05) / pod_network_bandwidth`.

### 2.4 Enforcement (Q3 — PASS, positive + two negatives)

- Positive: mutual verify succeeds; full transfer; sentinel md5
  `e187f76732140367822efbd7ac675019` identical src→dst.
- Negative Test A (client presents **no** cert): rejected at handshake;
  **0 bytes** reach CH.
- Negative Test B (client presents a **wrong-CA** cert, `CN=attacker`):
  rejected at handshake; **0 bytes** reach CH.

The plaintext `:6790` leg is localhost-only and not reachable cross-pod,
so there is no plaintext bypass path.

### 2.5 Sidecar model — one image, one ConfigMap, role-by-env

The spike used a single image (`dweomer/stunnel:latest`) and a single
ConfigMap carrying **both** server and client configs; the entrypoint
self-selects server-vs-client from `STUNNEL_ROLE` and injects the peer IP
from `DST_POD_IP` (sed over a `__DST_POD_IP__` placeholder). Phase 3c
keeps this shape; the controller parameterizes role + peer by **env**,
never by image (W-3c-2 load-bearing property).

---

## 3. Trust model & cert identity — DECIDED: Option B (per-node + SAN pinning)

> **Decision (operator steer, 2026-05-30): Option B — per-node /
> per-swiftletd identity with SAN pinning.** Strong authz (the channel is
> bound to the specific src↔dst node pair the controller chose from the
> CR) with zero per-migration issuance latency. Options A and C below are
> retained as the considered-and-rejected analysis. The remaining sub-
> decision is the per-node cert **provisioning mechanism** (§3.B), settled
> at implementation.

The spike deliberately scoped this out (spike-prep §4) and used the
weakest model — a **single shared long-lived leaf**
(`CN=kubeswift-migration`) on both pods with stunnel `verify = 2`.
`verify = 2` is `verifyChain` **without subject checks** (W-3c-4): it
proves "this peer holds a cert that chains to our CA"; it does **not**
prove "this peer is the legitimate src/dst for THIS migration." Under a
shared leaf, any pod that can mount the migration Secret is accepted.

**`verify = 2` alone is not shippable** — the chosen model adds subject/
SAN pinning (`verifyChain = yes` + `checkHost = <expected-peer-SAN>`). The
axis the decision settled is **how strongly the cert identity binds to the
migration, vs. how much provisioning machinery it costs.**

> **Directive note (PR 2 correction):** earlier drafts wrote this as
> `verify = 4`; that was imprecise shorthand for "chain-verify *and*
> subject-pin." The implemented sidecar config (PR 2) uses
> `verifyChain = yes` (numerically `verify = 2`) **plus** `checkHost`.
> stunnel's literal `verify = 4` is the wrong primitive for Option B:
> it ignores the CA chain and pins the *exact* leaf, which would require
> each sidecar to mount its peer's exact cert and would break on
> cert-manager rotation (the per-node certs renew via `renewBefore`).
> `verifyChain` proves "chains to the migration CA"; `checkHost` closes
> the W-3c-4 subject-check gap — together they authorize a *specific*
> peer while staying rotation-safe.

### Option A — Shared long-lived leaf (what the spike used)

One cert-manager `Certificate` → one `Secret`, mounted by every migration
sidecar. Both peers present the same leaf.

- **Authz strength:** weakest. "Can mount the migration Secret." Pinning
  `checkHost` to the shared CN proves only "a kubeswift-migration peer."
- **Hot-path cost:** zero issuance; the Secret is long-lived and
  pre-provisioned.
- **Provisioning machinery:** minimal (one Certificate, one Secret).
  Relies on tight RBAC/namespace scoping of the Secret + S1 for its real
  authz.
- **Verdict:** acceptable only as a fallback if B/C are judged too heavy,
  and only with S1 shipped and the Secret RBAC-scoped to swiftletd pods.

### Option B — Per-node / per-swiftletd identity + SAN pinning (CHOSEN)

Each swiftletd sidecar presents a cert whose SAN encodes its **node** (or
pod) identity. The controller already knows the **src node** and **dst
node** from the SwiftMigration CR + SwiftGuest, so it stamps the expected
peer identity onto each sidecar from the CR-known pair:

- dst sidecar: `checkHost = <src-node-SAN>` (accept only the source node);
- src sidecar: `checkHost = <dst-node-SAN>` (connect only to the target
  node).

- **Authz strength:** strong. Binds the channel to the specific src↔dst
  node pair the controller chose — and the controller chooses it from the
  CR (S1), not from attacker-writable annotations. Residual gap vs C:
  none that is reachable (concurrent migrations between the same node pair
  use distinct dst IPs/ports + distinct CH listeners the controller
  orchestrates).
- **Hot-path cost:** zero per-migration issuance (node certs are
  long-lived).
- **Provisioning machinery:** needs a per-node/per-swiftletd cert — the
  one remaining sub-decision (§3.B).
- **Verdict:** best authz-per-machinery. Gets nearly all of C's binding
  without per-migration issuance in any path. **Chosen.**

#### §3.B — Per-node cert provisioning (sub-decision, settle at implementation)

Two candidates for issuing each swiftletd a SAN=node (or SAN=pod) cert:

- **(a) Per-node cert-manager `Certificate`** (SAN = node name), one per
  worker/GPU node, long-lived. The controller references the dst node's
  Secret when building the dst pod and the src node's Secret for the src
  sidecar. Minimal new dependency surface (cert-manager already on the
  cluster); the wrinkle is mounting the right per-node Secret into a
  controller-created pod (the pod lands on a known node — the controller
  sets `nodeName` — so it can select the matching Secret at build time).
- **(b) cert-manager csi-driver** issuing a short-lived SAN=pod/node cert
  at pod start (SPIFFE-ish). Cleaner identity story (no per-node Secret
  bookkeeping; certs are ephemeral) at the cost of a new cluster
  dependency (the csi-driver) and short-cert-lifetime handling (§7).

Lean: **(a)** for first ship — no new cluster component beyond
cert-manager, and the controller already pins `nodeName` so Secret
selection is deterministic. Revisit **(b)** if per-node Secret management
becomes a burden or a stronger ephemeral-identity story is wanted.

### Option C — Per-migration identity

cert-manager `Certificate` per migration, SAN = migration UID; controller
pins `checkHost = <migration-UID>` on both sides.

- **Authz strength:** strongest. The channel is cryptographically bound to
  THIS migration.
- **Hot-path cost:** adds cert issuance to **every** migration and a hard
  cert-manager dependency in the migration path. **Key mitigation the
  spike surfaced:** issuance can sit in the **~38s pre-copy window**
  (issue during Validating/Preparing-live), NOT the **sub-3s cutover
  window** — so issuance latency need not touch downtime *if* the design
  issues early. The spike did **not** measure issuance latency; if C is
  chosen, the design must.
- **Provisioning machinery:** highest (a Certificate per migration, GC of
  issued certs, cert-manager on the migration critical path).
- **Verdict:** strongest but heaviest; justified only if per-migration
  binding is a hard requirement (e.g., a multi-tenant threat model where
  per-node trust is insufficient).

### Decision rationale

**Option B chosen.** It binds the channel to the specific src↔dst node
pair the controller selects from the CR (composing with S1, §6.1) without
adding cert issuance to any migration's critical path — the headline
sub-3s cutover is untouched. Option A's authz ("can mount the Secret") is
too weak to be the production default; Option C's per-migration issuance
buys binding strength KubeSwift's current single-tenant threat model does
not require, at the cost of a cert-manager dependency in the migration
path. Option A remains an **explicit fallback** for clusters unwilling to
provision per-node certs (shipping *only* with S1 + a tightly-scoped
Secret); Option C is held for a future multi-tenancy phase.

The choice sets, downstream: the `verifyChain = yes` + `checkHost =
<peer-node SAN>` config the controller stamps onto each sidecar (§4.2;
see the directive note above on why not literal `verify = 4`); **no** cert
issuance in the state machine — the per-node cert is a precondition
(§4.4); the cert-manager dependency stays at "already required" (§6.3);
and the failure modes reduce to precondition + handshake + expiry (§7).

---

## 4. Controller wiring

All changes live in the SwiftMigration controller; CH and swiftletd are
untouched (§1 non-goals). The wiring composes with the existing live-mode
state machine (Phase 3a/3b) — it adds pod-shape and URL details at points
the state machine already passes through.

### 4.1 Destination pod construction (`newDstPod`)

[`dst_pod.go::newDstPod`](../../internal/controller/swiftmigration/dst_pod.go)
builds the dst by `srcPod.DeepCopy()` (the W26/Phase-3b load-bearing
clone-src property — **do not** refactor to re-resolve from SwiftGuest
spec; that would regress version-skew prevention *and* now the sidecar
inheritance below). Post-DeepCopy, Phase 3c adds:

1. **Inject the stunnel sidecar container** (image + cert volumeMount of
   the migration Secret at `/etc/migration-tls/` + the sidecar ConfigMap
   volume). If the **src** pod already carries the sidecar (it must, to be
   the TLS client), the DeepCopy brings it along — the controller then
   **flips its role** rather than appending a second one.
2. **Flip role to server on dst:** set `STUNNEL_ROLE=server` on the dst
   sidecar env (src stays `client`) — W-3c-2.
3. **Freeze `lifecycle: run` on the dst intent** — W-3c-1. The dst must
   **not** mount the live, controller-mutable `<guest>-runtime-intent`
   ConfigMap, because patching the source guest to `runPolicy: Stopped`
   during cutover makes the SwiftGuest controller rewrite that CM to
   `lifecycle: stop`, which swiftletd's launch gate (`main.rs:201`) honors
   for **all** launch paths including `migration_receiver_mode` (receiver
   role is an env var, not an exemption). The controller mints/points the
   dst at a frozen `lifecycle: run` intent.
4. **SAN-pin per §3:** stamp `checkHost`/`verify` onto the dst sidecar
   from the §3 decision (dst expects the src-node SAN).

### 4.2 Source sidecar configuration + dst-IP sequencing

The **src** sidecar is the TLS client; it must `connect <dst-ip>:6789`.
The dst IP is known **only after** the dst pod is scheduled and gets an
IP (W-3c-2 sequencing constraint). The live-mode state machine already
satisfies this ordering — **Preparing-live creates the dst before
StopAndCopy patches the src** — so the dst IP is observable before the
src sidecar needs it. The controller stamps `DST_POD_IP=<dst-ip>` (and
the src-side `checkHost` per §3) onto the src sidecar env at that point.

Role/peer/SAN are **env-parameterized, mutated after DeepCopy, never
image-baked** (W-3c-2). Name this property in the `newDstPod` docstring so
a future "bake the role into a dedicated image — cleaner" refactor sees
the constraint first (W26 discipline).

### 4.3 URL repoint (localhost)

At the two URL-build sites (§2.2), `listen_url`/`target_url` become
`tcp:127.0.0.1:6790`. swiftletd receives a localhost URL and is unaware of
TLS (opacity contract preserved).

### 4.4 State machine touchpoints

No new phases, and (Option B) **no cert issuance in the machine** — the
per-node cert is a long-lived precondition, not a per-migration artifact.
The only addition is a **Validating-live precondition check**: the src
node's and dst node's per-node migration identity Secrets are present and
Ready, and (Option-B-(a)) selectable by `nodeName`. Fail fast with a
`FailureReason` (mirroring Phase 3a's enum) before entering Preparing-
live, so a missing/expired node cert never reaches the cutover window.

> Rejected alternative (Option C, for the record): per-migration issuance
> would create the `Certificate` at Validating/Preparing-live and gate
> StopAndCopy on the issued Secret mounting on both sidecars — issuance in
> the pre-copy window, never cutover. Not chosen; documented so a future
> multi-tenancy phase knows where issuance would slot in.

---

## 5. swiftletd / CH — unchanged (opacity contract)

Restated because it is the spike's headline and a load-bearing
non-change: swiftletd is handed `tcp:127.0.0.1:6790` and threads it
opaquely to `swift-ch-client`'s `send_migration` / `receive_migration`
(`rust/swiftletd/src/action.rs`). CH dials/listens on localhost. **No Rust
change, no CH change, no new swiftletd env beyond the existing
`KUBESWIFT_MIGRATION_ROLE=receiver`.** Phase 3c must not add TLS awareness
below the controller; doing so would couple the runtime to the transport
and break the "third-party sidecar owns the hop" property that makes this
shippable without an upstream CH change.

---

## 6. Composition with the security workstreams

The spike confirmed (W-3c-4 + carry-forwards) that mTLS is **necessary
but not sufficient**; three companion items ship with Phase 3c.

### 6.1 S1 — migration URLs from the SwiftMigration CR, not pod annotations

mTLS closes "redirect to an arbitrary attacker endpoint" (attacker has no
CA-signed cert — Q3 Test B). It does **not** close "redirect to a
different *valid* migration pod" (valid cert under a shared leaf) nor
"operator-writable annotation inputs." **S1 and mTLS are both required;
neither subsumes the other.** Phase 3c reads the data-path URLs from the
SwiftMigration CR / controller-derived state, not from operator-writable
pod annotations. (This is also what makes Option B's SAN pinning
trustworthy — the controller derives the node pair from the CR, not from
annotations an attacker could rewrite.)

### 6.2 Retire the `migration-phase2-unsafe-plaintext: ack` gate

Once mTLS is the production path, the ack annotation becomes a one-way
switch: the controller stops emitting it, swiftletd stops requiring it on
the secured path, and the key is slated for deletion (the THREAT-MODEL
Phase 3 must-have). Sequencing matters — swiftletd must not reject the new
secured flow for *lacking* the ack; coordinate the controller-side stop
with the swiftletd-side requirement removal so no version skew leaves a
flow un-runnable.

### 6.3 Audit events

Kubernetes Events on each migration phase transition (THREAT-MODEL Phase 3
must-have #3), including the mTLS handshake outcome and (Options B/C) the
pinned peer identity, so an operator can see *which* identity the channel
authenticated.

### 6.4 `spec.allowVersionSkew` stays retired

Phase 3b spike retired it (the `newDstPod` clone-src structurally prevents
skew). Phase 3c does not reintroduce it. Dropped from the must-have list.

---

## 7. Failure modes

| Mode | Detection | Disposition |
|---|---|---|
| Per-node identity Secret missing / not Ready / not selectable by `nodeName` | Validating-live precondition check (§4.4) | Fail fast, `FailureReason` set; never enter Preparing-live |
| TLS handshake rejected (wrong/no cert, SAN mismatch) | stunnel logs + 0 bytes to CH; swiftletd sees no receive/connect progress → existing `spec.timeout` floor | Migration fails on timeout; **source keeps running** (pre-copy does not pause source); no data loss |
| Sidecar crash mid-migration | pod Ready flips; existing live-mode dst-disappearance / src-RPC-wedge handling (TFU-14) | Same coping path as Phase 3b: controller drives to Failed/Cancelled; source unharmed |
| Cert expiry mid-migration (short-lived certs under Option-B-(b) csi-driver) | handshake fails on renegotiation/reconnect | Bound cert lifetime ≫ `spec.timeout`; under Option-B-(a) long-lived per-node certs this is a non-issue; document the floor |

The unifying property (Phase 3b inheritance): live-migration pre-copy does
**not** pause the source, so **every** transport failure leaves the source
guest running and recoverable. mTLS adds handshake-rejection as a new
*failure cause* but not a new *data-loss surface*.

---

## 8. Load-bearing architectural properties (do not regress)

1. **`newDstPod` clone-src** (W26 + 3b): prevents version skew AND carries
   the src sidecar onto the dst for role-flipping. A refactor to
   re-resolve dst from SwiftGuest spec regresses both.
2. **Sidecar role/peer is env-parameterized, not image-baked** (W-3c-2):
   one image, one ConfigMap, `STUNNEL_ROLE` + `DST_POD_IP`. Baking role
   into a dedicated image re-introduces build-time coupling.
3. **dst created before src sidecar configured** (W-3c-2): the existing
   Preparing-live → StopAndCopy ordering is the sequencing guarantee for
   stamping the dst IP onto the src sidecar. Do not reorder.
4. **`lifecycle: run` frozen on the dst intent** (W-3c-1): never mount the
   live controller-mutable intent CM on the dst; a `runPolicy: Stopped`
   patch during cutover would poison it to `lifecycle: stop`.
5. **CH/swiftletd speak localhost plaintext only** (§5): the cross-pod hop
   is the sidecar's, exclusively. No TLS below the controller.
6. **`verifyChain = yes` + SAN pinning (`checkHost`), never bare
   `verify = 2`** (W-3c-4): the shippable config authorizes a *specific*
   peer, not merely a CA-signed one. (Earlier drafts said `verify = 4`;
   see the §3 directive note — literal `verify = 4` pins the exact leaf
   and breaks cert rotation, so the implementation uses
   `verifyChain` + `checkHost`.)

---

## 9. Out of scope

- VFIO/SR-IOV cross-node migration (CH #2251).
- A dedicated migration network (Q2: not needed).
- Per-iteration progress streaming (Phase 3b Q1: annotation surface is the
  wrong tool; separate channel if ever needed).
- Multi-tenant per-migration isolation beyond the §3 choice (Option C is
  the hook; full multi-tenancy is a later phase).
- Replacing stunnel with first-party CH TLS support (if upstream CH grows
  it, Phase 3c's sidecar becomes removable — tracked, not built here).

---

## 10. Open questions (for the design conversation + walkthroughs)

1. **§3 cert identity model** — **RESOLVED 2026-05-30: Option B**
   (per-node identity + SAN pinning).
2. **§3.B per-node cert provisioning** — **RESOLVED 2026-05-31: (a)**
   per-node cert-manager `Certificate` + `nodeName`-keyed Secret
   selection (not the csi-driver (b)). Implemented in PR 1
   (`internal/controller/migrationcert`): a Node-watch reconciler issues
   one long-lived `Certificate` per worker node, SAN=nodeName, on node
   join — satisfying the §3.B(a) / §4.4 precondition that no cert-manager
   call sits on any migration path (the leaf is already issued; the
   migration path only consumes the Secret). cert-manager types are
   handled as `unstructured.Unstructured` to avoid a go.mod dependency on
   an optional, operator-installed operator.
3. **Sidecar image** — **RESOLVED 2026-05-31: build a tiny first-party
   image** (alpine + stunnel, ASCII-only entrypoint per the
   container-script rule), avoiding third-party supply-chain review.
   Built in PR 2.
4. **Cert lifetime vs `spec.timeout`** — ensure cert validity ≫ the
   longest permitted migration; document the relationship.
5. **Ack-gate retirement sequencing** (§6.2) — controller-stop vs
   swiftletd-require-removal ordering across a rolling upgrade.
6. **Cluster-level enable** — **RESOLVED 2026-05-31: Helm-gated opt-in
   during rollout.** mTLS is gated by `migration.mtls.enabled` (off by
   default) while Phase 3c lands across PRs 1-5, so clusters without
   cert-manager are unaffected and the rollout is incremental. This is the
   rollout posture, NOT a per-SwiftMigration opt-out: plaintext retirement
   on the secured path (the `migration-phase2-unsafe-plaintext: ack` gate
   becoming a one-way switch) still lands in PR 4 as designed. PR 1 ships
   the `migration.mtls.enabled` Helm gate, the static
   `--migration-mtls-enabled` flag, and the `deploy-with-mtls` kustomize
   overlay (TFU-16 pattern: opt-in surfaces get an explicit Make target +
   overlay).

---

## 11. References

- Spike findings: [`live-migration-phase-3c-mtls-spike.md`](live-migration-phase-3c-mtls-spike.md)
- Spike prep + trust framing: [`live-migration-phase-3c-spike-prep.md`](live-migration-phase-3c-spike-prep.md)
- Phase 3a design: [`live-migration-phase-3a.md`](live-migration-phase-3a.md)
- Phase 3b design: [`live-migration-phase-3b.md`](live-migration-phase-3b.md)
- Threat model: [`THREAT-MODEL.md`](THREAT-MODEL.md)
- Controller dst-pod builder: [`dst_pod.go`](../../internal/controller/swiftmigration/dst_pod.go)
- Live-mode URL wiring: [`stopandcopy_live.go`](../../internal/controller/swiftmigration/stopandcopy_live.go)

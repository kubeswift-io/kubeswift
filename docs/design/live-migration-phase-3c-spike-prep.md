# Live Migration Phase 3c — mTLS Transport Spike (Prep)

> This is the **spike-prep** doc, not findings. It captures the framing,
> candidate architecture, trust-model context, the spike questions, and
> Layer A's done state so a fresh session can pick up Layer B without
> re-deriving any of it.
>
> Spike branch: `spike/phase-3c-mtls` — **NOT for merge** (project spike
> contract; findings doc lands separately).
> Last updated: 2026-05-29.
>
> **Status (2026-05-30): the spike is COMPLETE.** All four questions in §4
> were answered empirically (all PASS). See the findings in
> [`live-migration-phase-3c-mtls-spike.md`](live-migration-phase-3c-mtls-spike.md).
> The "to resume" / "Resume checklist" language below is the original
> planning text, preserved as the spike's design-intent record; the
> Layer B–D walkthrough and the reproduction harness live on the unmerged
> `spike/phase-3c-mtls` branch.

---

## 1. Why this spike, and why first

Phase 3a/3b shipped live migration over **plaintext TCP**. The source
swiftletd calls Cloud Hypervisor's `vm.send-migration` with
`destination_url=tcp:<dst-pod-ip>:6789`; the destination listens via
`vm.receive-migration` on `tcp:0.0.0.0:6789`. Full guest memory crosses
the pod network in cleartext, gated only by a controller-auto-set
`kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation —
operators get the plaintext path without a real per-migration ack.

`docs/design/THREAT-MODEL.md` "Phase 3 must-have-before-production-
traffic" lists this as non-negotiable for production: **mTLS on the
migration channel** (sidecar with stunnel/socat **or** upstream CH
support); plus the related S1 work (URLs from SwiftMigration CR, not
operator-writable annotations) and audit events. (#4
`spec.allowVersionSkew` is **obsolete** — retired in the Phase 3b spike
because `newDstPod` clone-src structurally prevents skew.)

Phase 3c kicked off with the decision to **spike the mTLS transport
first** — validate the highest-risk unknown on the real cluster before
authoring the design doc. The candidate approach is a TLS-tunneling
sidecar (stunnel) per launcher pod, with **no Cloud Hypervisor
changes**: CH keeps speaking plaintext TCP to localhost; the sidecar
owns the cross-pod TLS.

**Spike scope intentionally narrowed:** the *transport* mechanics +
performance + enforcement. **The cert identity model** (per-migration
cert with SAN=migration UID vs. a longer-lived per-node/swiftletd
identity) is *the* key trust-model decision — but the spike validates
the transport with a **simple shared cluster-CA + single leaf identity**
to de-risk the mechanics; per-migration identity is then a design-doc
decision informed by the spike's cert-lifecycle observations.

---

## 2. Recon facts (verified on `main`)

| What | Where | Value |
|---|---|---|
| CH send body | `rust/swift-ch-client/src/methods.rs:164` | `{"destination_url":"tcp:<host>:<port>"}` — explicitly plaintext (line 128) |
| CH receive body | `rust/swift-ch-client/src/methods.rs:219` | `{"receiver_url":"tcp:0.0.0.0:<port>"}` |
| Migration port | `internal/controller/swiftmigration/stopandcopy_live.go:35` | `migrationListenPort = 6789` |
| Controller wires | `stopandcopy_live.go:335,380` | `ListenURL=tcp:0.0.0.0:6789`, `TargetURL=tcp:<dst-ip>:6789` |
| Dst pod build | `internal/controller/swiftmigration/dst_pod.go::newDstPod` | DeepCopies src pod spec → a sidecar added to launcher pod is **inherited by dst** |
| Launcher pod shape | `internal/controller/swiftguest/pod.go:227,361` | Single launcher container + init-containers (network-init, clone-grow-init) → a sidecar = one appended container |
| cert-manager | `kubectl get pods -n cert-manager` | Running with `certificates.cert-manager.io` etc. CRDs |
| Manual-demo scaffolding | `test/migration/manual/{source,destination,run,verify}.sh`, `tools/manual-demo/phase-3b-pr1/{launch-pods,trigger-migration,cleanup}.sh`, `Makefile:215 migration-phase2-manual` | The lever for driving CH through localhost-redirected URLs without modifying the shipped controller/swiftletd |

**The annotation/CR boundary the spike must be aware of:** swiftletd
currently reads `target_url`/`listen_url`/`guest_ip` from the
`migration-action-args` pod annotation (the three `SECURITY-S1` sites).
Manual-demo path can set these annotations directly — that's how the
spike will inject localhost URLs without touching swiftletd source.

---

## 3. Candidate architecture (under test)

**stunnel sidecar per launcher pod, no CH change:**

- **Destination pod:** stunnel runs as TLS **server** on `0.0.0.0:6789`,
  required-and-verifies client cert against the CA, forwards decrypted
  plaintext to `127.0.0.1:<local>` where CH's `vm.receive-migration`
  listens. swiftletd points CH at `tcp:127.0.0.1:<local>` instead of
  `0.0.0.0:6789`.
- **Source pod:** stunnel runs as TLS **client** on `127.0.0.1:<local>`,
  presenting a client cert. CH's `vm.send-migration` targets that local
  port. stunnel TLS-dials `<dst-pod-ip>:6789`, verifies the server cert
  against the CA, streams.
- **Cert material:** the `migration-leaf` Secret (`mtls-spike`
  namespace) mounted into both pods as `/etc/migration-tls/{tls.crt,
  tls.key,ca.crt}`. `usages: [server auth, client auth]` so one
  identity serves both ends (spike-only simplification).

**Where the dst-pod-IP gets injected into the src sidecar config** is a
real wiring question the spike should at least surface (env var written
by an init container? `kubectl exec`-driven sed? for the spike, hand-
configure the src stunnel with the known dst IP).

---

## 4. Spike questions

1. **CH-through-tunnel correctness.** Does CH's `vm.send-migration`
   (destination_url=localhost) and `vm.receive-migration`
   (receiver_url=localhost) complete cleanly with stunnel forwarding
   in between? Guest resumes intact (sentinel survives).
2. **Performance.** Throughput / transfer duration / downtime vs. the
   shipped plaintext baseline (~108 MB/s, ~38s transfer, ~2-3s downtime
   for a 4 GiB guest). Expectation: AES-NI ≫ gigabit, so impact should
   be minimal — but measure on real hardware.
3. **Enforcement (negative test).** A client presenting no cert / a
   cert from a different CA is **rejected** at TLS handshake. The
   transport actually authenticates, not just encrypts.
4. **Wiring sketch.** Where dst-pod-IP and cert material get injected
   into the sidecar config — enough to inform the controller-integration
   design.

**Out of scope for this spike (design-doc territory):** per-migration
cert identity / authz model (cert SAN binding the peer to *this*
migration), the controller-integration wiring (Certificate per pod vs.
per-node, issuance timing, rotation), audit events, the S1 annotations
→ SwiftMigration CR migration.

---

## 5. Trust-model framing (for the design doc after the spike)

The spike validates **transport** (mTLS authenticated channel). The
deeper trust questions the design doc must answer, informed by what the
spike observes:

- **Identity model.** What does each side's cert *assert* — pod-name
  SAN? ServiceAccount? SPIFFE? — and how does each side *authorize*
  (not merely authenticate) the peer? mTLS only proves "this peer has
  a CA-signed cert"; it does **not** prove "this is the legitimate src
  for THIS migration." Binding to a specific migration needs either
  per-migration cert identity (SAN = migration UID) OR a pre-shared
  per-migration secret carried in-band.
- **Trust anchor + cert lifecycle for dynamic pods.** cert-manager
  Certificate→Secret→Volume, but the dst pod is created *per migration*
  by the controller. Per-migration certs add issuance latency to every
  migration (and a cert-manager dependency in the cutover hot path);
  longer-lived per-node/per-swiftletd certs avoid latency but weaken
  per-migration authz. The spike's cert-lifecycle observations should
  inform this choice.
- **Composition with S1 (URLs-from-CR).** mTLS **does** close
  "redirect to an arbitrary attacker endpoint" (attacker has no
  CA-signed cert). It does **not** close "redirect to a different
  *valid* migration pod" (which has a valid cert) nor "operator-
  writable inputs." So **both** mTLS *and* URLs-from-CR are needed for
  defense-in-depth; design must do both, neither subsumes the other.
- **What the spike must prove to call the transport secure (Q3
  negative test):** peer with no cert *and* peer with wrong-CA cert
  both rejected at handshake.

---

## 6. Layer A status (DONE)

cert-manager resources at `tools/spike/phase-3c-mtls/certs.yaml`,
applied to namespace `mtls-spike`. Hierarchy:

- `Issuer/selfsigned` (bootstrap)
- `Certificate/mtls-ca` (self-signed CA root, ECDSA P-256, isCA=true,
  duration 1y) → `Secret/mtls-ca`
- `Issuer/mtls-ca-issuer` (references CA secret)
- `Certificate/migration-leaf` (commonName `kubeswift-migration`, dnsName
  `kubeswift-migration`, ECDSA P-256, **usages: server auth + client
  auth**, duration 1y) → `Secret/migration-leaf` carrying
  `ca.crt`/`tls.crt`/`tls.key`

Verified on cluster: both Certificates `Ready=True`; the
`migration-leaf` Secret has all three keys. Either reuse the live
state on resume, or `kubectl apply -f` the manifest fresh.

---

## 7. Layers B–D plan (to resume)

### Layer B — stunnel mTLS transport + CH-through-tunnel (the crux)

1. Read `test/migration/manual/{source,destination,run}.sh` and
   `tools/manual-demo/phase-3b-pr1/launch-pods.sh` to understand
   exactly how the manual-demo rig templates launcher pods + drives
   the migration via `kubectl annotate`.
2. Augment the launcher pod template with an **stunnel sidecar
   container**:
   - Image candidate: a maintained stunnel container (e.g., `dweomer/stunnel` or build a tiny one from `alpine` + `stunnel`).
   - Mount `Secret/migration-leaf` at `/etc/migration-tls/`.
   - Stunnel configs (server on dst, client on src) — choose a port
     pair (e.g., CH listens on 127.0.0.1:6790, stunnel public on
     :6789; CH dials 127.0.0.1:6790 on src, stunnel-client dials
     `<dst-ip>:6789`).
   - For the spike, hand-write the dst-IP into the src stunnel config
     (controller-integration wiring is design-doc territory).
3. Apply two augmented launcher pods (one on miles, one on boba) and
   verify both sidecars are Ready + cert-mounted.
4. Drive a manual live migration via annotations with **localhost**
   URLs: `listen_url=tcp:127.0.0.1:6790` on dst,
   `target_url=tcp:127.0.0.1:6790` on src.
5. Assertions: migration succeeds, guest sentinel survives on dst, no
   plaintext traffic on the cross-pod path (could verify with a quick
   `tcpdump` capture showing only TLS handshake + encrypted records).

### Layer C — performance

Run baseline (current plaintext shipped path, kernel-boot 4 GiB
guest): downtime, transfer duration, throughput. Then the augmented
mTLS pods, same workload, same nodes. Compare. Expect minor delta;
record numbers for the design doc.

### Layer D — enforcement / negative test

- Start the dst with the leaf cert + CA; try src connection with no
  cert → reject at TLS handshake.
- Issue a leaf cert from a *different* self-signed CA in another
  namespace; src uses that; dst still using `mtls-spike` CA → reject.
- Both must be rejected at TLS handshake, not at CH layer.

### Findings doc (after Layer D)

`docs/design/live-migration-phase-3c-mtls-spike.md` records the
question-by-question answers, headline numbers, and the design-doc
decisions the spike informs (per-migration vs. longer-lived identity,
sidecar image choice, wiring sketch). Per the spike contract, the
spike branch is **not merged**; only the findings doc lands on `main`.

---

## 8. Open follow-ups to flag in the findings doc

These don't gate the spike but should be carried into the Phase 3c
design:

- **S1 annotations → SwiftMigration CR** (separate but composing
  workstream; see §5).
- **`migration-phase2-unsafe-plaintext: ack` gate** — once mTLS ships,
  this gate becomes a one-way switch: plaintext rejected on the
  controller side too, or the annotation key is deleted entirely (the
  THREAT-MODEL Phase 3 must-have).
- **Audit events** for migration phase transitions (THREAT-MODEL
  Phase 3 must-have #3).
- **`spec.allowVersionSkew` must-have is OBSOLETE** (retired in Phase
  3b spike — `newDstPod` clone-src structurally prevents skew). Drop
  it from the must-have list when the design doc is written.

---

## 9. Resume checklist for the next session

1. `git checkout spike/phase-3c-mtls && git pull origin spike/phase-3c-mtls`.
2. `KUBECONFIG=/home/wrkode/code/vmm-kubeswift/dev-tests/kubeswift/kubeswift-cluster.yaml kubectl get certificate -n mtls-spike` —
   verify Layer A is still live; if not, `kubectl apply -f
   tools/spike/phase-3c-mtls/certs.yaml`.
3. Read this doc §3 and §7 — that's the architecture and the
   step-by-step.
4. Start Layer B step 1 (read the manual-demo scripts).

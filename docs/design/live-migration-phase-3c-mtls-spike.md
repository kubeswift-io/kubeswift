# Live Migration Phase 3c — mTLS Transport Spike (Findings)

> **Status:** Complete. Ready for Phase 3c design alignment.
> Last updated: 2026-05-30.
> Spike branch `spike/phase-3c-mtls` is **NOT for merge** (spike contract);
> only this findings doc lands on `main`.

---

## Goal

Empirically validate that Cloud Hypervisor live migration runs unchanged
over a mutually-authenticated TLS channel terminated by a sidecar (stunnel),
**before** the Phase 3c design doc is written. Same discipline as prior
spikes: findings either confirm assumptions or correct them BEFORE design
begins.

The architecture under test (from
[`live-migration-phase-3c-spike-prep.md`](live-migration-phase-3c-spike-prep.md) §3):
Cloud Hypervisor speaks **plaintext to localhost only**; an stunnel sidecar
in the same pod netns owns the cross-pod TLS hop. No CH change, no swiftletd
change — the migration channel is wrapped, not rewritten.

```
src pod (miles)                                  dst pod (boba)
┌───────────────────────────┐                    ┌───────────────────────────┐
│ swiftletd/CH               │                    │ swiftletd/CH (receiver)    │
│  vm.send-migration         │                    │  vm.receive-migration      │
│  target_url=               │                    │  listen_url=               │
│   tcp:127.0.0.1:6790  ─────┼──┐              ┌───┼─► tcp:127.0.0.1:6790       │
│                            │  │ plaintext    │   │       (localhost only)     │
│ stunnel CLIENT             │  │ (localhost)  │   │ stunnel SERVER             │
│  accept 127.0.0.1:6790 ◄───┼──┘              └───┼── connect 127.0.0.1:6790   │
│  connect <dst-ip>:6789 ────┼───── mTLS ──────────┼─► accept 0.0.0.0:6789      │
└───────────────────────────┘   (cross-pod)       └───────────────────────────┘
```

**Cluster:** 3-node k0s (frida cp; miles + boba workers), CH v51.1,
`longhorn-migratable` RWX/Block storage, cert-manager. swiftletd image
`sha-e1db826` (unmodified shipped image — the spike adds no Rust change).

**Reproduction harness:** `tools/spike/phase-3c-mtls/` —
`launch-mtls-pods.sh` (Fork-B hand-crafted src/dst pair) +
`trigger-mtls-migration.sh` (annotation-driven migration) + `certs.yaml`
(Layer A cert-manager hierarchy) + the two stunnel configs.

---

## Headline result

**All four spike questions PASS.** CH live migration over mutual TLS is
correct, has negligible performance cost, and the channel genuinely
authenticates (both invalid clients rejected at the TLS handshake, zero
bytes reaching CH). The Q4 wiring exercise surfaced three controller-
integration findings and one trust-model gap (`verifyChain` without
subject checks) that the Phase 3c design doc must resolve.

| Q | Question | Result |
|---|---|---|
| Q1 | CH-through-tunnel correctness | **PASS** — sentinel md5 identical src→dst |
| Q2 | Performance vs. plaintext baseline | **PASS** — 111.86 MB/s, ~1% delta (within noise) |
| Q3 | Enforcement (negative test) | **PASS** — no-cert AND wrong-CA both rejected at handshake; 0 bytes to CH |
| Q4 | Wiring sketch | **Done** — 3 controller-integration findings + identity-pinning gap |

---

## Q1 — CH-through-tunnel correctness: PASS

A 4 GiB Ubuntu Noble guest (RWX/Block PVC) was cold-booted on the src pod,
an **in-memory tmpfs sentinel** was written (so survival proves MEMORY
transfer, not just the shared PVC), then migrated miles→boba entirely over
the TLS tunnel with **localhost CH URLs** on both ends.

- Pre-migration sentinel (src guest tmpfs): `MTLS-MEMPROOF-1780122581`,
  md5 `e187f76732140367822efbd7ac675019`.
- Post-migration sentinel (dst guest tmpfs): **md5 identical** —
  `e187f76732140367822efbd7ac675019`.
- src `migration-status = complete`; dst `migration-status = running`
  (successful cutover; guest live on boba).
- Guest IP `192.168.99.20` (br0 `192.168.99.0/24`, the B0 fix subnet).

CH's `vm.send-migration` (`target_url=tcp:127.0.0.1:6790`) and
`vm.receive-migration` (`listen_url=tcp:127.0.0.1:6790`) completed cleanly
with stunnel forwarding in between. **CH required no awareness of the
tunnel** — from its perspective it dialed/listened on localhost.

---

## Q2 — Performance: PASS (negligible overhead)

| Metric | mTLS (this spike) | Plaintext baseline (Phase 3b Q2/Q4) |
|---|---|---|
| `vm.send-migration` RPC duration | 38675 ms | ~38.2 s |
| Bytes across the cross-pod channel | 4326154986 (~4.03 GiB) | ~4.03 GiB |
| Effective throughput | **111.86 MB/s** | ~107–113 MB/s |
| observedDowntime class | ~2–3 s (cutover-bound) | ~2–3 s |

Throughput computed from the dst stunnel SERVER close-stats —
`4326154986 byte(s) sent to socket` (decrypted and forwarded to CH on
`127.0.0.1:6790`) over the 38.675 s RPC = **111.86 MB/s**. That is within
noise of the Phase 3b Q4 raw-TCP ceiling (112.75 MB/s) and the inferred CH
data-path rate (107.2 MB/s).

**Conclusion:** AES-NI ≫ gigabit, exactly as predicted. The TLS layer is
not the bottleneck on this hardware; the pod-network bandwidth is. No
dedicated migration network or hardware-crypto tuning is needed for Phase
3c on commodity gigabit interconnects. Operator sizing formula from Phase
3b (`pauseWindow ≈ guest_RAM × 1.05 / pod_network_bandwidth`) carries over
unchanged.

---

## Q3 — Enforcement (negative test): PASS

The transport must **authenticate**, not merely encrypt. stunnel `verify = 2`
on both ends (require AND verify the peer cert chains to the shared CA).

### Positive (both peers mutually verified)

From the live stunnel logs of the successful migration:

```
# dst SERVER verifying the src's CLIENT cert:
LOG5[0]: Service [migration] accepted connection from 10.244.125.169:54124
LOG5[0]: Certificate accepted at depth=0: CN=kubeswift-migration
LOG5[0]: Connection closed: 112 byte(s) sent to TLS, 4326154986 byte(s) sent to socket

# src CLIENT verifying the dst's SERVER cert:
LOG5[0]: s_connect: connected 10.244.213.54:6789
LOG5[0]: Certificate accepted at depth=0: CN=kubeswift-migration
```

Both directions verified the peer → **mutual TLS**, not one-way server auth.

### Negative Test A — client presents NO cert

src stunnel client config stripped of `cert`/`key`. The dst SERVER log:

```
LOG5[1]: Service [migration] accepted connection from 10.244.125.169:39524
... peer did not return a certificate ...
LOG5[1]: Connection reset/closed: 0 byte(s) sent to TLS, 0 byte(s) sent to socket
```

### Negative Test B — client cert from a DIFFERENT CA

A leaf (`CN=attacker`) issued by a second, unrelated self-signed CA. The
dst SERVER log:

```
LOG5[2]: Service [migration] accepted connection from 10.244.125.169:33334
LOG4[2]: Rejected by CERT at depth=0: CN=attacker
LOG5[2]: SSL_accept: ssl/.../statem_srvr.c: error:0A000086: ... certificate verify failed
LOG5[2]: Connection reset/closed: 0 byte(s) sent to TLS, 0 byte(s) sent to socket
```

**Both invalid clients are rejected at the TLS handshake; in both cases
`0 byte(s) sent to socket` — CH on `127.0.0.1:6790` is NEVER reached.** The
rejection is at the transport layer, not the CH layer. This is the bar the
spike-prep §5 set for calling the transport secure, and it is met.

---

## Q4 — Wiring sketch + findings for the design doc

The spike hand-crafted the pods (Fork B). Translating that into controller
integration (`internal/controller/swiftmigration`) surfaced four findings.

### Finding W-3c-1 — `lifecycle: stop` poisons the receiver pod

**Symptom:** the first run, both launchers logged
`lifecycle=stop, skipping launch` and neither booted/received.

**Root cause:** the crafted pods mounted the controller-managed
`<guest>-runtime-intent` ConfigMap. When the harness patches
`runPolicy: Stopped` to release the controller pod, the SwiftGuest
controller **rewrites that same ConfigMap to `lifecycle: stop`**.
swiftletd's launch gate (`rust/swiftletd/src/main.rs:201`,
`if intent.lifecycle == "stop" { skip launch }`) fires for **all** launch
paths — **including `migration_receiver_mode`**. Receiver mode is selected
by the env var `KUBESWIFT_MIGRATION_ROLE=receiver`, not by intent JSON, so
it does not exempt the pod from the lifecycle gate.

**Spike fix:** mint a frozen `lifecycle: run` intent ConfigMap and repoint
the `runtime-intent` volume on both crafted pods to it (now baked into
`launch-mtls-pods.sh`).

**Design-doc consequence (load-bearing):** the controller-integrated **dst
(receiver) pod must carry a `lifecycle: run` intent and the receiver role,
and must never inherit a stopped lifecycle from the source guest's intent.**
Since `newDstPod` DeepCopies the source pod (see W-3c-2), and the source
guest may legitimately be `runPolicy: Stopped`-adjacent during cutover,
the dst-pod construction path must explicitly set/freeze `lifecycle: run`
in the dst's intent — not reuse a live, controller-mutable intent CM that
could flip to `stop` mid-migration.

### Finding W-3c-2 — `newDstPod` DeepCopy needs role-aware sidecar config

The spike used a **single sidecar image** (`dweomer/stunnel:latest`) +
**single ConfigMap** carrying both server and client configs; the sidecar
entrypoint self-selects server-vs-client from `STUNNEL_ROLE` and injects
the peer IP from `DST_POD_IP` (sed over `__DST_POD_IP__`).

This maps directly onto Phase 3a's
[`dst_pod.go::newDstPod`](../../internal/controller/swiftmigration/dst_pod.go),
which constructs the dst by `srcPod.DeepCopy()`. The DeepCopy hands you the
**source's CLIENT-role sidecar config**; the controller must then **flip it
to SERVER on the dst** (and, symmetrically, the src sidecar must be a
CLIENT pointed at the dst IP). Concretely the controller must, post-DeepCopy:

- set `STUNNEL_ROLE=server` on the dst sidecar (the src is `client`);
- set `DST_POD_IP` on the **src** sidecar to the dst pod IP — which is
  only known **after** the dst pod is scheduled and gets an IP. This is a
  **sequencing constraint**: dst pod created → dst IP observed → src
  sidecar (re)configured with that IP. The spike resolved this by crafting
  the dst first, reading its `podIP`, then crafting the src. The controller
  state machine has the same ordering already (Preparing-live creates the
  dst before StopAndCopy on the src), so the dst IP is available to stamp
  onto the src — but the **src sidecar config must be parameterized by env,
  mutated after DeepCopy, not baked into an image.** A future refactor that
  bakes role/peer into the image would break the single-image model and
  re-introduce a build-time coupling. (Same W26-class lesson: name the
  load-bearing property so a "cleaner" refactor doesn't regress it.)

### Finding W-3c-3 — `runPolicy: Stopped` is reactive-only (restated)

The harness's first cut waited for the controller pod to disappear after
`runPolicy: Stopped`; it never did. `runPolicy: Stopped` prevents the
controller from **recreating** the pod but does **not delete** the running
one (the Phase 1 stop-guard finding). The spike fix patches Stopped
(no-recreate) then explicitly `kubectl delete pod --wait`. Not a Phase 3c
design item per se, but it confirms — again — that any controller path that
needs a launcher pod **gone** must delete it, never assume Stopped does.

### Finding W-3c-4 (trust-model gap) — `verifyChain` without subject checks

The src stunnel CLIENT logged, at startup:

```
LOG4[ui]: Service [migration] uses "verifyChain" without subject checks
```

stunnel `verify = 2` validates that the peer cert **chains to the CA** — it
does **NOT** pin the peer's subject/CN/SAN. In this spike **both** pods
present the **same** leaf (`CN=kubeswift-migration`, a single shared
`migration-leaf` Secret), so any peer holding any CA-signed cert is
accepted. That is exactly the spike-prep §5 "authenticate ≠ authorize"
gap made concrete: mTLS here proves "this peer has a CA-signed cert"; it
does **not** prove "this is the legitimate src/dst for THIS migration."

This is the central question the Phase 3c **design doc** must answer (it is
explicitly out of scope for the spike). Options, with the trade-off the
spike clarifies:

- **Shared long-lived leaf (what the spike used).** Zero issuance latency
  in the cutover hot path; weakest authz (any CA-signed peer accepted).
  Acceptable only if the CA is tightly scoped (e.g., per-cluster, only
  swiftletd pods can mount it).
- **Per-node / per-swiftletd identity** (SAN = node or ServiceAccount) +
  stunnel `verify = 4` / `checkHost`. Pins peer to a known identity;
  still not per-migration; no per-migration issuance latency.
- **Per-migration identity** (SAN = migration UID), cert-manager
  Certificate per migration. Strongest authz (binds the channel to THIS
  migration); adds cert issuance to **every** migration's critical path
  and a hard cert-manager dependency in cutover. The spike did not measure
  issuance latency — design must, if this option is on the table.

**Whatever the choice, `verify = 2` alone is insufficient for production:
the design must add subject/SAN pinning** (`verify = 4` + `checkHost`, or
per-peer cert constraints), or the channel authenticates "a swiftletd" but
not "the right swiftletd."

---

## Trust-model carry-forwards (for the Phase 3c design doc)

From spike-prep §5, now informed by what the spike observed:

- **mTLS does NOT subsume S1 (URLs-from-CR).** mTLS closes "redirect to an
  arbitrary attacker endpoint" (attacker has no CA-signed cert — proven by
  Q3 Test B). It does **not** close "redirect to a different *valid*
  migration pod" (which has a valid cert under a shared-leaf model — see
  W-3c-4) nor "operator-writable annotation inputs." **Both** mTLS *and*
  URLs-from-SwiftMigration-CR are required for defense-in-depth; neither
  subsumes the other. The Phase 2 S1 work (read URLs from the CR, not pod
  annotations) remains a Phase 3c must-have alongside this transport work.
- **`migration-phase2-unsafe-plaintext: ack` becomes a one-way switch.**
  Once mTLS ships, the design must either reject plaintext on the
  controller side too, or delete the annotation key entirely
  (THREAT-MODEL Phase 3 must-have). The spike still relied on the ack gate
  (the shipped swiftletd image enforces it); Phase 3c retires it.
- **`spec.allowVersionSkew` is OBSOLETE** — retired in the Phase 3b spike
  (`newDstPod` clone-src structurally prevents skew). Do not carry it into
  the Phase 3c API surface.
- **Audit events** for migration phase transitions remain a THREAT-MODEL
  Phase 3 must-have (not exercised by this spike).

---

## What the spike did NOT cover (explicitly design-doc territory)

- Per-migration cert identity / authz (SAN binding the peer to THIS
  migration) and the cert-issuance-latency measurement that choice needs.
- Controller-integration wiring beyond the sketch in W-3c-1/2: Certificate
  per pod vs. per-node, issuance timing, rotation, secret mount plumbing.
- The S1 annotations→SwiftMigration-CR migration (separate composing
  workstream).
- RWO disk-boot live migration (orthogonal storage-architecture question;
  this spike used RWX/Block like Phase 3a/3b).
- tcpdump confirmation that the cross-pod path carries only TLS records —
  inferred from the stunnel close-stats (`0 byte(s) sent to socket` on
  rejects; encrypted bytes only on the accepted path) rather than a packet
  capture. A packet capture is a cheap belt-and-suspenders add if the
  design review wants it.

---

## Reproduction

```bash
export KUBECONFIG=.../kubeswift-cluster.yaml
cd tools/spike/phase-3c-mtls

# Layer A — cert hierarchy (idempotent)
kubectl apply -f certs.yaml          # ns mtls-spike; migration-leaf Secret

# Layer B — bring up the mTLS src/dst pair (folds in the choreography fix)
./launch-mtls-pods.sh

# write an in-memory sentinel into the src guest (serial console), then:
export NS=mtls-spike SRC_POD=mtls-guest-mig-src DST_POD=mtls-guest-mig-dst
./trigger-mtls-migration.sh

# Layer C/D evidence
kubectl -n mtls-spike logs mtls-guest-mig-dst -c stunnel   # SERVER: verify + bytes
kubectl -n mtls-spike logs mtls-guest-mig-src -c stunnel   # CLIENT: verify + warning
```

The spike branch `spike/phase-3c-mtls` carries the harness, the certs, the
stunnel configs, and the Layer A→D walkthrough. **It is not for merge** —
only this findings doc lands on `main`.

# In-guest vsock identity agent — regenerate clone identity without a reboot

> Status: DESIGN — **PR 0 spike COMPLETE / PASS (2026-06-15)**, see
> [`clone-identity-vsock-agent-spike.md`](clone-identity-vsock-agent-spike.md).
> The spike confirmed every load-bearing assumption (vsock device + agent survive
> CH v52 `--restore`; identity regen + MAC-set + re-DHCP work **in place, no
> reboot**) and **corrected one framing**: "distinct IP across clones" is a
> non-goal — per-pod-netns isolation makes the agent's real job *re-DHCP-for-status
> + MAC-alignment*, not a globally-unique guest-internal IP (§Q4 note). Proceed to
> PR 1. Supersedes the reboot-based clone-identity remedy, which is BROKEN on
> Cloud Hypervisor v52
> ([`known-issues-clone-reboot-firmware-hang.md`](known-issues-clone-reboot-firmware-hang.md)).
> Anchors on the Snapshot Phase 2 resume-vs-boot limitation
> ([`snapshots.md`](snapshots.md) + context doc "Known limitation: identity
> regeneration on clone resume") and Snapshot Phase 4 cloneFromSnapshot
> ([`snapshot-phase-4-clonefromsnapshot.md`](snapshot-phase-4-clonefromsnapshot.md)).
> Last updated: 2026-06-15.

## 1. Problem

A `SwiftGuest.spec.cloneFromSnapshot` guest boots via Cloud Hypervisor
`--restore` — it **resumes** the source's captured RAM byte-for-byte (CH v52
`resume=true`, see `swift-ch-client/src/spawn.rs::restore_args`). A resume is
**not a boot**: cloud-init does not re-run, systemd-firstboot does not run, and
the guest kernel's device/driver state is the source's. Every clone therefore
inherits the source's **guest-visible identity**:

| Identity field | After clone resume | Set per-clone today? |
|---|---|---|
| `/etc/machine-id` | inherited from source | no |
| `/etc/ssh/ssh_host_*_key*` | inherited from source | no |
| hostname | inherited from source | no |
| guest-visible `eth0` MAC (virtio-net driver cache) | inherited from source | no |
| guest's DHCP-acquired IP (lease in RAM) | inherited; no re-DHCP on resume | no |
| hypervisor `config.net[].mac` (host-side tap/bridge fdb) | **rewritten per-clone** (`clonecommon.ComputeMACRewrites`) | yes |
| pod network namespace | per-clone (Kubernetes-isolated) | yes |

The per-pod-netns isolation makes N coexisting clones **L2-collision-safe**
regardless of the shared guest MAC, so clones are usable today as **warm,
read-mostly replicas** that share the source's identity. They are NOT
independently addressable: `status.network.primaryIP` stays empty (no re-DHCP),
and machine-id/SSH-key/hostname collisions break anything that keys on them
(dbus, journald dedup, SSH host-key pinning, cluster membership).

The documented remedy was **"reboot the clone once"**: the controller appends
`kubeswift.clone=true` to the snapshot's config.json cmdline
(`internal/snapshot/configjson/`), and the source's `SwiftSeedProfile` ships a
`bootcmd` (`config/samples/seed-profiles/clone-identity-regen.yaml`) that, gated
on the marker + a sentinel, regenerates machine-id / SSH keys / hostname and the
guest re-DHCPs on the reboot. **That remedy is broken on CH v52**: a `--restore`d
guest's reboot hangs in EDK2 firmware (S3-resume / AP-init, freezes after
`MpInitChangeApLoopCallback`) — a normal guest reboots fine
([`known-issues-clone-reboot-firmware-hang.md`](known-issues-clone-reboot-firmware-hang.md),
cluster-isolated 2026-06-15). The firmware never reaches the kernel, so identity
is never regenerated and no IP is ever discovered. That is an upstream CH/EDK2
issue, not KubeSwift code.

**The vsock identity agent is the real fix: regenerate identity + renew the IP
in place, with no reboot.**

## 2. Goals / Non-goals

### Goals

- Regenerate a clone's `/etc/machine-id`, SSH host keys, and hostname **in
  place** (no reboot), driven by the existing
  `CloneFromSnapshotSource.Regenerate []CloneIdentityItem` list.
- **Re-DHCP the clone in place** (no reboot) so a fresh lease lands in the clone's
  own per-pod dnsmasq and surfaces in `status.network.primaryIP` via the existing
  lease-poller path (v0.4.3 `LEASE_POLL_ATTEMPTS_RESTORE`). (Spike correction: the
  goal is *re-DHCP-for-status*, **not** a globally-distinct guest-internal IP —
  per-pod-netns isolation already separates clones; see §Q4 note + the spike doc.)
- A **host↔guest-only** control channel (vsock) — not network-reachable, minimal
  command surface (identity-regen only, no arbitrary exec).
- **Opt-in, fail-safe**: images without the agent keep today's
  inherited-identity behavior with a loud, status-visible "agent absent" signal
  — never a silent failure (Design Principle #6).

### Non-goals

- **Not** a general guest agent (no exec, no file transfer, no metrics — those
  are a separate future surface if ever wanted). The agent does identity-regen
  and nothing else in v1.
- **Not** a fix for the CH v52 restore-then-reboot firmware hang itself — this
  sidesteps the reboot. The hang stays an upstream-CH tracked issue; if a future
  CH fixes it, the bootcmd path can co-exist (see §6.6) but the agent remains the
  primary path.
- **Not** Windows (osType: windows) in v1 — cloudbase-init + Windows identity
  regen (sysprep/Restart-Computer) is a different mechanism; out of scope, noted
  in §6.7.
- **Not** live-migration related. Live migration deliberately preserves identity
  byte-for-byte; the agent is never invoked on a migration receiver.

## 3. Architecture

```
                         launcher pod (per clone, own netns)
  ┌──────────────────────────────────────────────────────────────────────┐
  │  swiftletd (host side)                                                 │
  │  ───────────────────                                                   │
  │  action loop (action.rs)  ──dispatch IdentityRegen──┐                  │
  │   (annotation-driven, new                           │                  │
  │    "identity" KeySet)                                ▼                  │
  │                                          ┌───────────────────────┐     │
  │                                          │ swift-vsock-client     │     │
  │                                          │ connect AF_VSOCK port  │     │
  │                                          │ over the unix socket   │     │
  │                                          └──────────┬────────────┘     │
  │                                                     │                  │
  │  cloud-hypervisor  --vsock cid=<N>,socket=<rt>/vsock.sock              │
  │   (spawn.rs / config.rs)                            │                  │
  └─────────────────────────────────────────────────────┼─────────────────┘
                  vsock AF_VSOCK (host CID 2 ↔ guest CID │N), localhost-only
  ┌─────────────────────────────────────────────────────▼─────────────────┐
  │  GUEST (resumed from snapshot; cloud-init did NOT re-run)              │
  │  kubeswift-guest-agent.service  — listens AF_VSOCK port 1024           │
  │    on RegenerateIdentity{items, mac, hostname}:                        │
  │      machine-id  → systemd-machine-id-setup                            │
  │      sshHostKeys → ssh-keygen -A + reload sshd                         │
  │      hostname    → hostnamectl set-hostname                            │
  │      macAddresses→ ip link set eth0 address <mac>                      │
  │      IP          → dhclient -r eth0 && dhclient eth0  → return new IP  │
  │    reply {ok, newIP, regenerated[]}                                    │
  └────────────────────────────────────────────────────────────────────────┘
```

The agent is a tiny static binary + systemd unit that **must already be running
in the SOURCE guest at snapshot-capture time** so it is part of the captured RAM
and resumes — alive and listening — in every clone (the load-bearing constraint,
§6.1). The host side reuses the existing annotation-driven action-loop surface;
the only genuinely new runtime piece is a thin vsock client in swiftletd and the
`--vsock` arg on the CH spawn.

The host's host-side AF_VSOCK CID is always **2** (`VMADDR_CID_HOST`). The guest
CID is the per-guest `cid=<N>` we assign on the CH `--vsock` arg; CH bridges the
guest's AF_VSOCK to the host-side **unix socket** we name under the runtime dir.

## 4. Resolved decisions

### Q1 — Agent delivery (THE crux): cloud-init install from a KubeSwift-shipped seed snippet, opt-in via the SwiftSeedProfile; golden-image bake is the documented power path

**Decision.** KubeSwift ships:

1. **The agent binary** built in-repo (`cmd/kubeswift-guest-agent`, a small
   static Go binary — language rationale below) and **embedded in the swiftletd
   image** at a stable path (e.g. `/usr/local/share/kubeswift/guest-agent/<arch>/
   kubeswift-guest-agent`). It is NOT a container the guest runs; it is a file
   that must land **inside the guest filesystem**.
2. **A canonical `SwiftSeedProfile` cloud-init snippet** (a sample +
   documented include) whose `write_files` drops a tiny systemd unit and whose
   `runcmd` fetches-or-installs the agent binary into the guest and `systemctl
   enable --now`s it on the SOURCE guest's **first boot**, BEFORE the snapshot is
   taken.

**How the binary gets into the guest** is the sub-decision. Two mechanisms, both
shipped:

- **(a) Primary — install via the source's cloud-init over a virtiofs read-only
  share.** The launcher already has a virtiofs path (`launch.rs::spawn_virtiofsd`
  + `--fs`). For a source guest opted into the agent, the controller mounts the
  swiftletd image's agent directory as a read-only virtiofs share (tag e.g.
  `kubeswift-agent`); the seed `runcmd` does `mount -t virtiofs kubeswift-agent
  /mnt/...; install -m0755 .../kubeswift-guest-agent /usr/local/bin/; systemctl
  enable --now kubeswift-guest-agent`. This keeps "what KubeSwift ships" as a file
  in the swiftletd image (one source of truth, version-pinned with the image) and
  needs no external hosting. **This requires the source guest to declare it wants
  the agent** (an opt-in seed profile + a controller flag to attach the share) —
  it is not automatic.
- **(b) Power path — bake into the golden image.** Operators building a golden
  SwiftImage install the agent + unit at image-build time (documented `apt`/`dnf`
  package or a `curl | install` in their image recipe). The snapshot of any guest
  from that image then carries a running agent with zero per-guest seed wiring.
  This is the recommended path for production fleets.

**Opt-in & fallback (Principle #6 — no silent failure).** The agent's presence is
**not assumed**. When the controller drives a regen and the agent does not answer
on vsock within a bounded window, the clone:

- keeps today's inherited-identity behavior (still usable as a warm replica),
- gets a status condition `CloneIdentityRegenerated=False` with reason
  `GuestAgentUnreachable` and a message pointing at the seed-profile / golden-image
  docs,
- and the documented limitation (shared identity, empty primaryIP) is restated in
  the runbook.

This is loud and actionable, not a silent degrade.

**Rejected alternatives.**

- *Deliver the agent over a dedicated virtio device at clone time.* Doesn't work:
  the guest never re-enumerates devices on resume (no boot), and the agent must
  already be RUNNING in the captured RAM. Anything delivered only at clone time is
  too late. (This is the same constraint that kills "install the agent on the
  clone".)
- *Bake the agent into swiftletd and have swiftletd push it in over vsock at clone
  time.* Same fatal flaw: nothing is listening on vsock in the guest until the
  agent is running, and the agent can't be running unless it was in the snapshot.
  Chicken-and-egg.
- *Make the agent mandatory (every image must ship it).* Violates minimalism +
  the "all modern distributions supported" promise; KubeSwift must degrade
  gracefully on a stock cloud image. Opt-in with a loud fallback is the discipline.

### Q2 — vsock transport: CH `--vsock cid=<N>,socket=<rt>/vsock.sock`; CID derived deterministically; the device MUST be present on the SOURCE snapshot

**Decision.** Add a `--vsock cid=<N>,socket=<runtime-dir>/vsock.sock` argument to
the CH spawn for guests that use the agent. Conventions:

- **Unix socket path** mirrors the existing `ch.sock` / `serial.sock` /
  `qmp.sock` runtime-dir paths: `<runtime-dir>/vsock.sock`
  (`RuntimeDir::root().join("vsock.sock")`, cleaned up by
  `launch.rs::remove_stale_sockets` exactly like the others — same SIGKILL
  stale-socket hazard).
- **Which side connects:** the host (swiftletd) is the **client**. CH listens on
  the host-side unix socket; swiftletd connects to it and speaks the AF_VSOCK
  framing (the `CONNECT <port>\n` handshake CH's vsock backend expects), targeting
  the guest agent's AF_VSOCK port. The guest agent is the AF_VSOCK **server**
  (listens on a fixed port, see Q3).
- **CID assignment:** deterministic per guest, derived from `(namespace, name)`
  the same way MACs are (`runtimeintent.InterfaceMACSeed` style hashing into the
  valid CID range, ≥ 3; CIDs 0–2 are reserved). Stored implicitly (recomputable);
  also surfaced read-only in the runtime intent so swiftletd and any debug tooling
  agree. Uniqueness is **per-host** (one CH per launcher pod, isolated netns/PID),
  so even a hash collision across pods is harmless — vsock is pod-local.
- **`--vsock` is added by `swift-ch-client` config/spawn** (a new `vsock:
  Option<VsockConfig>` field on `VmConfig`, emitted in `to_args()` next to the
  other devices; the restore path needs its own handling — below).

**The capture-time constraint (load-bearing, ties to Q1).** CH captures the
guest's **device state** in the snapshot. The vsock device — like every virtio
device — is part of that captured state. So **the SOURCE guest must have been
launched WITH `--vsock` for the restored clone to have a working vsock device**.
This means:

- The controller sets `--vsock` on the **source** guest whenever it is a snapshot
  source that opted into the agent (the same opt-in that attaches the agent, Q1).
- On `--restore`, CH re-opens the devices from the snapshot's `config.json`,
  which will list the vsock device with the source's socket path. As with seed.iso
  and disk paths (already handled for clones), the clone's runtime-dir differs, so
  the vsock **socket path** in config.json must be **patched to the clone's
  runtime-dir path** — this extends the existing `configjson` patcher
  (`internal/snapshot/configjson/`, which already rewrites disk paths and the
  serial socket; the vsock socket is the same class of per-pod path). The **CID**
  is captured in state and resumes as-is; we do not re-CID a restored guest (the
  guest kernel's vsock is bound to the captured CID — changing it host-side would
  desync). Per-pod isolation makes the inherited CID safe.

If the source was launched **without** `--vsock`, the snapshot has no vsock device
and the clone cannot be reached — this collapses into the Q1 fallback
(`GuestAgentUnreachable`), loud and documented. The webhook can warn at clone-time
if the referenced snapshot's `CapturedGuestSpec` indicates no agent (best-effort;
the authoritative signal is the runtime no-answer).

**Rejected alternatives.**

- *Per-migration / ephemeral CID.* No — the CID is baked into captured guest
  state; it can't be reassigned on resume without the guest noticing.
- *Network socket instead of vsock.* Defeats the security boundary (vsock is
  host↔guest only, unroutable); and the clone has no usable IP yet anyway, which
  is the whole point.

### Q3 — Protocol: minimal length-prefixed JSON request/response over a fixed AF_VSOCK port, versioned

**Decision.** The agent listens on a fixed AF_VSOCK port (**1024**, above the
privileged range, documented constant shared between agent and
`swift-vsock-client`). One request/response per connection, newline-or-length-
prefixed JSON (mirrors how the action surface stays tiny and the snapshot
`config.json` is JSON; serde-friendly).

**Request (host → guest):**

```json
{
  "v": 1,
  "op": "regenerate-identity",
  "items": ["machineId", "sshHostKeys", "hostname", "macAddresses"],
  "mac": "52:54:00:7e:0c:47",
  "hostname": "ft-clone-a",
  "renewLease": true
}
```

- `items` maps **directly** from `CloneFromSnapshotSource.Regenerate
  []CloneIdentityItem` (`hostname` / `machineId` / `sshHostKeys` /
  `macAddresses`). Empty list defaults to all four (matches the CRD default).
- `mac` is the per-clone hypervisor MAC the controller already computes
  (`clonecommon.ComputeMACRewrites`) — pushed to the guest so the **guest-visible**
  MAC matches the host-side fdb (resolving the long-standing split where only the
  hypervisor MAC was rewritten). Present only when `macAddresses ∈ items`.
- `hostname` is the controller-chosen name (defaults to the clone's
  `metadata.name`); the agent uses it directly instead of deriving from machine-id.
- `renewLease` asks the agent to re-DHCP after the MAC change.

**Response (guest → host):**

```json
{ "v": 1, "ok": true, "regenerated": ["machineId","sshHostKeys","hostname","macAddresses"],
  "newIP": "192.168.99.12", "error": null }
```

- `newIP` is the address from the agent's `dhclient` renew — fed back so the
  controller (or the lease poller, §Q4) can land it in `status.network.primaryIP`.
- `regenerated` echoes what actually ran (an old agent that doesn't understand an
  item omits it — forward-compat).

**Versioning / handshake.** `v` is the protocol version. **Degrade rules:**

- New host + old agent: agent ignores unknown fields (serde default) and omits
  unsupported items from `regenerated`; the host treats any item it asked for but
  didn't get back as not-regenerated and reflects that in status (no false claim).
- Old host + new agent: the agent accepts `v:1` requests; new ops are additive.
- An agent that doesn't recognize `op` returns `{ok:false, error:"unknown op"}` —
  the host surfaces it, never silently succeeds.

Command surface is deliberately ONE op in v1 (`regenerate-identity`). No exec, no
file ops. (A `ping`/`hello` op for liveness probing is the only other candidate;
fold it in if the spike wants an explicit readiness check before the regen.)

### Q4 — Trigger flow: swiftletd's action loop drives it (annotation-driven, new "identity" KeySet), after GuestRunning; the agent-returned IP feeds the existing restore lease-poller path

**Decision.** Reuse the **annotation-driven action loop** (`rust/swiftletd/src/
action.rs`) — the same pattern as `kubeswift.io/snapshot-action` and
`kubeswift.io/migration-action`. Add a third **`identity` namespace KeySet**
(`IDENTITY_KEYS`) with:

- controller writes: `kubeswift.io/identity-action` (verb `regenerate`),
  `…/identity-action-id`, `…/identity-action-args` (the JSON request from Q3).
- swiftletd writes: `kubeswift.io/identity-status` (`running`/`ready`/`failed`),
  `…/identity-status-id`, `…/identity-status-detail`, and a new
  `kubeswift.io/identity-new-ip` carrying the agent-returned IP.

**Who drives & when.** The **SwiftGuest controller** (not a new controller)
stamps `identity-action: regenerate` once the cloneFromSnapshot guest reaches
**GuestRunning=True** (CH v52 auto-resumes via `resume=true`, so the VM is
running, not paused — `controller.go` already keys off this). The action is
**one-shot, idempotent** via the action-id (the decide()/`last_completed_id`
machinery already guarantees exactly-once), mirroring how the former
`resumeCloneIfNeeded` sent a one-shot `snapshot-action: resume`.

swiftletd's `dispatch` gains an `ActionKind::IdentityRegenerate` arm that calls
the new `swift-vsock-client` to connect `<runtime-dir>/vsock.sock`, send the Q3
request, await the reply (bounded timeout, e.g. 30 s), and:

1. write `identity-status: ready` + `identity-new-ip: <newIP>` on success, or
   `identity-status: failed` with a sanitized detail on agent error/timeout
   (`GuestAgentUnreachable` when no connect/no answer).

**Reconciling with the lease poller (v0.4.3).** The clone's lease poller is
already alive for the pod's lifetime (`LEASE_POLL_ATTEMPTS_RESTORE`, because
`is_restore()`), watching `dnsmasq.leases`. When the agent runs `dhclient -r &&
dhclient`, the per-pod-netns **dnsmasq issues a fresh lease**, which the poller
discovers and writes to `kubeswift.io/guest-ip` → `status.network.primaryIP`
**by the existing path, with no new plumbing**. The `identity-new-ip` annotation
is a **belt-and-suspenders** secondary surface (the agent already knows the IP it
got, so reporting it directly removes the race where the controller patches status
before the lease file is written). The controller prefers the lease-poller
`guest-ip` (authoritative, comes from dnsmasq) and uses `identity-new-ip` only to
short-circuit the wait. **This is the resolution of "compose with the v0.4.3
fix": the agent's `dhclient` is precisely what finally gives the alive poller a
lease to find.**

> **MAC-vs-renew sub-question (called out in the prompt).** Is a MAC change
> actually *required* for a distinct IP? Each clone has its **own pod-netns
> dnsmasq** with its own lease DB, so the inherited source MAC would NOT collide
> across clones at the dnsmasq layer — a bare `dhclient -r && dhclient` renew
> *might* suffice to get an address. **BUT** the source's lease in the new
> dnsmasq's DB is keyed by the source MAC, and the guest still presents the
> source's MAC, so dnsmasq may hand back the *same* address the source had — not
> distinct, and worse, two clones on the **same node** (Tier B is node-local!)
> share one dnsmasq and WOULD then collide on MAC → same IP / lease thrash.
> **Decision: set the per-clone MAC (`ip link set eth0 address <mac>`) AND renew.**
> The MAC is the per-clone-unique key the controller already computes; setting it
> guarantees distinct dnsmasq lease identities, which guarantees distinct IPs, and
> aligns the guest-visible MAC with the host fdb. The spike must confirm the
> `ip link set` → `dhclient` ordering works cleanly on a live virtio-net link
> without a link bounce that wedges (and whether NetworkManager vs ifupdown vs
> systemd-networkd in the guest needs different renew commands — likely the agent
> shells `dhclient` directly and falls back to `networkctl`/`nmcli`).
>
> **SPIKE RESULT (2026-06-15 — corrects this decision).** `ip link set` on the
> resumed link works with NO wedge (ping 2/2, fdb learned the new MAC, ARP
> REACHABLE on it). **But the MAC change does NOT produce a distinct IP** — the
> guest re-requests its in-RAM address on renew and dnsmasq grants it back. The
> "two clones share one dnsmasq" premise is **FALSE in production**: each clone is
> its own launcher pod with its own netns + own dnsmasq, so the guest-internal IP
> is pod-isolated and need not be distinct (the pod IPs already differ). **Revised:
> the per-clone MAC-set is for guest-visible-MAC ↔ host-`config.net[].mac` (fdb /
> DNAT) consistency, not IP-distinctness; the renew's job is to land a lease in
> this pod's dnsmasq for the v0.4.3 poller** (confirmed — the lease file gained the
> clone's entry). Renew order: `dhclient`, fallback `networkctl renew/reconfigure`
> (Noble = networkd); optional `ip addr flush` first for a clean lease. Evidence:
> [`clone-identity-vsock-agent-spike.md`](clone-identity-vsock-agent-spike.md).

**Rejected alternative.** *Controller connects to vsock directly (host process
outside the launcher pod).* The vsock unix socket lives in the launcher pod's
mount namespace under the runtime dir; only swiftletd (in-pod) can reach it.
Driving via swiftletd's action loop is the only clean path and reuses the proven
annotation surface + idempotency.

### Q5 — Security: vsock is host↔guest-only; agent runs as root in-guest; one-op surface; no migration/mTLS interaction

**Decision & trust boundary.**

- **vsock is not network-reachable.** AF_VSOCK host↔guest only; there is no
  route from the pod network or any other guest to this channel. The unix-socket
  endpoint is under the launcher pod's runtime dir (pod-local mount namespace),
  reachable only by swiftletd in that pod. So the channel's confines are the
  launcher pod itself — the same boundary that already holds CH's API socket and
  the serial socket.
- **The agent runs as root in the guest** (it writes `/etc/machine-id`,
  `/etc/ssh/ssh_host_*`, sets the link MAC, drives dhclient — all root-only). This
  is acceptable because the **command surface is exactly one op** with a fixed
  schema (regenerate-identity); there is **no exec, no path argument, no file
  transfer**. An attacker who somehow reached the vsock port could only ask the
  guest to regenerate its own identity — a self-inflicted, non-escalating action.
  The agent MUST validate inputs (MAC format, hostname charset) and refuse
  anything else; it must never shell out with attacker-controlled strings
  (hostname/MAC are validated then passed as argv, never interpolated into a
  shell).
- **No interaction with the live-migration mTLS / threat model.** That stack
  (`migration-stunnel`, the S1 URLs-from-CR concern, the plaintext-ack gate) is
  about a **cross-pod TCP** guest-state channel. The identity vsock is
  **intra-pod host↔guest**; it carries no guest RAM and crosses no node boundary.
  The two are orthogonal; the agent is never invoked on a migration receiver
  (`is_migration_receiver()` guests skip identity-regen). No ack gate, no TLS —
  the transport's locality is the control.

### Q6 — Composition

- **`cloneFromSnapshot.Regenerate` list** — the agent **consumes** it directly
  (Q3 `items`). No CRD change to the list. `macAddresses` is no longer
  "host-side only": the agent now applies it guest-side too, closing the
  resume-vs-boot MAC gap.
- **The seed bootcmd path** (`clone-identity-regen.yaml` + the configjson cmdline
  marker `kubeswift.clone=true`) — the agent **replaces** it as the primary path.
  Disposition: **keep the bootcmd as a documented fallback** for (a) images with
  the agent absent AND a working reboot (i.e. *not* CH v52 — the very bug that
  motivated this), and (b) belt-and-suspenders if an operator both ships the
  bootcmd seed and reboots. They are not mutually exclusive (the bootcmd is gated
  on a sentinel + the cmdline marker; the agent writes the same files, so a
  later reboot's bootcmd sees the sentinel-absent state and is harmless/idempotent
  if it runs). **Recommendation: when the agent is present, the controller does
  NOT append the cmdline marker** (the agent owns regen), so the two never both
  fire on the same clone. Document the precedence: agent present → agent path;
  agent absent → bootcmd-on-reboot path (broken on CH v52; documented).
- **Snapshot-action annotation surface** — the identity action is a sibling
  KeySet in the same action loop; mutual exclusion across namespaces is already
  enforced at dispatch (`handle_pod_state`). A clone's lifecycle is: restore →
  GuestRunning → (one-shot identity-action) → steady state. No overlap with a
  concurrent snapshot/migration on the same guest (those are guarded).
- **SwiftGuestPool clone pools** — `cloneFromSnapshot` is templated per replica
  for free (pool DeepCopy). The controller drives one identity-action per replica;
  each replica gets its own deterministic MAC/CID/hostname from its own
  `(namespace, name)`. No pool-level change. (Same-node replica caveat from
  Phase 4 — the MAC-distinct lease identity per Q4 is exactly what makes
  same-node clones get distinct IPs.)
- **Windows guests (osType: windows)** — **out of scope v1.** A Windows agent
  would be a service speaking the same vsock protocol but using
  sysprep/`Rename-Computer`/`netsh`/cloudbase-init metadata; identity regen
  semantics and the reboot requirement differ materially. Noted as a follow-up;
  the protocol is OS-neutral enough to extend later.

## 5. Component-by-component change surface

| Component | Change | Files (cited) |
|---|---|---|
| **Go API** | No new CRD field (reuse `CloneFromSnapshotSource.Regenerate`). Add a status condition `CloneIdentityRegenerated` + reason enum; optionally echo the regenerated items in `status`. Add an **opt-in flag** for attaching the agent to a SOURCE guest (e.g. a `SwiftSeedProfile`-level or `SwiftGuest`-level boolean, or inferred from the seed including the agent snippet — spike to choose the least-surface signal). | `api/swift/v1alpha1/swiftguest_types.go` (`CloneFromSnapshotSource`, `SwiftGuestStatus`, conditions) |
| **SwiftGuest controller** | (1) For a source guest opted into the agent: set the CH `--vsock` device + attach the read-only agent virtiofs share (Q1a). (2) For a clone: drive the one-shot `identity-action: regenerate` after GuestRunning (mirrors the removed `resumeCloneIfNeeded`); read back `identity-new-ip` / lease-poller `guest-ip`; map to status. (3) Stop appending the `kubeswift.clone=true` cmdline marker when the agent is present. | `internal/controller/swiftguest/clone.go` (`prepareCloneFromSnapshot`, `cloneRestoreAnnotations`), `internal/controller/swiftguest/controller.go` (the GuestRunning/IP requeue block ~L515-528) |
| **configjson patcher** | Extend to rewrite the **vsock socket path** in the restored `config.json` to the clone's runtime-dir path (same class as the existing disk-path + serial-socket rewrites). | `internal/snapshot/configjson/configjson.go` |
| **clonecommon** | The CID derivation helper (deterministic from ns/name), alongside the existing `ComputeMACRewrites` / `RuntimeDirPrefix` primitives, so controller + intent agree. | `internal/snapshot/clonecommon/` (new `vsock.go`) |
| **runtimeintent (Go) + intent.rs (Rust)** | New `VsockIntent { cid, socket_path }` (or fold into the existing `RestoreIntent`): emitted by the controller, deserialized by swiftletd. Carries CID + (for the source) the socket path. | `internal/runtimeintent/types.go`, `rust/swiftletd/src/intent.rs` (new field on `RuntimeIntent`) |
| **swift-ch-client (Rust)** | `VmConfig.vsock: Option<VsockConfig>`; `to_args()` emits `--vsock cid=<N>,socket=<path>`; the **restore** spawn leaves vsock to config.json (CH re-opens it) — assert no `--vsock` leaks into `restore_args` (CH reads it from config.json), mirroring the existing "restore_args does not include --disk/--net" contract. | `rust/swift-ch-client/src/config.rs`, `src/spawn.rs` |
| **swift-vsock-client (NEW Rust crate)** | Thin AF_VSOCK-over-unix-socket client: connect `<rt>/vsock.sock`, do CH's vsock `CONNECT <port>` handshake, send/recv the Q3 JSON. Sync (mirrors the synchronous `swift-qemu-client` QMP choice — no tokio needed for one request/response). Workspace member. | `rust/swift-vsock-client/` (new), `rust/Cargo.toml` |
| **swiftletd action loop** | Add `IDENTITY_KEYS` KeySet + `ActionKind::IdentityRegenerate` + `dispatch_identity_regenerate` (connect vsock client, send request, write status + `identity-new-ip`). | `rust/swiftletd/src/action.rs` |
| **The guest agent (NEW)** | `cmd/kubeswift-guest-agent` — small **static Go** binary; AF_VSOCK listener on port 1024; the one regenerate-identity op (machine-id / ssh-keygen -A + sshd reload / hostnamectl / `ip link set` / dhclient). + a systemd unit `kubeswift-guest-agent.service`. Built `CGO_ENABLED=0`, multi-arch, **embedded into the swiftletd image** at a stable path for the virtiofs-install path. | `cmd/kubeswift-guest-agent/` (new), `images/swiftletd/Containerfile` (COPY the agent + unit into the image's share dir) |
| **Seed delivery** | A canonical `SwiftSeedProfile` sample whose `write_files` drops the unit and `runcmd` installs+enables the agent from the virtiofs share on the SOURCE's first boot. Replaces (deprecates, keeps as fallback) `clone-identity-regen.yaml`. | `config/samples/seed-profiles/` (new `guest-agent.yaml`), docs |
| **Webhook** | Best-effort warn at clone-time if the referenced snapshot indicates no agent (informational; runtime no-answer is authoritative). | `internal/webhook/swiftguest/validator.go` |
| **Docs** | Runbook update (`docs/snapshots/clone-from-snapshot.md` — replace the "reboot once / broken on v52" caveat with the agent flow); cross-ref the firmware-hang known-issue. | `docs/snapshots/clone-from-snapshot.md` |

### Language / packaging of the agent

**Static Go**, `CGO_ENABLED=0`, no libc dependency, runs on any glibc/musl guest.
Rationale: KubeSwift already builds Go binaries (the build infra is there);
`AF_VSOCK` is reachable via `golang.org/x/sys/unix` or `mdlayher/vsock`; a static
Go binary is the most portable thing to drop into an arbitrary Linux guest
(Ubuntu/Rocky/Debian/Fedora) without runtime deps. It is **not** a KubeSwift Rust
crate because it ships *inside the guest*, not in the swiftletd image's process
tree — keeping it Go avoids cross-compiling a Rust static binary for the guest
target and reuses the existing Go toolchain. ~200 LOC. The unit is a 6-line
`systemd` service (`ExecStart=/usr/local/bin/kubeswift-guest-agent`,
`Restart=always`, `After=network-pre.target`).

## 6. Phasing into PRs

| PR | Scope | Validatable |
|---|---|---|
| **PR 0 — spike** | Off-cluster + 1-node: launch a guest WITH `--vsock`, snapshot it, restore a clone, and prove a host process can reach a hand-run in-guest AF_VSOCK listener through the clone's `<rt>/vsock.sock` after resume (i.e. the device survives capture/restore). Confirm `ip link set eth0 address` + `dhclient` yields a distinct IP on a live link without wedging. Pin the CH `--vsock` socket handshake bytes. **Gate everything on this.** | Manual; the load-bearing "agent-in-snapshot resumes alive on vsock" claim |
| **PR 1 — guest agent + packaging** | `cmd/kubeswift-guest-agent` + unit + embed in swiftletd image + the canonical seed profile + golden-image docs. No host wiring yet; testable by hand-driving the vsock socket. | Agent answers regenerate-identity over vsock on a guest where it was installed pre-snapshot |
| **PR 2 — CH `--vsock` + intent + configjson patch** | `VmConfig.vsock`, `to_args`, intent field, CID helper, configjson vsock-path rewrite, controller sets `--vsock` on opted-in source + clone. CH spawns with the device; nothing drives it yet. | Source + clone come up with a working vsock device (manual probe); restore config.json carries the patched socket path |
| **PR 3 — swift-vsock-client + action loop `IdentityRegenerate`** | The Rust client crate + `IDENTITY_KEYS` + `dispatch_identity_regenerate` + status/`identity-new-ip` writes. Still operator-triggered (annotation). | Operator stamps `identity-action: regenerate` → clone regenerates identity + gets a new IP (manual) |
| **PR 4 — controller auto-drive + status** | Controller stamps the one-shot identity-action after GuestRunning; maps `identity-new-ip`/lease-poller `guest-ip` → `status.network.primaryIP`; `CloneIdentityRegenerated` condition; drop the cmdline marker when agent present; fallback path on no-answer. | End-to-end: a clone pool comes up with distinct identity + distinct IPs, no reboot |
| **PR 5 — cluster walkthrough + runbook** | Multi-replica clone pool on the dev cluster; update `clone-from-snapshot.md`; deprecate the bootcmd sample. | Cluster-validated; the demo-06 caveat becomes "works via the agent" |

## 7. Risks (lead with the load-bearing properties)

- **LBA-1 (THE constraint) — the agent must be in the SOURCE snapshot.** Every
  capability here depends on the agent already running in the captured RAM. A
  future refactor that tries to install/start the agent on the *clone* (because it
  "reads cleaner") silently re-breaks everything — the clone never boots, so
  nothing new starts. **State this explicitly at the controller injection site and
  in the agent's unit comment**, the way W26 / LBA-2 lessons demand: name the
  load-bearing property so a maintainer sees it before refactoring. The `--vsock`
  device, the CID, and the running agent are all captured-state properties, not
  clone-time properties.
- **LBA-2 — `--vsock` must be on the SOURCE launch, not just the clone.** Same
  class: CH captures the device. If a future change only adds `--vsock` to the
  restore/clone path (the intuitive place), the snapshot has no vsock device and
  the clone is unreachable. The controller MUST set it on the source whenever that
  source is an agent-opted snapshot source. (The webhook/runtime fallback catches
  the miss loudly, but the correct behavior is source-side.)
- **LBA-3 — CID/socket-path are per-pod; the restored CID is inherited, the
  socket path is patched.** Don't "helpfully" re-CID a restored guest (desyncs the
  guest kernel's bound vsock); don't reuse the source's socket path on the clone
  (wrong runtime dir). The configjson patcher owns the path rewrite; the CID rides
  the snapshot. A refactor that swaps these breaks reachability subtly.
- **No-silent-failure (Principle #6).** Agent-absent / device-absent / no-answer
  MUST surface as `CloneIdentityRegenerated=False` + a clear reason, never a
  silent "clone is fine" while it shares the source's identity and has no IP. The
  whole point of this feature is that the *current* state (silent shared identity,
  empty IP) is unacceptable; the fallback must be visible.
- **Guest-distro variance (needs the spike).** `dhclient` vs `networkctl renew`
  vs `nmcli`; `hostnamectl` vs `/etc/hostname`; `systemd-machine-id-setup`
  availability; `ssh-keygen -A` reading sshd_config. The bootcmd sample already
  encodes the fallbacks; the agent should port them. Risk: a distro where setting
  the MAC on a live link bounces it badly enough to wedge the resumed virtio-net.
- **Security surface creep.** The agent is root-in-guest; the discipline is ONE
  op, fixed schema, validated inputs, never shell-interpolated. A future "add an
  exec op for convenience" would turn a benign self-identity-regen channel into a
  guest-root RCE primitive over a channel the operator can reach. Guard the op set.
- **Migration interaction (none, but assert it).** The agent must never fire on a
  migration receiver (identity is preserved by design there). `is_migration_
  receiver()` already gates the receiver launch path; the identity-action must be
  controller-gated to cloneFromSnapshot guests only.

## 8. Open questions (flag what needs a spike)

1. **Opt-in signal for attaching the agent to a SOURCE guest.** A `SwiftSeedProfile`
   convention (the seed includes the agent snippet) vs an explicit
   `SwiftGuest`/class boolean vs always-on-for-snapshot-sources. Least-surface
   wins; spike to decide. (Always-on would simplify but attaches a vsock device +
   virtiofs share to guests that may never be snapshotted — minimalism tension.)
2. **CH `--vsock` socket handshake exact bytes** (the `CONNECT <port>\n` /
   `OK <cid>\n` framing CH's vsock backend uses). PR 0 must pin this against the
   shipped CH v52 binary; the `swift-vsock-client` is trivial once it's known.
3. **MAC-set-then-renew on a live virtio-net link** (Q4 sub-question) — does `ip
   link set eth0 address` mid-flight bounce the link cleanly, and does `dhclient`
   reliably get a *distinct* address? Per-distro renew command matrix. **Spike.**
4. **Does the restored guest's vsock device actually carry state correctly across
   CH `--restore`?** The whole design rests on it; PR 0 proves it empirically (a
   vsock device in a snapshot has not been exercised by KubeSwift before).
5. **Bounded-wait tuning** for the agent answer (30 s?) and the
   GuestRunning→identity-action delay (the agent's systemd unit must be up; on a
   resume it is *already* up in RAM, so ~immediate — but confirm `dhclient`
   readiness vs `network-pre.target`).
6. **Hostname source of truth** — controller passes `metadata.name`; should the
   operator be able to template it (pool ordinal)? Defer; default to clone name.
7. **Windows agent** (out of scope v1) — the protocol is OS-neutral; the
   regen mechanics (sysprep/Rename-Computer, which themselves want a reboot
   Windows handles differently) are a separate design. Flag, don't design.
8. **Tier C (s3) clones** — identical agent flow once the clone boots
   (cross-node), since the snapshot carries the agent + vsock device; only the
   `targetNode`/download differs (already handled). Confirm no node-locality
   assumption leaks into the CID/socket convention (it doesn't — both are pod-local).

## 9. Cross-references

- [`known-issues-clone-reboot-firmware-hang.md`](known-issues-clone-reboot-firmware-hang.md)
  — the CH v52 restore-then-reboot firmware hang this feature sidesteps (the
  motivating bug; its "next step #4" names this agent as the real fix).
- [`snapshot-phase-4-clonefromsnapshot.md`](snapshot-phase-4-clonefromsnapshot.md)
  — the cloneFromSnapshot boot path the agent runs on top of.
- [`snapshots.md`](snapshots.md) + context doc "Known limitation: identity
  regeneration on clone resume" — the resume-vs-boot inheritance rule (Phase 2)
  that is the root cause.
- `config/samples/seed-profiles/clone-identity-regen.yaml` — the bootcmd remedy
  this agent supersedes (kept as the agent-absent fallback).
- `rust/swiftletd/src/lease.rs` (`LEASE_POLL_ATTEMPTS_RESTORE`, v0.4.3) — the
  alive-for-restore poller the agent's `dhclient` finally feeds a lease.

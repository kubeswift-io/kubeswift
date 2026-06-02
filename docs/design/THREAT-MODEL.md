# KubeSwift Threat Model

> **Status:** Living document. Updated per major feature ship.
> **Audience:** KubeSwift operators, security reviewers, AI assistants modifying security-relevant code paths.

---

## Phase 2 Live Migration — UNAUTHENTICATED PLAINTEXT TRANSPORT

**Phase 2 swiftletd live-migration plumbing carries unauthenticated guest state in cleartext on the cluster network. Operators MUST NOT route production traffic through this path. Phase 2 is a swiftletd-extension test surface; Phase 3 adds mTLS for production use.**

**Routing a production VM through Phase 2 is a security incident.** Full guest memory and CPU state, including any in-memory secrets (TLS private keys, application credentials, kernel keyrings, decrypted disk content held in page cache), is exposed in cleartext to anyone with read access to the cluster pod network for the duration of the migration.

### Phase 2 explicit gates

Two gates make Phase 2 unusable by accident in production:

1. **`kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation gate** — swiftletd refuses any `migration-action: send` or `migration-action: receive` on a launcher pod that does not carry this annotation set to the literal string `ack`. The action is rejected at the action-handler entry point with `migration-status: rejected, detail: phase2_plaintext_ack_missing` BEFORE any URL parse or CH dispatch.

2. **No SwiftMigration controller integration in Phase 2.** The Phase 1 `SwiftMigration` CRD ships only `mode: offline`; live mode is Phase 3. Operators cannot trigger the Phase 2 swiftletd handlers via SwiftMigration today — they must hand-roll launcher pod YAML and use `kubectl annotate`. This is intentionally inconvenient.

### Threat surface (Phase 2)

| Threat | Impact | Mitigation |
|---|---|---|
| **Pod-network eavesdropper reads in-flight migration** | Full guest memory + CPU state exposed in cleartext | Operator gate (`unsafe-plaintext: ack` ack annotation), banner in operator-facing docs (`docs/migration/phase-2.md`, `test/migration/manual/README.md`), recommended NetworkPolicy default-deny on test namespace. **Phase 3 mTLS** replaces this entirely. |
| **`migration-target-url` rewrite via `pods/patch` RBAC** | Attacker redirects in-flight migration to attacker-controlled endpoint; reads guest memory | **`SECURITY-S1`** grep-able tag on every annotation-URL-read line in swiftletd. Phase 3 deletes the annotation key entirely; URLs come from the SwiftMigration CR (operator-not-writable). |
| **`migration-listen-url` rewrite via `pods/patch`** | Attacker forces destination CH to bind on attacker-chosen port; race a malicious source | Same as above (S1). |
| **CPU-feature mismatch leaves partial guest memory in destination's page allocator** | Implicit cleanup via `__GFP_ZERO` (Linux frees pages, next allocator zero-fills) | Phase 2 swiftletd writes only sanitized error categories into `migration-status-detail` (no raw stderr). Phase 3 adds controller pre-flight CPU-feature check (mirrors Phase 1's target-node-Ready check). |
| **`--log-file` log-tail used for progress reporting (not done in Phase 2)** | A guest escape could spoof CH log lines, forging migration progress | Phase 2 explicitly chose poll-`info`-API for progress reporting (S4 mitigation). Log-tailing is Phase 3+ work, only added if operator demand surfaces. |

### Phase 3 must-have-before-production-traffic

These are non-negotiable before Phase 3 production migration traffic flows. The Phase 2 design did NOT close them; Phase 3 must:

- **mTLS or equivalent transport authentication** on the migration channel (sidecar pattern with stunnel/socat OR upstream CH first-party support).
- **swiftletd reads URL inputs from the SwiftMigration CR** via kube-rs, NOT from controller-set pod annotations. The five operator-writable migration annotation keys (`migration-action`, `migration-action-id`, `migration-target-url`, `migration-listen-url`, `migration-phase2-unsafe-plaintext`) are DELETED in Phase 3, not repurposed.
- **Controller-level CPU-feature pre-flight check** in the SwiftMigration validating webhook (Phase 2 OQ1).
- **Audit-event schema** for migration phase transitions (target-URL, source-pod, destination-pod, operator identity).
- **`spec.allowVersionSkew` opt-in flag with operator-identity binding** (Phase 2 Decision 3 noted this; Phase 3 implements).

See `docs/design/live-migration-phase-2.md` §8 (security posture), §10.1 (Phase 3 work surface inventory), and `docs/design/live-migration-phase-2-spike.md` Section 9 (security review findings S1–S4) for the full detail.

### Phase 3c — status (shipped behind `--migration-mtls-enabled`)

Phase 3c (PRs #79–#84 + the PR 5 walkthrough) closes the must-haves above.
The mechanisms differ from the original Phase 2 plan in two places — the
stunnel-sidecar architecture made simpler closures possible — so the
actual posture is recorded here:

- **mTLS transport — DONE.** A first-party `stunnel` sidecar owns the
  cross-pod TLS hop; CH/swiftletd speak plaintext to localhost only (no
  CH change, no swiftletd data-path change). **Option B** identity:
  cert-manager issues one leaf per worker node (SAN = nodeName); the peer
  is pinned via `verifyChain` + `checkHost` (chains to the migration CA
  **and** matches the expected peer-node SAN — not bare `verify=2`).
  Cluster-validated: SAN-pinned mutual TLSv1.3, ~0% throughput overhead.
- **S1 (untrusted migration URL) — DONE, via loopback validation.** Under
  secured mode the migration URL is always the local stunnel proxy, so
  swiftletd **rejects any non-loopback `target_url`/`listen_url`** rather
  than reading from the SwiftMigration CR (the equivalent, simpler closure
  the sidecar architecture enables). A tampered remote URL can no longer
  redirect the cleartext CH stream off-box. The annotation keys are NOT
  deleted (the controller is their sole legitimate writer and swiftletd no
  longer trusts their host); the `SECURITY-S1` tags now document the
  loopback mitigation.
- **Plaintext-ack gate — RETIRED on the secured path.** swiftletd bypasses
  the `migration-phase2-unsafe-plaintext: ack` requirement in secured
  mode (the channel is TLS), and the controller **no longer emits** the
  ack on the secured path (Phase 3c cleanup) — an "unsafe-plaintext"
  annotation on a TLS-secured pod is misleading. The annotation key is
  **retained** (not deleted): the plaintext path still uses it as the
  operator's "I acknowledge cleartext" gate. Safe from version skew
  because mTLS only ever runs against the Phase 3c swiftletd that added
  the secured-mode bypass.
- **Audit events — DONE.** An `MTLSChannel` Kubernetes Event names the
  pinned per-node peer identities (`src`/`dst`) at Validating; the
  handshake outcome surfaces via the Completed event or the failure
  event's TLS-related detail.
- **CPU-feature pre-flight check — DEFERRED** (Tracked Follow-up #10;
  procedural mitigation: operator verifies `lscpu` flag uniformity).
- **`spec.allowVersionSkew` — RETIRED, not implemented** (Phase 3b Q3:
  `newDstPod` clone-src structurally prevents version skew).

Operator runbook: `docs/migration/phase-3c.md`.

---

## Other KubeSwift threat surfaces

(Future entries: SwiftKernel artifact-pull integrity, SwiftGPU VFIO peer-binding correctness, snapshot-stager init-container privilege boundary, etc. Not in scope of Phase 2 ship; this file is a living document and entries land as features ship.)

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

---

## Other KubeSwift threat surfaces

(Future entries: SwiftKernel artifact-pull integrity, SwiftGPU VFIO peer-binding correctness, snapshot-stager init-container privilege boundary, etc. Not in scope of Phase 2 ship; this file is a living document and entries land as features ship.)

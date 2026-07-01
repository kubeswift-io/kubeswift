# ORAS Provenance Signing (P2) — Design

Status: **Accepted** — Staff-Architect, 2026-07-01. Sub-phase P2 of the ORAS arc
([`oras-vm-disk-artifacts.md`](oras-vm-disk-artifacts.md) §7, §11, §12). Phased:
**3a** API surface (shipped, PR #300) → **3b** image signing → **3c** controller
wiring + cluster validation.

This note resolves the P2-specific decisions the ADR left open (§12.5 offline
verification, §12.6 registry Referrers compat) so the 3b/3c build is unambiguous.
It is deliberately narrow: the *signature* of a snapshot artifact. SBOM /
attestation referrers are a follow-on (§8).

---

## 1. Goal

Make a pushed oci snapshot artifact **cosign-signed**, with the signature stored
as an **OCI referrer** of the artifact — discoverable via `oras discover` and
verifiable with `cosign verify`. This extends the supply-chain spine (already
keyless-cosign on images/charts/CLI blobs in `release-stable.yaml`) to the
at-rest disk/memory artifact, and is the provenance half of the
confidential-computing alignment (ADR §7): build/capture-time provenance,
distinct from and complementary to SEV-SNP launch-time attestation.

The spike proved the mechanism against a real registry: `oras attach` +
`oras discover` return a referrer; cosign signs the artifact digest. P2
productizes it.

---

## 2. Decisions

### 2.1 Identity model — **key-based, not keyless**

Keyless cosign (Sigstore/Fulcio/Rekor) binds a signature to a short-lived OIDC
identity minted in CI. An **in-cluster capture has no CI OIDC identity** and the
edge target is explicitly **offline / air-gapped** (Rekor/Fulcio are online). So
P2 signs with a **long-lived cosign keypair** the operator provisions:

- `spec.backend.oci.signingKeySecretRef` → a same-namespace Secret holding
  `cosign.key` (the encrypted private key) and `cosign.password`.
- Verification is **offline** against the matching `cosign.pub` — the sovereign
  edge story. No transparency log; `--tlog-upload=false` on sign,
  `--insecure-ignore-tlog` on verify.

Keyless (workload-identity / SPIFFE) is a **future** option (§8), not P2.

### 2.2 Where signing happens — **in the `snapshot-oras` push Job, after push**

The Job already has the artifact digest, the registry, and (via the dst mount)
the network path. Signing there keeps it **one node-pinned Job, one identity
boundary** — no second Job, no controller-side registry access (the controller
stays a Kubernetes actor, never a registry client; ADR §5). The `snapshot-oras`
image gains the `cosign` binary; after a successful push the binary shells out:

```
cosign sign --key <mounted cosign.key> --registry-referrers-mode=oci-1-1 \
  --tlog-upload=false --yes <repository>@<manifestDigest>
```

`--registry-referrers-mode=oci-1-1` stores the signature as an **OCI 1.1
referrer** (not the legacy `sha256-<digest>.sig` tag), so it rides the same
Referrers API the ADR's discovery/provenance story depends on.
`COSIGN_PASSWORD` comes from the Secret's `cosign.password` (env, never a flag or
log). Registry auth reuses the push path (dockerconfigjson at `DOCKER_CONFIG`, or
anonymous).

*Rejected — embedding sigstore-go in the binary:* heavier dep tree, and the
cosign CLI is the spike-validated surface. Shelling out to a pinned cosign is
simpler and auditable.

### 2.3 Failure semantics — **strict**

If `signingKeySecretRef` is set and signing fails, the **snapshot Fails**. An
operator who asked for a signed artifact must never get an unsigned one silently
marked as signed (Design Principle #6). Because signing runs *after* the push,
the pushed (unsigned) artifact may already exist in the registry on a signing
failure — the SwiftSnapshot goes `Failed` and `deletionPolicy` governs cleanup;
the operator retries. (The push itself is idempotent; a retry re-signs.)

### 2.4 Status — `status.oci.signed`

`true` only when the sign step succeeded (shipped in 3a). Surfaced so
`kubectl get swiftsnapshot` and dashboards can see provenance at a glance. No
separate signature-digest field in P2 — the signature is discoverable from the
artifact digest via the Referrers API; adding a pinned signature digest is a
cheap follow-on if operators want it.

### 2.5 Verification story — the **TLS caveat is load-bearing**

Spike finding (carried into the CRD docstring + this doc): cosign's
referrer-mode **verify** path is **HTTPS-only** — `--allow-http-registry` is not
honored on the initial `/v2/` ping for verify / oci-1-1 against a plaintext
registry. Consequences:

- The signature **lands and is discoverable** (`oras discover`) even on a
  plaintext registry — the referrer graph is registry-native.
- `cosign verify` requires a **TLS** registry (every real one — GHCR, Harbor,
  ECR, Zot-with-TLS — is HTTPS; plaintext is the explicit-unsafe in-cluster/test
  path, `spec.backend.oci.insecure`).

So 3c's cluster validation proves the referrer **lands** (via `oras discover`) on
the in-cluster Zot, and proves `cosign verify` against a **TLS-fronted** Zot (a
self-signed cert + `cosign verify --key --insecure-ignore-tlog`). The webhook
does **not** reject `insecure: true` + signing — the signature is still useful
(offline `oras`-level discovery, TLS-fronted later); it is documented, not
blocked.

### 2.6 Key management + security

- The cosign private key is a **Kubernetes Secret in the snapshot's namespace**,
  read only by the node-pinned push Job (which already runs as root to read the
  0600 capture artifacts). Blast radius = that namespace's snapshots.
- **Per-snapshot ref, operator-chosen key.** A pool/schedule can point every
  snapshot at one shared signing key (the common case) or vary it; KubeSwift does
  not manage key rotation — the operator owns the keypair lifecycle (documented).
- No new controller RBAC: the Job reads the Secret via a projected volume the
  controller mounts (same pattern as the dockerconfigjson creds), not via a
  controller Secret read.

---

## 3. API surface (shipped — PR 3a / #300)

`spec.backend.oci.signingKeySecretRef` (`*SecretObjectReference`) +
`status.oci.signed` (`bool`) + `vswiftsnapshot` validation of
`signingKeySecretRef.name`. `make generate` + chart sync.

---

## 4. Phased PRs

- **3a (shipped, #300)** — API/CRD surface + webhook + tests.
- **3b** — `snapshot-oras` image: add the pinned cosign binary; a `--sign-key
  <path>` (+ `COSIGN_PASSWORD` env) flag; sign-after-push in upload mode; strict
  failure. Unit-test the arg/flow; local smoke against a TLS `registry:2` or Zot.
- **3c** — controller: `buildOCIPushJob` mounts the `signingKeySecretRef` Secret
  (`cosign.key`→file, `cosign.password`→env) and passes `--sign-key` when set;
  `handleUploadingOCI` stamps `status.oci.signed` from the push report; strict —
  a signing failure surfaces as a Failed snapshot. Cluster-validate: referrer
  lands (`oras discover`) on the HTTP Zot; `cosign verify` on a TLS-fronted Zot.

---

## 5. Non-goals (P2)

- **Keyless / workload-identity signing** — future; needs an in-cluster OIDC
  (SPIFFE) + online Fulcio/Rekor, which contradicts the offline-edge target.
- **SBOM / attestation referrers** — the spike attached an SBOM via `oras
  attach`; P2 ships the *signature* only. SBOM + SLSA-provenance attestations of
  the disk artifact are a clean follow-on on the same Referrers mechanism.
- **Key lifecycle / rotation** — operator-owned; KubeSwift consumes a keypair.
- **Signing the s3 (Tier C) backend** — s3 objects have no referrer graph;
  signing is an oci-backend property (ADR §9 — the referrer graph is a reason to
  choose oci over s3).
- **Blocking plaintext + signing at admission** — documented caveat, not a
  rejection (§2.5).

---

## 6. Open questions

1. **Pin the cosign binary version** in the `snapshot-oras` image (reproducible +
   CVE-trackable). 3b picks a pinned release.
2. **Signature-digest in status** — add `status.oci.signatureDigest` if operators
   want to pin the exact signature referrer (cheap; deferred until asked).
3. **Registry Referrers-API compat** across GHCR / Harbor / ECR / Zot — the
   referrer graph is load-bearing; 3c validates Zot, the rest is an operator
   matrix (ADR §12.6).

---

*🤖 Generated with [Claude Code](https://claude.com/claude-code)*

# Live Migration (Phase 3c) — mTLS Transport Operator Runbook

Phase 3c puts the live-migration data channel inside a mutually-
authenticated, SAN-pinned TLS tunnel, with **no Cloud Hypervisor change
and no swiftletd data-path change**. A first-party `stunnel` sidecar owns
the cross-pod TLS hop; CH and swiftletd speak plaintext to `localhost`
only.

mTLS is **opt-in, off by default**. With `--migration-mtls-enabled=false`
(the default) the migration path is byte-for-byte the Phase 3a/3b
plaintext behaviour.

---

## 1. Architecture in one picture

```
 source pod (node A)                         destination pod (node B)
 ┌───────────────────────────┐               ┌───────────────────────────┐
 │ CH ──plaintext──► 127.0.0.1│               │127.0.0.1 ──plaintext──► CH │
 │              :6790 (client │   TLS (mTLS)  │ :6790  (server sidecar)    │
 │              stunnel) ─────┼──:6789────────┼──► accept 0.0.0.0:6789     │
 └───────────────────────────┘  SAN-pinned   └───────────────────────────┘
```

- Each node has a cert-manager-issued leaf cert (SAN = nodeName) — **Option B**.
- The source stunnel is the TLS **client**; it pins the destination node's
  SAN via `checkHost`. The destination stunnel is the TLS **server**; it
  pins the source node's SAN. Both chain to the migration CA (`verifyChain`).
- swiftletd is handed `tcp:127.0.0.1:6790` and is unaware of TLS.

---

## 2. Enabling mTLS

mTLS requires **cert-manager** installed cluster-side (same dependency as
the admission webhooks).

```bash
# webhook-enabled cluster (the common case) — enables BOTH webhooks + mTLS,
# avoiding the TFU #16 stranded-webhook trap:
make deploy-with-webhook-and-mtls

# minimal (no admission webhooks) cluster:
make deploy-with-mtls
```

These layer `config/overlays/{webhook-mtls,migration-mtls}`, which add:
- the migration PKI (selfSigned Issuer → CA → CA Issuer) in the system namespace,
- the `kubeswift-migration-stunnel` ConfigMap (server.conf + client.conf),
- `--migration-mtls-enabled=true` on the controller.

The `migrationcert` reconciler then issues one `Certificate` per worker
node (`kubeswift-migration-node-<node>`) into the system namespace. Verify:

```bash
kubectl get certificate -n kubeswift-system | grep migration-node
# kubeswift-migration-node-<nodeA>   True
# kubeswift-migration-node-<nodeB>   True
```

### Prerequisite: the migration-stunnel image must be pullable

The sidecar image `ghcr.io/projectbeskar/kubeswift/migration-stunnel` must
be readable by the cluster. New ghcr packages default to **private** and
**GitHub has no API to change package visibility** — a maintainer must set
it public once (Package settings → Danger Zone → Change visibility), or the
cluster needs an imagePullSecret. Symptom if missed: launcher pods stick at
`1/2` with `ImagePullBackOff` / `401 Unauthorized` on the sidecar.

---

## 3. How a guest becomes mTLS-source-ready

When mTLS is enabled, the SwiftGuest controller injects into **every
migration-eligible** launcher pod (at pod creation):

- an idle stunnel **client** sidecar (`STUNNEL_ROLE=client`) that
  idle-polls for its inputs and stays Running/Ready at rest,
- a downward-API volume surfacing the `migration-dst-ip` / `migration-peer-san`
  annotations the controller stamps at migration time,
- an empty per-guest identity Secret (`<guest>-migration-identity`,
  populated at migration time with the source node's cert),
- `KUBESWIFT_MIGRATION_MTLS=1` on the launcher container (swiftletd's
  secured-mode signal: loopback-URL enforcement + plaintext-ack bypass).

> **Immutable-pod limitation.** The sidecar is added only to **newly
> created** launcher pods. A guest whose pod predates mTLS enablement has
> no sidecar and is **not** mTLS-source-ready — recycle its pod (stop/start)
> to gain one. See §6.

---

## 4. Running a migration

Identical to Phase 3b — `swiftctl migrate` or a `SwiftMigration` CR:

```yaml
apiVersion: migration.kubeswift.io/v1alpha1
kind: SwiftMigration
metadata: {name: move-db, namespace: default}
spec:
  guestRef: {name: db}
  target: {nodeName: worker-2}
  mode: live
  allowIPChange: true        # required on default node-local networking
```

An `MTLSChannel` event records the pinned identities:

```
Normal  MTLSChannel  live-migration channel secured with mTLS;
                     pinned peers src="worker-1" dst="worker-2"
                     (SAN-pinned per-node identities)
```

You can confirm the handshake in the sidecar logs (the source client side):

```
CERT: Host name "worker-2" matched with "worker-2"   # SAN pin verified
Certificate accepted at depth=0: CN=worker-2
TLS connected: new session negotiated
TLSv1.3 ciphersuite: TLS_AES_256_GCM_SHA384 (256-bit encryption)
```

---

## 5. Validated baseline (PR 5 cluster walkthrough)

Kernel-boot guest (4 Gi class), default node-local networking, `miles→boba`:

| Metric | mTLS | Plaintext (Phase 3b) |
|---|---|---|
| `observedDowntime` (cutover window) | **1.80 s** | ~1.8 s |
| `observedTransferDuration` (full send RPC) | **38.48 s** | ~38 s |
| TLS handshake | SAN-pinned mutual TLSv1.3 (`AES_256_GCM`) | — |

**mTLS throughput overhead ≈ 0** (the transfer duration matches the
plaintext baseline), confirming the spike's ~1% finding. The source guest
is **never paused** during pre-copy, so a failed transfer leaves it running
and recoverable (no data loss).

> **Sidecar resources matter.** The stunnel sidecars use **request-only**
> CPU (no limit): TLS encryption is CPU-bound and bursts to line rate. A
> CPU *limit* throttles throughput — an early walkthrough build with a
> `100m` limit dropped a 4 GiB migration to ~7 MB/s (~16× slower) and it
> failed. Do not add a CPU limit to these sidecars.

---

## 6. Troubleshooting

| Symptom | Cause | Action |
|---|---|---|
| Launcher pod stuck `1/2`, sidecar `ImagePullBackOff` / `401` | migration-stunnel ghcr package private | Make the package public (§2) or add an imagePullSecret |
| Migration fails fast, `failureReason=SourceSidecarNotReady` | Source pod has no client sidecar — it predates mTLS enablement **or** is a post-cutover destination pod (server-role sidecar) | Recycle the guest's pod (stop/start); see §3 + the chain-migration note below |
| Migration fails, `failureReason=MigrationIdentityNotReady` | A participating node's per-node identity Secret isn't issued yet | `kubectl get certificate -n kubeswift-system`; wait for cert-manager, or check the migration CA Issuer is Ready |
| Migration fails, `failureReason=ReceiveDisconnect` after retries | Source sidecar never came up (rare timing race), or the dst is unreachable | The controller bounded-retries the send on `connection_refused`; if it still fails, recycle the source pod |
| Migration pathologically slow then fails | A CPU *limit* on the stunnel sidecar (do not add one — §5) | — |

### Chain migrations under mTLS need a pod recycle (known limitation)

After a migration's cutover, the destination pod becomes the guest's
running pod — carrying a **server**-role sidecar. Live-migrating that guest
**again** makes it a source that cannot act as a TLS client. The migration
fails fast with `SourceSidecarNotReady` (the source VM is unharmed).
**Workaround:** recycle the guest's launcher pod (stop/start) between
chained mTLS migrations — the SwiftGuest controller recreates it with a
fresh client-role sidecar. Tracked as a follow-up.

---

## 7. Reverting to plaintext

`make deploy-with-webhook` (or `make deploy`) flips the controller back to
`--migration-mtls-enabled=false`. New launcher pods then have no sidecar;
existing mTLS-equipped pods keep their (now idle) sidecar harmlessly until
recreated. The migration PKI / per-node certs can be left in place or
removed with `kubectl delete -k config/migration-mtls`.

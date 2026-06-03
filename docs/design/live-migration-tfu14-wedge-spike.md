# TFU #14 — Source send-wedge on dst-disappearance — Spike findings

> Spike completed 2026-06-03 on the dev cluster (miles/boba, CH v51.1, image
> sha-2d0c2e9, **mTLS enabled**). Conclusion: **the 10-minute wedge does NOT
> reproduce under the Phase 3c mTLS sidecar — that architecture incidentally
> resolves TFU #14 for the production path.** No swiftletd threading refactor is
> warranted.

## The original finding (Phase 3b PR 2 walkthrough)

When the destination pod disappears mid-send (force-deleted or CH killed), the
source `vm.send-migration` RPC was measured blocking for the full
`timeout_seconds` budget (~10 minutes at the 600s default) before returning.
During that window the source action loop AND the source CH HTTP API were
blocked — the source pod's `migration-status` annotation stayed stale at
`sending` and the guest could not accept a new migration action for up to 10
minutes. The candidate fixes were (a) shorter dst-disappearance detection, (b)
interrupt the in-flight send, or (c) a swiftletd worker-thread refactor.

That measurement was taken on the **Phase 3b plaintext path** (no stunnel
sidecar): the source CH dialed the **destination pod IP directly**.

## What the recon found (why 600s)

- `swift-ch-client` (`api.rs`) talks to the CH API over a `UnixStream` with
  `set_read_timeout`/`set_write_timeout`. CH's `vm.send-migration` is a single
  request→response — CH responds only when the whole migration finishes — so the
  600s read timeout **is the legitimate migration-duration bound** (sized for a
  ~200 GiB VM). It cannot be safely shortened.
- The action loop runs on a `current_thread` tokio runtime, and `send_migration`
  is a synchronous blocking call, so the loop cannot tick (process cancel,
  report status) until the read returns.
- The wedge therefore depends entirely on **how fast the source CH's send fails
  when the destination goes away**. On the plaintext path the source CH waits on
  a dead *remote* TCP connection to the dst pod IP (no RST → kernel retransmit /
  the 600s read timeout).

## The empirical reproduction (mTLS path)

A kernel-boot guest on miles, live migration miles→boba, and the **destination
pod force-deleted at transferProgress=26%** (`kubectl delete pod ... --grace-period=0 --force`).

Source swiftletd log (the smoking gun):

```
07:47:53.604  dispatch_migration_send id=t14-mig:send:1 target=tcp:127.0.0.1:6790
              (dst pod force-deleted at 07:48:06)
cloud-hypervisor:  Migration failed: MigrateSend(Error transferring memory to
              socket: Guest memory error: Connection reset by peer (os error 104))
07:48:06.606  w23_terminal_write_signal_fired id=t14-mig:send:1
```

Observed:

| Signal | Result |
|---|---|
| Source send return | **immediate** on dst-delete (~sub-second; not 600s) — CH got `ECONNRESET (errno 104)` |
| Source `migration-status` | `failed` ~23s after the wedge attempt (i.e. promptly) |
| Source CH API responsiveness | **responsive** — a `vm.info` curl on the CH socket returned the full config (CH not wedged) |
| Migration outcome | `Failed`, reason `PodTerminated`, "destination pod disappeared during StopAndCopy" — controller drove it correctly |
| Source guest | back to `Running` on miles, **unharmed** (live pre-copy never pauses the source) |

## Why mTLS fixes it

Under the Phase 3c sidecar, the source CH dials the **local** stunnel
(`tcp:127.0.0.1:6790`, `stopandcopy_live.go::migrationLocalPlaintextPort`), which
tunnels over TLS to the dst sidecar. When the destination pod (and its stunnel)
is deleted, the source stunnel's connection to the dst breaks and it **resets the
local loopback connection** to the source CH. The source CH gets an immediate
`Connection reset by peer` on a *local* socket and fails fast — instead of
waiting on a dead *remote* TCP connection. The local proxy turns a silent remote
disappearance into a prompt local reset.

## Conclusion & recommendation

- **TFU #14's wedge does not occur on the mTLS path**, which is the
  Phase 3c+ production-recommended path. The sidecar architecture resolves it as
  a side effect. **No swiftletd worker-thread refactor is warranted** — it would
  be a risky change to a load-bearing path to fix a problem the production path
  no longer has.
- **Residual (plaintext path only):** a deployment running the legacy plaintext
  path (`--migration-mtls-enabled=false`, gated behind the
  `kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation) still has the
  remote-connection wedge. This was inferred from the mechanism, not re-validated
  (the spike ran mTLS-on). It is acceptable to leave it: plaintext is the
  explicitly-unsafe, non-recommended path; the controller still drives the
  migration to a terminal state independently; and the source VM is never harmed.
- If a future deployment must run plaintext at scale, the worker-thread refactor
  (option c) + a controller-signalled abort (option b) remain the path — but that
  should be driven by real plaintext demand, not done speculatively.

**Disposition:** TFU #14 marked **RESOLVED (mTLS path) / residual-plaintext-only**.
No code change.

# Phase 3 r4: ntx and sov on `9604992`

Run 2026-09-25, 19:51–19:54 UTC, after Phase 1 r4 passed on each cluster.

| # | Cluster | Verdict |
|---|---|---|
| N3 | ntx | **PASS** |
| `val-n2r` deletes | ntx | **PASS**: gone at 19:51:44, 48 s after the upgrade (details in `phase1-r4.md`) |
| Kernel re-pull | ntx | **PASS**: `field-testing/ft-faas` Pulling 19:51:34 → Ready 19:51:41, under new hashed Jobs, with the old ones deleted |
| S1 | sov | **PASS** (the webhook dry-run is skipped: there is no webhook) |

## N3 (ntx)

At 19:53:11:

```text
cp launcher uid=a5420720-639b-4b4c-883f-ceede8794fb0 restarts=0 phase=Running
cp guest phase=Running rv=31943800
  network: primaryIP=192.168.99.10 podIP=<launcher pod IP> primaryIPScope=Pod ready=true
  conds: GuestRunning=True@2026-09-21T21:59:57Z … EgressReady=True@2026-09-21T22:00:43Z   (every timestamp unchanged)
controller log lines naming the guest since the upgrade: 0
rv after another 45 s: 31943800
```

- **The resourceVersion moved once, as expected.** It was `31535194` from before
  round 1 until this upgrade. The single write is the new controller filling
  `status.network.podIP` and `primaryIPScope` (#676).
- No condition changed, and the resourceVersion then held steady. That is the
  expected one-time backfill, not churn.
- `kubectl get swiftguest -n capi-udn -o wide` now shows `Guest IP`
  (192.168.99.10 / .14), a distinct `Pod IP` per guest, and `IP Scope` `Pod`.

## S1 (sov)

```text
controller-manager:sha-9604992 ready=1/1, 13 controllers started, 0 error lines
VAPs [Deny]: kubeswift-gateway-exec-gate, kubeswift-launcher-sa-gate, kubeswift-launcher-sa-token-secret-gate, kubeswift-launcher-sa-tokenrequest-gate
kubeswift-gateway-console <- kubeswift-system/kubeswift-gateway; the gateway SA can create pods/exec
kubeswift-vm-reader pods/exec rules: 0
kubeswift webhook configurations: 0 (the --dry-run=server check is skipped)
```

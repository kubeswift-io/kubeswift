# Phase 3 r3: ntx and sov on `f260277`

Run 2026-09-25, 11:09 UTC, right after Phase 1 r3 (`phase1-r3.md`).

| # | Cluster | Verdict |
|---|---|---|
| N3 | ntx | **PASS** |
| S1 | sov | **PASS** (the webhook dry-run is skipped: there is no webhook) |

N1 and N2 are not re-run, per the go-ahead.

## N3 (ntx). PASS

At 11:09:28, 1.5 min after the ntx upgrade (11:07:58):

```text
cp launcher uid=a5420720-639b-4b4c-883f-ceede8794fb0 restarts=0 start=2026-09-21T21:59:01Z phase=Running
cp guest phase=Running rv=31535194
  conds GuestRunning=True@2026-09-21T21:59:57Z … EgressReady=True@2026-09-21T22:00:43Z   (identical to the round-1 baseline)
controller log lines naming the guest since 11:07: 0
```

The CAPI worker guest is still `Running` on worker-2, as reported in `phase3-r2.md`.

## S1 (sov). PASS

```text
controller-manager:sha-f260277 ready=1/1, 13 controllers started, 0 error lines
VAPs [Deny]: kubeswift-gateway-exec-gate, kubeswift-launcher-sa-gate, kubeswift-launcher-sa-token-secret-gate, kubeswift-launcher-sa-tokenrequest-gate
kubeswift-gateway-console <- kubeswift-system/kubeswift-gateway
kubectl auth can-i create pods --subresource=exec -A --as=…:kubeswift-gateway -> yes
kubeswift-vm-reader pods/exec rules: 0
kubeswift webhook configurations: 0  (the --dry-run=server check is skipped)
```

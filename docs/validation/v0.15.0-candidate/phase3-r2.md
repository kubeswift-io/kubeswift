# Phase 3 r2: ntx and sov on `7a1a76d`

Run 2026-09-25, 07:21–07:24 UTC, right after Phase 1 r2 passed on each cluster.
Node names are generalised as in `phase0.md`.

| # | Cluster | Verdict |
|---|---|---|
| N3 | ntx | **PASS**: the CAPI control-plane guest is untouched by the upgrade |
| S1 | sov | **PASS** (the webhook dry-run is skipped: there is no webhook) |

N1 and N2 are not re-run, per the go-ahead.

## N3 (ntx). PASS

Taken at 07:24:06, about 2.5 min after the ntx upgrade (07:21:45):

```text
cp launcher uid=a5420720-639b-4b4c-883f-ceede8794fb0 restarts=0 start=2026-09-21T21:59:01Z image=swiftletd:v0.13.14 phase=Running
cp guest phase=Running rv=31535194                  (unchanged since before round 1)
controller log lines naming the guest since 07:21: 0
```

The condition timestamps are identical to the round-1 baseline.

### Two corrections to round 1's `phase3.md`

**1. The CAPI worker guest recovered, as a side effect of my round-1 cordon.**
- `capi-udn/ks-udn-md0-sz2ql-bz9zs`, stuck on the worker-1 Longhorn attach
  since before round 1, is now **Running on worker-2**. Its launcher started at
  2026-09-25T01:02:26Z, with `primaryIP 192.168.99.14`.
- That falls inside the window when I had worker-1 cordoned for the N2 retry
  (01:01:24 → 01:05:18). The relaunch the controller issues every ~5 min
  landed on worker-2, where Longhorn attach works.
- Its CAPI Machine is now `READY True / AVAILABLE True`, and the Cluster `ks-udn`
  went from `Available=False` to `Unknown`. The control-plane Machine is still
  `Ready=Unknown`.
- Nothing about the CP guest changed, but the ntx CAPI baseline is no longer
  the Phase 0 one.

**2. `val-n2r` did not delete.**
- `phase3.md` said it was deleted. It is actually **stuck `Terminating`**, on
  the same pre-existing local-snapshot finalizer defect as dev's
  `val-d2`/`val-d3`:

  ```text
  E … "Reconciler error" "error"="create cleanup pod: pods \"swift-snap-cleanup-snapshot-local-mem\" is forbidden:
  unable to create new content in namespace val-n2r because it is being terminated"
  ```

- These retries are all 11 error lines of the new ntx controller.
- Left as is. `val-n2` (N2 attempt 1) is still `Active`, kept for inspection.

## S1 (sov). PASS

Taken at 07:23:30, right after the upgrade:

```text
controller-manager:sha-7a1a76d 1/1, 13 controllers started, 0 error lines
VAPs [Deny]: kubeswift-gateway-exec-gate, kubeswift-launcher-sa-gate, kubeswift-launcher-sa-token-secret-gate, kubeswift-launcher-sa-tokenrequest-gate
kubeswift-gateway-console <- ServiceAccount:kubeswift-system/kubeswift-gateway
kubectl auth can-i create pods --subresource=exec -A --as=…:kubeswift-gateway  -> yes
kubeswift-vm-reader pods/exec rules: 0; has swiftguests/console create, swiftsandboxes/exec create, swiftsandboxes/log get
(no kubeswift webhook configuration: the --dry-run=server check is skipped)
```

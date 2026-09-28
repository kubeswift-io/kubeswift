# Phase 3 r7: ntx and sov on `8afbad3`

Run 2026-09-28, 08:33–08:35 UTC, after Phase 1 r7 passed on each cluster.

| # | Cluster | Verdict |
|---|---|---|
| N3 | ntx | **PASS** |
| S1 | sov | **PASS** (the webhook dry-run is skipped: there is no webhook) |

## N3 (ntx)

At 08:33:36, and again 45 s later:

```text
cp launcher uid=a5420720-639b-4b4c-883f-ceede8794fb0 restarts=0 phase=Running
cp guest rv=31943800 phase=Running primaryIP=192.168.99.10 scope=Pod
  conds: GuestRunning=True@2026-09-21T21:59:57Z … EgressReady=True@2026-09-21T22:00:43Z   (every timestamp unchanged)
controller log lines naming the guest since the upgrade: 0
rv after 45 s: 31943800
```

The two pre-check 2 leftovers stay `Failed` ("SwiftImage failed: import job
failed"), with no launcher pod.

## S1 (sov)

```text
controller-manager:sha-8afbad3 ready=1/1, 13 controllers started, 0 error lines
VAPs: kubeswift-gateway-exec-gate, kubeswift-launcher-sa-gate, kubeswift-launcher-sa-token-secret-gate, kubeswift-launcher-sa-tokenrequest-gate
kubeswift-gateway-console (ClusterRoleBinding) <- kubeswift-system/kubeswift-gateway
  SubjectAccessReview, gateway SA, create pods/exec: allowed (by ClusterRoleBinding kubeswift-gateway-console)
  SubjectAccessReview, controller SA, list events: allowed (by ClusterRoleBinding kubeswift-controller-manager)
kubeswift-vm-reader pods/exec rules: 0
kubeswift webhook configurations: 0 (the --dry-run=server check is skipped)
```

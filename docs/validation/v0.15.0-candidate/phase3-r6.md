# Phase 3 r6: ntx and sov on `4f08e87`

Run 2026-09-28, 06:30–06:33 UTC, after Phase 1 r6 passed on each cluster.

| # | Cluster | Verdict |
|---|---|---|
| N3 + pre-check 2 leftovers | ntx | **PASS** |
| S1 | sov | **PASS** (the webhook dry-run is skipped: there is no webhook) |

## N3 (ntx)

At 06:31:49, and again 45 s later:

```text
cp launcher uid=a5420720-639b-4b4c-883f-ceede8794fb0 restarts=0 phase=Running
cp guest rv=31943800 phase=Running primaryIP=192.168.99.10 scope=Pod
  conds: GuestRunning=True@2026-09-21T21:59:57Z … EgressReady=True@2026-09-21T22:00:43Z   (every timestamp unchanged)
controller log lines naming the guest since the upgrade: 0
rv after 45 s: 31943800
```

The resourceVersion has not moved since round 4. This upgrade wrote nothing
to the guest.

**The pre-check 2 leftovers,** as recorded in `phase1-r6.md`:
- `default/sample` and `val-n2/snapshot-local-source` are `Failed`, with
  "SwiftImage failed: import job failed". Their images are `Failed`.
- **Neither has a launcher pod.**

## S1 (sov)

```text
controller-manager:sha-4f08e87 ready=1/1, 13 controllers started, 0 error lines
VAPs [Deny]: kubeswift-gateway-exec-gate, kubeswift-launcher-sa-gate, kubeswift-launcher-sa-token-secret-gate, kubeswift-launcher-sa-tokenrequest-gate
kubeswift-gateway-console (ClusterRoleBinding) <- kubeswift-system/kubeswift-gateway
  SubjectAccessReview, gateway SA, create pods/exec: allowed ("allowed by ClusterRoleBinding kubeswift-gateway-console")
kubeswift-vm-reader pods/exec rules: 0
kubeswift webhook configurations: 0 (the --dry-run=server check is skipped)
events RBAC (SubjectAccessReview, controller SA, list events): allowed
```

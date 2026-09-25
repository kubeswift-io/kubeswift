# Phase 3 r5: ntx and sov on `08d0165`

Run 2026-09-25, 23:53–23:56 UTC, after Phase 1 r5 passed on each cluster.

| # | Cluster | Verdict |
|---|---|---|
| N3 + events RBAC | ntx | **PASS** |
| S1 + events RBAC | sov | **PASS** (the webhook dry-run is skipped: there is no webhook) |

## N3 (ntx)

At 23:53:10, and again 45 s later:

```text
cp launcher uid=a5420720-639b-4b4c-883f-ceede8794fb0 restarts=0 phase=Running
cp guest rv=31943800 phase=Running primaryIP=192.168.99.10 scope=Pod ready=true
  conds: GuestRunning=True@2026-09-21T21:59:57Z … EgressReady=True@2026-09-21T22:00:43Z   (every timestamp unchanged)
controller log lines naming the guest since the upgrade: 0
rv after 45 s: 31943800
```

- **The resourceVersion is the one round 4 left (`31943800`).** This upgrade
  wrote nothing to the guest: the one-time `podIP`/`primaryIPScope` backfill
  already happened in round 4.
- **Events RBAC:** `can-i list events` for the controller SA → `yes`. A
  `SubjectAccessReview` agrees (see `phase1-r5.md`).

## S1 (sov)

```text
controller-manager:sha-08d0165 ready=1/1, 13 controllers started, 0 error lines
VAPs [Deny]: kubeswift-gateway-exec-gate, kubeswift-launcher-sa-gate, kubeswift-launcher-sa-token-secret-gate, kubeswift-launcher-sa-tokenrequest-gate
kubeswift-gateway-console (ClusterRoleBinding) <- kubeswift-system/kubeswift-gateway
  SubjectAccessReview, gateway SA, pods/exec: create allowed ("allowed by ClusterRoleBinding kubeswift-gateway-console"), get not allowed
kubeswift-vm-reader pods/exec rules: 0
kubeswift webhook configurations: 0 (the --dry-run=server check is skipped)
events RBAC: can-i list events (controller SA) -> yes
```

**A kubectl quirk, not a product change.** On both sov and dev,
`kubectl auth can-i create pods/exec --as=<gateway SA>` (kubectl client 1.32.3)
printed `no`, and `get pods/exec` printed `yes`. A direct `SubjectAccessReview`
gives the opposite, which is the intended grant: `create` allowed by
`kubeswift-gateway-console`, `get` not.
- The ClusterRole is unchanged: `pods/exec: [create]`.
- I did not chase the client-side cause.
- The SubjectAccessReview is the authoritative answer, so S1 holds as in
  round 4.

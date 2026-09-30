# v0.15.1 round, Phase 3: ntx and sov on `5968241`

Run 2026-09-30, 16:00–16:50 UTC. ntx nodes are generalised `<n1>`–`<n3>`.

| # | Cluster | Verdict |
|---|---|---|
| V1 | ntx | **PASS** (Phase 1): the two `Failed` CAPI guests stop churning. They were still at the same resourceVersion at the end of the round |
| N1 | ntx | **BLOCKED-ENV**: ntx's Longhorn is still down, so no volume can be provisioned |
| N2 | ntx | **BLOCKED-ENV**: the same |
| N4 | ntx | **PASS**: refused by `kubeswift-launcher-sa-tokenrequest-gate` |
| S1 | sov | **PASS** |
| F3 (a), (b) | sov | **PASS**: (a) accepted; (b) refused |

**At the end:**
- **The controllers** are unchanged since the upgrade, with 0 restarts.
  - **ntx:** 16 ERROR lines, all the routine `ft-faas` optimistic-lock
    conflict; 0 panics.
  - **sov:** 0 errors.
- **No `val-151-*` namespace is left** on either cluster.

## V1: churn stops (#696), ntx. PASS

Measured in Phase 1 (`v0151-phase1.md`). Before, on v0.15.0, each CAPI
guest's resourceVersion moved every ~35 s: `EgressReady.lastTransitionTime`
was refreshed each time. After the upgrade, over the 5-minute window:

```text
16:00:26 → 16:05:27, every 30 s: ks-udn-cp-54klw rv=33994334, ks-udn-md0-sz2ql-bz9zs rv=33994335;
         EgressReady=False/LauncherExited@16:00:21 (fixed), PodScheduled=False/PodNotScheduled
end of the round (16:49): still rv=33994334 / 33994335, both Failed
```

The guests were not touched: `capi-udn` is hands-off, and their launchers
(`a5420720-…`, `f598c58f-…`, `Failed`, restarts 1) are unchanged.

## N1, N2: BLOCKED-ENV

ntx's Longhorn has not been repaired since the regression run:

```text
longhorn-manager: one CrashLoopBackOff (216 restarts), one with 217 restarts (Running at the time of sampling)
Longhorn nodes: <n1> Ready=False/ManagerPodDown, <n2> Ready=False/ManagerPodDown, <n3> Ready=True
csi-provisioner: mostly ContainerStatusUnknown, one CrashLoopBackOff
the regression run's N1 PVC (val-reg-smoke/swiftimage-import-ubuntu-noble) is still Pending
```

- **Not run.** Neither scenario can get a volume.
- **Still kept:** `val-reg-smoke` from the regression run, holding its N1
  objects (`SwiftImage ubuntu-noble` `Importing`, `SwiftGuest sample`
  `Pending`, the PVC `Pending`).

## N4: TokenRequest gate (ntx). PASS

```text
can-i create serviceaccounts/token (as val-tok-user): yes;  control token for default: issued
kubectl create token kubeswift-launcher --as=val-tok-user -> forbidden: ValidatingAdmissionPolicy 'kubeswift-launcher-sa-tokenrequest-gate' … denied request:
  tokens for the KubeSwift launcher ServiceAccounts are reserved: …
val-151-tok deleted in 6 s
```

## S1: controller and policies (sov). PASS

```text
controller-manager ready 1/1, sha-5968241, 13 controllers, 0 errors, --metrics-secure=true
VAPs [Deny]: kubeswift-gateway-exec-gate, kubeswift-launcher-sa-gate, kubeswift-launcher-sa-token-secret-gate, kubeswift-launcher-sa-tokenrequest-gate
kubeswift-gateway-console (ClusterRoleBinding) <- kubeswift-system/kubeswift-gateway
  SubjectAccessReview, gateway SA, create pods/exec: allowed (by ClusterRoleBinding kubeswift-gateway-console)
exec gate matches pods/exec, pods/attach, pods/portforward;  kubeswift-vm-reader pods/exec rules: 0
```

## F3 (a), (b) on sov. PASS

```text
SwiftGuestClass (a) storage {storageClassName: longhorn}    -> created (server dry run)
SwiftGuestClass (b) storage {accessMode: ReadWriteMany}      -> invalid: spec.storage: accessMode=ReadWriteMany requires volumeMode=Block; Filesystem RWX is not live-migration-capable
classes created: 0
```

## At the end

| | ntx | sov |
|---|---|---|
| kubeswift pods | `controller-manager` (started 16:00:02), `sha-5968241`, 0 restarts | `controller-manager` (16:05:54), 0 restarts |
| Controller log, new controller only | 848 lines; 16 ERROR, all `Reconciler error … swiftkernels "ft-faas": the object has been modified` (scenario: none; the routine conflict seen in every round); 0 panics; 0 `being terminated` | 77 lines; 0 ERROR; 0 panics |
| `val-151-*` left | none | none |
| Terminating namespaces | none | none |
| Other | `val-reg-smoke` (the regression run's N1, BLOCKED) and `val-n2` (round 1) remain, as before | none |

**Harness note.** My log capture's first attach, to the old v0.15.0 pod,
replayed that pod's whole log. The counts above are from the new
controller's section only.

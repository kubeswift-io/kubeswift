# Regression run, Phase C: ntx and sov on v0.15.0

Run 2026-09-30, 09:34–10:44 UTC, after Phase A (`regression-a.md`). ntx nodes
are generalised `<n1>`–`<n3>`. Pod IPs are redacted.

## Summary

| # | Cluster | Verdict |
|---|---|---|
| N1 | ntx | **BLOCKED-ENV**: ntx's Longhorn is down (see below). The image import PVC never binds, and the smoke times out at "SwiftImage ubuntu-noble did not reach Ready" |
| N2 | ntx | **BLOCKED-ENV**, not run: the same cause, as it needs a new root volume |
| N3 | ntx | **FINDING C1**: the CAPI guests keep their launcher UIDs, and nothing restarted them. But they are `Failed` (A2), and their resourceVersion is **not** steady: `EgressReady.lastTransitionTime` is rewritten every ~33 s |
| N4 | ntx | **PASS**: refused by `kubeswift-launcher-sa-tokenrequest-gate` |
| S1 | sov | **PASS** |
| S2 | sov | **PASS**: refused by `kubeswift-launcher-sa-tokenrequest-gate` |
| S3 | sov | **PASS**: the server dry-run is accepted with no webhook, and nothing is created |

**At the end:**
- **ntx:** the controller pod is the same as in Phase A, with 0 restarts. 40
  ERROR lines, all optimistic-lock conflicts; 0 panics.
- **sov:** the same pod, 0 restarts, 0 ERROR lines.
- **Left:** only ntx's `val-reg-smoke`, kept because N1 is blocked.

**Findings:**
- **C1:** status churn on Failed guests whose evicted launcher pod still
  exists.
- **C2:** ntx's Longhorn is down: longhorn-manager is crash-looping, and 2 of
  3 Longhorn nodes report `ManagerPodDown`. This is environment, and it
  predates the upgrade.

**Timing comparison:** there is nothing to compare. N1 and N2 did not run,
and N4, S1–S3 are admission checks.

## N1: disk-boot smoke on the OVN primary network. BLOCKED-ENV

`NAMESPACE=val-reg-smoke test/smoke/boot-test.sh --scenario disk-boot --no-cleanup`
(09:37:31–09:52:32), from the `v0.15.0` worktree:

```text
--- Scenario: disk-boot (Cloud Hypervisor + Ubuntu Noble) ---
  Using the existing swiftguestclasses.swift.kubeswift.io "default" as it is      (the round-1 leftover class)
  Waiting for SwiftImage ubuntu-noble Ready (timeout 15m)...
  FAIL: SwiftImage ubuntu-noble did not reach Ready
PVC swiftimage-import-ubuntu-noble (storageClass longhorn-r1, the default): Pending for 15 min,
  63 × ExternalProvisioning "Waiting for a volume to be created … by the external provisioner 'driver.longhorn.io'"
import pod: FailedScheduling "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims"
SwiftImage ubuntu-noble: Importing;  SwiftGuest sample: Pending (not Failed: #685 waits for the image)
```

**Cause (C2): Longhorn on ntx cannot provision.**

```text
longhorn-manager-<a> on <n1>: CrashLoopBackOff, 174 restarts, running since 2026-09-29T13:07
longhorn-manager-<b> on <n3>: CrashLoopBackOff, 174 restarts, running since 2026-09-29T12:57
  both: level=fatal msg="Error starting webhooks: admission webhook service is not accessible on cluster after 2m0s …
        timed out waiting for endpoint https://longhorn-admission-webhook.longhorn-system.svc:9502/v1/healthz"
Longhorn nodes: <n1> Ready=False/ManagerPodDown, <n2> Ready=False/ManagerPodDown, <n3> Ready=True
csi-provisioner: most pods ContainerStatusUnknown, one CrashLoopBackOff (108 restarts)
```

- **When it started.** The crash loop began with the 2026-09-29 disk-pressure
  event (`regression-a.md`, A3). It predates the v0.15.0 upgrade.
- **Not retried pinned to worker-2.** The plan's retry covers a worker-1
  attach failure. Here provisioning fails before any pod is scheduled, so
  pinning cannot help.
- **The likely lab fix** is the recorded ntx "stale netns" playbook (restart
  the Longhorn pods left with a stale network namespace). That is William's
  call, and nothing was changed.
- **Kept on ntx** for inspection, as a scenario that did not pass:
  `val-reg-smoke`, holding the SwiftImage `ubuntu-noble` (Importing), the
  SwiftGuest `sample` (Pending) and the pending PVC.
  `make smoke-test-cleanup` was not run.

## N2: in-place restore. BLOCKED-ENV (not run)

`local-roundtrip-test.sh` needs an image import and a root volume. With N1's
PVC unbound for 15 minutes, it would fail the same way.

## N3: CAPI guests after the run. FINDING C1

| Guest | Phase A (09:29) | End (10:42) |
|---|---|---|
| `capi-udn/ks-udn-cp-54klw` | Failed, launcher uid `a5420720-…`, restarts 1, rv `33877463` | Failed, the same uid, restarts 1, rv **`33899133`** |
| `capi-udn/ks-udn-md0-sz2ql-bz9zs` | Failed, launcher uid `f598c58f-…`, restarts 1, rv `33877462` | Failed, the same uid, restarts 1, rv **`33899134`** |

- **Nothing touched them.** They were not relaunched, and they are still
  hands-off (A2).
- **But their resourceVersion keeps moving.** Sampled every 40 s:

```text
10:42:37 rv=33899266  … EgressReady=False@2026-09-30T10:42:36Z   (every other condition timestamp unchanged since 2026-09-29 12:57 or earlier)
10:43:17 rv=33899425  … EgressReady=False@2026-09-30T10:43:10Z
10:43:57 rv=33899584  … EgressReady=False@2026-09-30T10:43:42Z
controller log lines naming the guest in the capture: 0
```

**C1: `EgressReady`'s `lastTransitionTime` is rewritten about every 33 s**,
although its status stays `False`.
- **The cost.** About 110 status writes an hour per guest, and a
  `lastTransitionTime` that does not mean what it says.
- **Only these two guests churn.** The ntx leftovers `default/sample` and
  `val-n2/snapshot-local-source` are also `Failed`, but they have no launcher
  pod, and they do not churn: `default/sample`'s rv has been `33021563` since
  round 6.
- **So the trigger looks like** a `Failed` guest whose (evicted, `Failed`)
  launcher pod still exists.
- **Not seen before.** In earlier rounds these guests were `Running`, and
  their rv did not move (round 7 N3). Whether the dev-candidate controller
  churned the same way on 2026-09-29, before the upgrade, cannot be told.
- **Leftovers:** still `Failed`, with no launcher pod.

## N4: TokenRequest gate. PASS

As round 1's D10, in `val-reg-tok`:

```text
can-i create serviceaccounts/token (as val-tok-user): yes
control, token for default: eyJhbGciOiJSUzI1NiIs…
token for kubeswift-launcher: error: failed to create token: serviceaccounts "kubeswift-launcher" is forbidden:
  ValidatingAdmissionPolicy 'kubeswift-launcher-sa-tokenrequest-gate' with binding 'kubeswift-launcher-sa-tokenrequest-gate'
  denied request: tokens for the KubeSwift launcher ServiceAccounts are reserved: their pods run privileged, …
val-reg-tok deleted in 6 s
```

## S1: controller and policies (sov). PASS

```text
controller-manager ready 1/1, image controller-manager:v0.15.0
VAPs: kubeswift-gateway-exec-gate[Deny] kubeswift-launcher-sa-gate[Deny] kubeswift-launcher-sa-token-secret-gate[Deny] kubeswift-launcher-sa-tokenrequest-gate[Deny]
kubeswift-gateway-console (ClusterRoleBinding) <- kubeswift-system/kubeswift-gateway
  SubjectAccessReview, gateway SA, create pods/exec: allowed (by ClusterRoleBinding kubeswift-gateway-console)
kubeswift-gateway-exec-gate matches CONNECT on pods/exec, pods/attach, pods/portforward
kubeswift-vm-reader pods/exec rules: 0
```

## S2: TokenRequest gate (sov). PASS

The same as N4: refused by `kubeswift-launcher-sa-tokenrequest-gate`, and
`val-reg-tok` was deleted in 9 s.

## S3: admission with no webhook (sov). PASS

```text
kubeswift webhook configurations: 0
kubectl apply -n val-reg-s3 --dry-run=server -f config/samples/disk-boot/swiftguest-sample.yaml (namespace line dropped)
  -> swiftguest.swift.kubeswift.io/sample created (server dry run)
SwiftGuests created: 0 in val-reg-s3, 0 cluster-wide; val-reg-s3 deleted in 9 s
```

The first attempt pointed at a non-existent sample path, which was my error.
The re-run above used `config/samples/disk-boot/swiftguest-sample.yaml`.

## At the end

| | ntx | sov |
|---|---|---|
| kubeswift pods | `controller-manager-…-x7s7m`, the same as Phase A, 0 restarts | `controller-manager-…-whhzd`, the same, 0 restarts |
| Controller log capture | 1 attach (09:28:28), 1312 lines | 1 attach (09:28:29), 77 lines |
| ERROR lines | 40: 39 × `Reconciler error … swiftkernels "ft-faas": the object has been modified`, 1 × the same on `swiftimages "ubuntu-noble"` (N1's). Scenario: none for the first, N1 for the second. Both are routine optimistic-lock conflicts | 0 |
| Panics | 0 | 0 |
| `val-reg-*` left | `val-reg-smoke` (N1, BLOCKED, kept) | none |
| Terminating namespaces | none | none |

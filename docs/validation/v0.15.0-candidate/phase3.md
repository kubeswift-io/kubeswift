# Phase 3: ntx and sov on `d232581`

Run 2026-09-25, 00:12–01:06 UTC, after Phase 1 passed on each cluster (`phase1.md`).
Node hostnames are generalised: ntx `cp-1` / `worker-1` / `worker-2`, as in `phase0.md`.

| # | Cluster | Scenario | Verdict |
|---|---|---|---|
| S1 | sov | controller, VAPs, console grant, `vm-reader` | **PASS** (the webhook dry-run is skipped: the webhook is off) |
| N1 | ntx | disk-boot smoke on the OVN-K primary network | attempt 1 **BLOCKED-ENV** (Longhorn attach on worker-1); retry pinned to worker-2 **PASS** for boot, SSH and cluster egress. See "OVN address" below |
| N2 | ntx | `local-roundtrip-test` | attempt 1 **BLOCKED-ENV** (same attach failure); retry pinned to worker-2 **PASS**. The guest kept its address; the launcher's OVN pod address changed (see below) |
| N3 | ntx | CAPI-managed guest unchanged after the upgrade | **PASS** |

---

## S1: sov (no KVM, no webhook). PASS

Collected at 00:13 UTC:

```text
controller-manager 1/1 ... ghcr.io/kubeswift-io/kubeswift/controller-manager:sha-d232581
controller-manager-757f64b467-rxxn5 1/1 Running 0
Available=True; 13 controllers "Starting workers" + "informer cache synced"; 0 error lines (E....)

NAME                                      FAILURE      (all bound [Deny])
kubeswift-gateway-exec-gate               Fail
kubeswift-launcher-sa-gate                Fail
kubeswift-launcher-sa-token-secret-gate   Fail
kubeswift-launcher-sa-tokenrequest-gate   Fail

(no kubeswift webhook configurations)

clusterrole kubeswift-gateway-console
clusterrolebinding kubeswift-gateway-console <- ServiceAccount:kubeswift-system/kubeswift-gateway
kubectl auth can-i create pods --subresource=exec -A --as=system:serviceaccount:kubeswift-system:kubeswift-gateway  -> yes
vm-reader pods/exec rules: 0
{"apiGroups":["swift.kubeswift.io"],"resources":["swiftguests/console"],"verbs":["create"]}
{"apiGroups":["sandbox.kubeswift.io"],"resources":["swiftsandboxes/exec"],"verbs":["create"]}
{"apiGroups":["sandbox.kubeswift.io"],"resources":["swiftsandboxes/log"],"verbs":["get"]}
```

- The exec gate's `matchConditions` limit it to
  `request.userInfo.username == 'system:serviceaccount:kubeswift-system:kubeswift-gateway'`,
  on `CONNECT` of `pods/exec|attach|portforward`.
- **SKIPPED: `kubectl apply --dry-run=server` of a sample SwiftGuest.** sov runs
  `--webhook-enabled=false` and has no kubeswift webhook configuration, so there
  is no webhook to answer (per the go-ahead).
- **Recording artefact, not a finding.** My first `kubectl auth can-i create
  pods/exec` said `no`. That form parses as resource `pods`, *name* `exec`; the
  subresource form above says `yes`.

---

## N1: disk-boot smoke on the OVN-K primary network

### Attempt 1: BLOCKED-ENV

Command: `test/smoke/boot-test.sh --scenario disk-boot --no-cleanup --timeout-image 30`
in `default`. The smoke samples hard-code `namespace: default`, so the script
cannot run in a `val-*` namespace; see `phase2.md` D1.

- The import pod was scheduled on worker-1. Every pod the Job created (7 in
  total) failed the same way:

  ```text
  FailedAttachVolume  AttachVolume.Attach failed for volume "pvc-876e6c87-…" : rpc error: code = DeadlineExceeded
                      desc = volume pvc-876e6c87-… failed to attach to node <worker-1> with attachmentID csi-46075eca…
  Job swiftimage-import-ubuntu-noble: Failed, BackoffLimitExceeded (failed=7), 00:13:41 -> 00:50:26
  Longhorn volume pvc-876e6c87-…: state=attaching -> detaching, robustness=unknown, owner=<worker-1>
  ```

  This is the same failure as the CAPI worker guest's root volume, which
  predates the candidate (`phase0.md`, heads-up 4).
- **Side observation.** The SwiftImage went `phase: Failed` (`ImportFailed`,
  "import job failed") at 00:18:25, after the first pod failed, while the Job
  kept retrying until 00:50:26. The status says Failed while retries are still
  running.
- Kept for inspection in `default` on ntx:
  - SwiftImage `ubuntu-noble` (Failed) and SwiftGuest `sample` (Failed);
  - SwiftSeedProfile `minimal`, and the cluster-scoped SwiftGuestClass `default`;
  - PVC `swiftimage-import-ubuntu-noble`.

### Retry, pinned to worker-2: PASS (boot, SSH, egress)

The same smoke manifests, in `val-n1`, with the guest's `spec.nodeName: <worker-2>`.

**Worker-1 was also cordoned during the retry, 00:51:41 to 00:55:20.** The
SwiftImage import is a Job with no placement field, so pinning the guest alone
could not keep the import off worker-1. `kubectl get nodes` before and after
the retry showed all three nodes `Ready` and schedulable.

```text
00:52:07 image=Importing  (import pod on worker-2)
00:52:39 image=Ready
00:53:30 guest=Running  node=worker-2
00:54:11 guest=Running  primaryIP=192.168.99.13
conditions: Resolved, StorageReady, PodScheduled, GuestRunning/VmRunning, EgressReady/ClusterServicesReachable, NetworkReady/IPAcquired
```

SSH into the guest from its launcher:

```text
kubeswift-guest
 00:55:23 up 1 min
ens3    inet 192.168.99.13/24 ... dynamic ens3
10.96.0.1       kubernetes.default.svc.cluster.local      <- cluster DNS resolves (egress via OVN)
```

**"Reachable at its OVN address".** This guest is a default-namespace nat guest.
Its own address is the bridge lease `192.168.99.13`. Its launcher's OVN
primary-network address is `<ovn-pod-ip>` (`k8s.ovn.org/pod-networks`
`role: primary`). A busybox pod on cp-1 got the following:

```text
ping <ovn-pod-ip>   -> 2/2 received, 0% loss
nc -z <ovn-pod-ip> 22 -> port22-closed
```

So the OVN address is reachable, but the guest's SSH is not published on it:
the smoke guest declares no exposed ports. The guest was reachable at its
own address. If the plan meant inbound to the guest through the OVN pod
address, this needs a guest with exposed ports, which N1 does not have.
`val-n1` was deleted after the retry.

## N2: `local-roundtrip-test`

### Attempt 1: BLOCKED-ENV

Command: `test/snapshot/local-roundtrip-test.sh --namespace val-n2 --no-cleanup`.

- The import pod again landed on worker-1:

  ```text
  00:59:38 FailedAttachVolume … pvc-fc31818f-… : rpc error: code = DeadlineExceeded … failed to attach to node <worker-1>
  Longhorn volume pvc-fc31818f-…: state=attaching, robustness=unknown, owner=<worker-1>
  ```

- I stopped the script at 01:01 (it was waiting out its 15 min image timeout).
  `val-n2` is kept for inspection: SwiftImage `ubuntu-noble` Importing, and
  SwiftGuest `snapshot-local-source` Failed while waiting for its image.

### Retry, pinned to worker-2: PASS

- **Setup:** in `val-n2r`, with worker-1 cordoned (01:01:24 → 01:05:18). The
  source SwiftGuest was pre-created with `kubectl create`, from the sample's
  own manifest plus `spec.nodeName: <worker-2>`.
- **Command:** the unmodified script,
  `test/snapshot/local-roundtrip-test.sh --namespace val-n2r --no-cleanup`,
  with `KUBESWIFT_TEST_IDENTITY` set to an ephemeral key.
- The script's `kubectl apply` of the sample keeps the pre-created `nodeName`,
  because its manifest does not set that field.

```text
  Captured on node <worker-2> (pause window 631ms)
  OK: launcher pod is in restore-receive mode, no stager (in-place fast path)
  OK: sentinel survived: kubeswift-roundtrip-1790298213-3850
=== Tier B round-trip e2e PASS ===
```

| | Before the restore | After the restore |
|---|---|---|
| Guest `status.network.primaryIP` | `192.168.99.15` | `192.168.99.15` (reported from 01:03:42, before the restore was Ready) |
| Snapshot `status.guestSpec.primaryIP` | `192.168.99.15` | – |
| Snapshot `status.memorySnapshot.handle` | `/var/lib/kubeswift/snapshots/val-n2r_snapshot-local-mem` (#649 format) | – |
| Launcher pod | uid `c568858c-…`, OVN `<ovn-ip-A>` | uid `0280fa55-…` (`restore-receive`), OVN `<ovn-ip-B>` |
| SwiftRestore | – | `startedAt` 01:03:38 → `completedAt` 01:03:46 (**8 s**) |

- **The guest kept its address** (#657). **The launcher's OVN pod address
  changed**, because the in-place restore replaces the launcher pod, and a pod
  on the default network gets a fresh address.
- If "keeps its OVN address" meant the pod address, that does not hold for a
  default-network guest. Holding it would need the UDN/NAD path with an
  IPAMClaim. This is reported as observed, not judged.
- `val-n2r` was deleted. The cluster-scoped SwiftGuestClass `snapshot-local-class`
  (created by the script) is left behind.

## N3: CAPI-managed guest unchanged. PASS

The baseline is from `phase0.md` and `phase1.md`, checked again at 01:05:36:

```text
cp launcher uid=a5420720-639b-4b4c-883f-ceede8794fb0 restarts=0 start=2026-09-21T21:59:01Z node=<cp-1> image=swiftletd:v0.13.14 phase=Running
cp guest phase=Running rv=31535194 gen=1
  conds: GuestRunning=True@2026-09-21T21:59:57Z Resolved=True@2026-09-24T21:42:03Z StorageReady=True@2026-09-24T21:42:03Z
         PodScheduled=True@2026-09-24T21:42:03Z NetworkReady=True@2026-09-21T21:59:57Z PortsProgrammed=True@2026-09-21T21:59:57Z
         EgressReady=True@2026-09-21T22:00:43Z
KubeSwiftMachine ks-udn-cp-54klw Ready=True
controller-manager log lines naming the guest since the upgrade (00:08Z): 0
```

- **The guest is unchanged:** the same launcher UID, 0 restarts, the same
  SwiftGuest `resourceVersion` (31535194, no status write since before the
  upgrade), and identical condition transition times.
- **Unchanged from Phase 0:** the CAPI Cluster `ks-udn` is `Provisioned`,
  `Available=False`, and the worker guest is still stuck on the Longhorn attach
  on worker-1. That state predates the candidate.

## Left on ntx for inspection

These are from the two BLOCKED-ENV first attempts.

- **`default` (N1 attempt 1):** SwiftImage `ubuntu-noble` (Failed), SwiftGuest
  `sample` (Failed), SwiftSeedProfile `minimal`, and PVC
  `swiftimage-import-ubuntu-noble`.
- **`val-n2` (N2 attempt 1):** the whole namespace.
- **Cluster-scoped:** SwiftGuestClasses `default` and `snapshot-local-class`.

All nodes are uncordoned. The cordons I set were released at 00:55:20 and 01:05:18.

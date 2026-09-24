# Lab validation: v0.15.0 candidate

Written by the cloud session driving the release (it cannot reach the lab). It
is run by a Claude session on William's machine, where the kubeconfigs are.

**Channel:** this branch, `validation/v0.15.0-candidate`.
- Write one report per phase under this directory: `phase0.md`, `phase1.md`, …
- Commit only report files and push this branch. Pull before each push.
- Never push to `main`, and never open a PR.
- Stop after each phase and wait: the driving session reads the report and
  pushes the go-ahead for the next phase as a commit to this file (see
  **Go-ahead** at the bottom). `git pull` and check it before continuing.

**Rules:**
- Pass `--kubeconfig <path>` (or set `KUBECONFIG` per command) on every
  command, for the right cluster.
- Do not fix anything. On a failure, collect evidence (below), keep the failed
  resources for inspection, and carry on with the next independent scenario.
- Each scenario gets **PASS / FAIL / SKIPPED (why)**, the commands run, and the
  lines of output that prove the result.
- Evidence on failure:
  - `kubectl describe` of the objects involved, and events in the namespace;
  - controller-manager logs since the scenario started;
  - launcher logs for every guest involved, including `--previous`;
  - `kubectl get <kind> -o yaml` of the SwiftGuest, SwiftSnapshot,
    SwiftRestore or SwiftMigration.
- Use a namespace per scenario, named `val-<scenario>`, and delete it on PASS.

| Cluster | Kubeconfig | Notes |
|---|---|---|
| dev | `/home/wrkode/code/vmm-kubeswift/dev-tests/kubeswift/kubeswift-cluster.yaml` | primary lab, federation hub, GPU node |
| sov | `/home/wrkode/sovhetzner-kubeconfig.yaml` | Hetzner, controller only, no `/dev/kvm` |
| ntx | `/home/wrkode/code/vmm-kubeswift/ntx-cluster.kubeconfig` | OVN-Kubernetes primary CNI, CAPI guests |

What is under test (merged since v0.14.1):

| PR | Change |
|---|---|
| #649 | snapshot node dirs are `<ns>_<name>`, recorded in `status.memorySnapshot.handle`; digest-pinned base images |
| #652 | sharing-aware shared-base eviction (C23); TokenRequest VAP for launcher ServiceAccounts (S8) |
| #653 | swiftletd runs actions off its loop; a migration cancel cannot kill a completed migration |
| #657 | an in-place restore resumes the snapshot (it used to boot the guest cold) and keeps its address |
| #655 | G9: the UI console, sandbox shell and logs no longer use the user's `pods/exec`; `kubeswift-gateway-exec-gate` VAP |
| #656 | G15: `/metrics` is HTTPS and authorized by default (merging as this is written) |

## Phase 0: read-only reconnaissance (change nothing)

1. For each cluster:
   - Kubernetes version.
   - Nodes: which are KVM-capable, and which carry kubeswift labels (e.g. `kubeswift.io/basedisk-node`).
   - The kubeswift Helm release: name, namespace, chart version.
   - `helm get values` for it (user-supplied only; redact secrets).
   - The controller-manager image and readiness.
   - Whether each of these is installed: webhook, gateway, UI, cert-manager, Prometheus Operator, migration mTLS issuer.
   - `federation.role`.
   - ValidatingAdmissionPolicies present, by name.
   - Storage classes: which is the default, which are RWX, whether snapshot-capable (Longhorn?).
   - Installed CRD versions: does `swiftsnapshots` have `status.guestSpec`?
2. **Activity in progress:** SwiftGuests, SwiftSnapshots, SwiftRestores and SwiftMigrations in all namespaces (age, phase), and namespaces created in the last 6 hours. Flag anything that looks like another validation still running. Phase 1 must not start while one is.
3. **dev only:** can it run a live migration (two KVM nodes, an RWX class)? Which storage class suits the snapshot suites?

## Phase 1: upgrade to the candidate (only after the Go-ahead)

The go-ahead names the main commit `<sha>` (7 chars) whose dev build to use.
- Chart: `oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.0.0-dev.<sha>`
- Images: every kubeswift image is tagged `sha-<sha>`.

For each cluster, in the order dev, ntx, sov:
1. **CRDs first.** `helm upgrade` does not update CRDs, and #657 adds `status.guestSpec.primaryIP`; a stale CRD silently drops it. `git checkout <sha>` in your checkout, then `kubectl apply --server-side --force-conflicts -f charts/kubeswift/crds/`.
2. `helm upgrade <release> oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.0.0-dev.<sha> -n <ns> --reuse-values`, with `--set <component>.image.tag=sha-<sha>` for each image the values set:
   - controllerManager
   - swiftletd
   - sandboxMaterialize
   - snapshotORAS
   - migrationStunnel
   - gpuDiscovery
   - dra
   - gateway

   Leave `ui.image.tag` as it is.
3. `kubectl rollout status` for every kubeswift Deployment and DaemonSet.
4. Report the controller image, its `--metrics-secure` flag value (expect `false` under `--reuse-values`; that is by design), and the VAPs now present.

## Phase 2: dev cluster suites (only after the Go-ahead)

Run with `KUBECONFIG` pointing at dev, from the checkout at `<sha>`.

| # | Scenario | How | Pass |
|---|---|---|---|
| D1 | Boot smoke | `make smoke-test` (then `make smoke-test-cleanup`) | every scenario the cluster supports PASS |
| D2 | In-place restore of a running guest (#657) | `make local-roundtrip-test`; key via `KUBESWIFT_TEST_IDENTITY` (see the e2e workflow's "Generate ephemeral SSH keypair" step) | sentinel survives; the post-restore SSH address equals the captured one; SwiftRestore took more than ~5 s (it used to be Ready in 3 s) |
| D3 | Clone restore (Tier B) | `make local-clone-identity-test` | passes (on OVN-K the clone's IP comes from `k8s.ovn.org/pod-networks`) |
| D4 | Snapshot dirs (#649) | inspect D2's SwiftSnapshot before cleanup, or create one: `status.memorySnapshot.handle` = `/var/lib/kubeswift/snapshots/<ns>_<name>`. Create a local SwiftSnapshot with `spec.backend.local.hostPath: /var/lib/kubeswift/snapshots/elsewhere`. | handle as expected; the bad hostPath is refused (admission if the webhook is on, else the snapshot goes Failed before capturing) |
| D5 | Tier A CSI snapshot | `make snapshot-test` and `make clonestrategy-test` | pass |
| D6 | Cross-node TCP | `make b0-cross-node-tcp-test`, then `make b0-cross-node-tcp-test-cleanup` | pass |
| D7 | Live migration (#653) | `test/migration/migration-test.sh` (read its header for arguments) | migration Succeeded; guest reachable; source pod gone |
| D8 | Migration cancel mid-transfer (#653) | give a guest enough memory to make the transfer take several seconds, start a live migration, and cancel while phase is transferring (docs/migration/phase-3a.md, "Cancelling a migration") | SwiftMigration Cancelled; the guest keeps running on the source and is reachable; the destination pod is gone; no VM killed |
| D9 | Cancel racing completion (#653) | repeat D8 but cancel just as the migration completes | either Cancelled with the source running, or Succeeded with the destination running; never neither (no guest left without a running VM) |
| D10 | TokenRequest gate (#652) | as a user bound to `admin` in `val-tok`, try `kubectl create token kubeswift-launcher -n val-tok` (via `--as`) | refused by `kubeswift-launcher-sa-tokenrequest-gate` |
| D11 | Gateway exec gate (#655), policy | the gateway credential is `system:serviceaccount:<ns>:kubeswift-gateway`. With a running guest: `kubectl exec --as=<that SA> <launcher-pod> -c launcher -- sh -c id` | refused by `kubeswift-gateway-exec-gate`; the same exec with the console bridge command from `internal/gateway/exec_bridge.go` (`consoleBridge(ns, guest)`) is admitted (connects; interrupt it) |
| D12 | Console through the UI (#655) | a user with the Console capability opens a guest console; a user without it tries | the first works; the second gets 403 `cannot create swiftguests/console`. Also run the CHANGELOG `jq` role migration and report which Access-editor roles it changed |
| D13 | Secure metrics (#656), dev only | `helm upgrade ... --reuse-values --set controllerManager.metrics.secure=true`, then from a debug pod: plain `http://<pod-ip>:8080/metrics`; `https` with an unbound SA token; `https` after binding that SA via `controllerManager.metrics.readers` | plain HTTP fails; unbound token gets 403; bound token gets 200 with `kubeswift_` series; a Prometheus ServiceMonitor, if present, shows the target up once its SA is a reader |

## Phase 3: ntx and sov (only after the Go-ahead)

| # | Cluster | Scenario | Pass |
|---|---|---|---|
| N1 | ntx | `make smoke-test` (disk-boot) on the OVN-K primary network | pass; guest reachable at its OVN address |
| N2 | ntx | `make local-roundtrip-test` | sentinel survives; the guest keeps its OVN address after the restore |
| N3 | ntx | a CAPI-managed guest still reconciles after the upgrade (no restart, no status churn) | unchanged |
| S1 | sov | controller Ready; webhooks answering (`kubectl apply --dry-run=server` of a sample SwiftGuest); VAPs present | pass |

## Go-ahead

Phase 0: **DONE** (`phase0.md`). Thanks — the heads-ups are all taken below.

**Candidate: main @ `2d146eb`** (#649, #652, #653, #655, #656, #657). William has
confirmed the v0.14.1 validation session is finished.

Phase 1: **GO**.
Phase 2: **GO** once Phase 1 has succeeded on dev.
Phase 3: **GO** per cluster, once Phase 1 has succeeded on that cluster.
Push `phase1.md`, `phase2.md` and `phase3.md` as each finishes; do not wait for
another go-ahead between them.

**Stop conditions:**
- If the upgrade or rollout fails on a cluster, collect the evidence, run
  nothing more on that cluster, and continue with the others.
- If a scenario leaves a guest without a running VM (lost, killed, or stuck with
  no launcher), stop Phase 2 there and report at once.

### Amendments from Phase 0 (these override the tables above)

**Phase 1:**
- **Image published?** Before step 1, check the build exists:
  - `helm show chart oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.0.0-dev.2d146eb`
  - the image `ghcr.io/kubeswift-io/kubeswift/controller-manager:sha-2d146eb`

  The Release Dev run for `2d146eb` was still publishing at 22:58 UTC. If either
  is missing, re-check every 5 minutes for up to 45 minutes, then report and stop.
- **Also set `snapshotS3.image.tag=sha-2d146eb`** (heads-up 2): all nine
  pinned tags, `ui` excepted.
- **CRDs first** on every cluster, from a checkout at `2d146eb` (heads-up 3).
  Then confirm `swiftsnapshots` has `status.guestSpec.primaryIP`.
- **After each cluster's upgrade, record:**
  - the Helm revision;
  - every kubeswift image;
  - the controller's `--metrics-secure` value;
  - the VAPs present. Expected: `kubeswift-launcher-sa-tokenrequest-gate`
    everywhere; `kubeswift-gateway-exec-gate` on dev and sov, not ntx;
  - on dev and sov: that ClusterRole `kubeswift-gateway-console` exists, and that
    `kubeswift-vm-reader` (sov) no longer grants `pods/exec`.
- **Running guests are not disturbed.** Record the launcher pod UID and
  restart count of every running guest before and after:
  - dev: `gpu-cells/innercp` (`10bd28b8-…`);
  - ntx: `capi-udn/ks-udn-cp-54klw` (`a5420720-…`).

  The controller upgrade must not recreate them.

**Phase 2 (dev):**
- **D5.** Run the scripts directly with `--vsclass longhorn-snapshot-vsc`
  (`test/snapshot/snapshot-test.sh --vsclass …`, and the same for
  `test/clonestrategy/clonestrategy-test.sh` if it accepts the flag; if it does
  not, SKIPPED with the reason).
- **D7 is replaced.** Do not run `test/migration/migration-test.sh`: it is an
  offline test with the defects you listed, and they will be fixed separately.
  - Instead, create a live SwiftMigration by hand for a guest of class
    `small-migratable` (`longhorn-migratable`, RWX Block), from worker-1 to
    worker-2 (`spec.mode: live`; see `docs/migration/phase-3a.md` for the spec).
  - **Before:** plant a tmpfs sentinel in the guest (as the round-trip test does)
    and record `/proc/uptime`.
  - **Pass:** phase **`Completed`** (not `Succeeded`); the sentinel is still
    there; uptime kept counting (no reboot); the guest is reachable; the source
    pod is gone; `status.podRef` names the destination pod.
- **D8, cancel mid-transfer.**
  - Setup: a cluster-scoped SwiftGuestClass `val-migratable-16g`, a copy of
    `small-migratable` with 16Gi memory (delete it afterwards). Make memory busy
    in the guest, e.g. fill a tmpfs with random data and keep rewriting it, so
    the transfer lasts long enough to cancel.
  - Start a live migration, and while it is transferring set
    `spec.cancelRequested: true` (phase-3a, "Cancelling a migration").
  - **Pass:** the SwiftMigration ends Cancelled; on the source, the sentinel is
    present, uptime is continuous, and the guest is reachable; the destination
    pod is gone.
  - Also report the phase and phaseDetail sequence you observed, and the
    destination launcher's swiftletd log lines around the cancel.
- **D9, cancel racing completion.** Same setup; set `cancelRequested` as close
  as you can to the source reporting complete.
  - **Pass:** exactly one VM survives: either Cancelled with the source running,
    or Completed with the destination running. The sentinel is present and
    uptime continuous in whichever runs.
  - Try it three times, and report the timing of each attempt.
- **D11.** Take the bridge command verbatim from `consoleBridge()` in
  `internal/gateway/exec_bridge.go` @ `2d146eb`, with the guest's namespace and
  name substituted. The gateway credential on dev is
  `system:serviceaccount:kubeswift-system:kubeswift-gateway`.
- **D12 is split.**
  - The browser half is William's: an OIDC login with and without the Console
    capability. List the exact steps for him in the report.
  - Your half:
    - run the CHANGELOG `jq` role migration as a **dry run** first, printing each
      role that would change and its before/after rules;
    - then apply it on dev;
    - report which roles changed, and that a new role saved in the Access editor
      carries `swiftguests/console` rather than `pods/exec` (check an existing
      editor-created role's rules via kubectl if you can't use the UI).
- **D13 on dev.**
  - Find the ServiceAccount the kube-prometheus-stack Prometheus runs as, in the
    `monitoring` release.
  - Then `helm upgrade … --reuse-values --set controllerManager.metrics.secure=true`
    with `controllerManager.metrics.readers` set to that SA.
  - Check:
    - plain HTTP fails;
    - an unbound SA token gets 403;
    - a bound one gets 200 with `kubeswift_` series;
    - the Prometheus target `kubeswift-controller-manager` is `up` (via its
      `/api/v1/targets`).
  - Leave dev in this state (it is the new default); report the final values.

**Phase 3:**
- **ntx:**
  - The default class is `longhorn-r1`, and worker-1's Longhorn has an attach
    problem that predates everything here (heads-up 4).
  - If N1 or N2 fails on a Longhorn attach (`FailedMount`, volume `attaching`),
    mark it **BLOCKED-ENV** with the evidence. Retry once with the guest pinned
    to worker-2 (`spec.nodeName`).
  - N3 uses the baseline above: same launcher UID, 0 restarts.
- **sov** (no KVM, no webhook):
  - S1 is controller Ready; the VAPs as expected; the sov-side console grant and
    exec gate present; `kubeswift-vm-reader` without `pods/exec`.
  - Its webhook is off, so skip the `--dry-run=server` check and say so.

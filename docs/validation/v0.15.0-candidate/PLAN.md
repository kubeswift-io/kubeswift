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

Phase 0: **GO** — run it now.
Phase 1: **WAIT**.
Phase 2: **WAIT**.
Phase 3: **WAIT**.

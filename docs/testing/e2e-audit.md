# End-to-End Test Audit

> Audience: KubeSwift maintainers. Inventory of every cluster-side
> e2e script, what it covers, and what runs it. Drives "what gets
> into CI" decisions.

## Why this exists

The Tier A SwiftRestore data-loss bug fixed in PR #21 had been latent
since the controller's first commit (`4e055a6`). The unit test suite
passed because the fake-client unit test didn't exercise the
SwiftGuest controller's orphan-delete race against the SwiftRestore-
created PVC. The e2e test that **would** have caught the bug
(`test/snapshot/snapshot-test.sh`, lines 184–196 assert the
`restore-seeded` label and the `dataSource` on the per-guest PVC)
existed in the repo but was never invoked by CI.

That pattern — e2es exist, never run, bugs accumulate that the e2e
would catch — is the underlying issue. This audit lists every e2e
and exactly how it gets exercised.

## Inventory

| Script | Purpose | Make target | CI invocation | Bug coverage notes |
|---|---|---|---|---|
| `test/smoke/boot-test.sh` | End-to-end VM boot in five scenarios (disk-boot, kernel-boot, qemu-boot, gpu-alloc, multi-nic) | `make smoke-test`, `make smoke-test-cleanup` | `e2e-on-cluster.yaml` (manual + nightly) | Sole "does the cluster boot a VM" test. Catches integration breakage in image import, CH spawn, networking. |
| `test/snapshot/snapshot-test.sh` | Tier A (csi-volume-snapshot) snapshot+restore. Asserts the restored PVC carries `restore-seeded` label and `dataSource: VolumeSnapshot`. | `make snapshot-test` | `e2e-on-cluster.yaml` (manual + nightly) | **Would have caught the Tier A data-loss bug fixed in PR #21** (the orphan-delete race). |
| `test/snapshot/local-roundtrip-test.sh` | Tier B (local hostPath) memory snapshot + in-place restore. tmpfs sentinel survives the kill+restore cycle. | `make local-roundtrip-test` | `e2e-on-cluster.yaml` (manual + nightly) | Validates memory-state preservation: the headline Phase 2 promise. |
| `test/snapshot/local-clone-identity-test.sh` | Tier B clone restore. Asserts the documented identity-collision behavior (machine-id, hostname, SSH host keys, guest-side MAC all match source). | `make local-clone-identity-test` | `e2e-on-cluster.yaml` (manual + nightly) | Pins the resume-vs-boot limitation. If a future change accidentally fixed the collision (e.g. by triggering a guest reboot post-restore), this test would surface it for re-evaluation. |
| `test/clonestrategy/clonestrategy-test.sh` | `cloneStrategy: snapshot` SwiftImage path: status.cloneSeed populated, per-guest PVC clone via dataSource. | `make clonestrategy-test` | `e2e-on-cluster.yaml` (manual + nightly) | Validates the Phase 1 fast-clone path (independent of SwiftRestore). |

## CI coverage strategy

Two complementary jobs:

### `ci.yaml` — `e2e-scripts` job (every PR)

`make verify-e2e-scripts` runs `bash -n` against every script in `test/`. Catches typos, unclosed quotes, missing-EOF heredocs — anything that prevents the script from running. Cheap (no cluster), runs on every PR. Designed so e2es never silently rot between cluster runs.

### `e2e-on-cluster.yaml` — cluster-side (manual + nightly)

A new workflow that:

1. Creates a kind cluster with snapshot CRDs + `csi-driver-host-path` (snapshot-capable reference CSI).
2. Builds and loads KubeSwift images into kind.
3. Deploys CRDs + controller-manager.
4. Runs the selected e2e scripts.
5. Dumps cluster state on failure (events, controller logs, launcher logs per guest).

**Triggers today:** `workflow_dispatch` (manual) and `schedule` (nightly at 04:00 UTC).

**Not yet wired:** `pull_request` path-touch on `internal/controller/{swiftrestore,swiftguest,swiftsnapshot}/**` and `rust/**`. Stabilizing the manual + nightly path first; add PR triggers once the workflow has a track record. The data-loss-class bug PR #21 fixed would be caught by the nightly run within 24 hours of merge — better than indefinite latency, worse than per-PR. Closing the gap is tracked.

## Adding a new e2e

1. Drop the script in `test/<area>/<name>-test.sh`. Make it idempotent and self-cleanup-friendly (`--no-cleanup` flag if it fits).
2. Add a Make target to `Makefile` (look for the existing snapshot-test / clonestrategy-test entries to copy from).
3. Add a row to the inventory table above with what it covers and the bug class it would have caught had it existed earlier.
4. Add an entry to the `e2e-on-cluster.yaml` `scenario` choice list and the corresponding `case` branch in the run step.
5. Verify locally: `make verify-e2e-scripts` (must pass), then run against a real cluster (the GHA workflow_dispatch is the lowest-friction way once the workflow is merged).

## Non-goals

- **kubeswift-tester / dedicated test cluster.** Out of scope for now. The kind-based e2e is good enough for catching regressions; production-shape testing is a separate exercise (real CSI drivers, multi-node networking, GPU hardware) that benefits from a different runner topology.
- **End-to-end performance benchmarks.** The walkthrough captures pause-window-style timings as observations; a benchmarking suite would be a separate workflow.

## History

- PR #21: Tier A restore data-loss fix. Surfaced during operator walkthrough Scenario 1.
- PR #22 (this PR): wires snapshot e2e into CI; introduces `e2e-on-cluster.yaml`; adds Make targets for every existing e2e; adds `verify-e2e-scripts` to the per-PR fast-feedback loop.

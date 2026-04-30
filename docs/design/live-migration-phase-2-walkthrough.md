# Live Migration Phase 2 — Cluster Walkthrough Log

> **Date:** 2026-04-29
> **Cluster:** k0s 1.34.3, frida (CP) + miles (worker) + boba (worker), Longhorn 24d (RWO only)
> **Images deployed:** `controller-manager:sha-6fa2394`, `swiftletd:sha-6fa2394`
> **Audience:** PR #29 (Phase 2 PR-C) post-merge validation; Phase 3 implementer pre-flight reading
> **Scope:** First post-merge run of `make migration-phase2-manual`. Three findings surfaced (W3, W4, W6); two architectural fixes shipped pre-walkthrough (W3, W4); one design contradiction surfaced mid-walkthrough (W6).

---

## TL;DR

What was validated end-to-end:
- The PR #30 RBAC auto-bind landed cleanly. Per-namespace `swiftletd-reporter` RoleBinding auto-created on first SwiftGuest reconcile in a fresh `mig-walkthrough` namespace; zero manual operator RBAC steps.
- swiftletd's lease poller wrote `kubeswift.io/guest-ip` annotation; SwiftGuest's `status.network.primaryIP` populated correctly. Verifies the W4 retry-on-failure fix isn't masking a different symptom.
- Source SwiftGuest (Ubuntu Noble, 2 vCPU / 2 GiB / 20 GiB root PVC) reached Running on `miles` with `primaryIP=10.244.125.13` in ~118 s from apply.
- Pre-migration sentinel written via the serial console: `SPIKE-PHASE2-WT-1777503996` → md5 `88d94a051ea2db180606535a4125784d`.
- The Phase 2 manual demo orchestration scripts (`source.sh`, `destination.sh`) ran correctly up to the destination pod's PVC-attach step.

What did NOT validate end-to-end:
- The actual cross-node migration. The destination launcher pod hit `FailedAttachVolume: Multi-Attach error for volume ... Volume is already used by pod(s) mig-source`. The wire-protocol exchange never started; the receive-migration call was never issued.
- Timing measurements (vCPU pause window, BEACON gap, observed downtime) — not collected, since the migration didn't run.
- Sentinel md5 survival post-migration — not verified, since the migration didn't run.

What surfaced as a finding:
- **W6 — disk handoff design contradiction.** PR-C's design doc §5.1.2 said "RWO is required" and "RWX is rejected" for the destination-receive pod template. In practice on RWO storage, the destination pod can't attach the source PVC simultaneously. RWX or Phase 3's RWO-handoff choreography (live-migration.md Constraint 4) is the actual requirement; Phase 2's manual demo cannot complete on RWO without Phase 3 controller logic.

---

## Pre-walkthrough fixes (PR #30 + follow-up)

Before the walkthrough could even start a SwiftGuest in the new namespace, two latent bugs surfaced and required architectural fixes. Both are documented in detail in [`kubeswift_context.md`](../../kubeswift_context.md) Phase 2 walkthrough findings section.

### W3 — Per-namespace RoleBinding required manual application (latent re-surface of snapshot F2)

First attempt: `kubectl create namespace mig-walkthrough` + apply SwiftImage + SwiftGuest. Result: SwiftGuest never reached Running with `primaryIP`. Controller logs showed:

```
swiftletd::action] action_loop_get_pod_err: ApiError: pods "mig-source" is forbidden:
  User "system:serviceaccount:mig-walkthrough:default" cannot get resource "pods"
  in API group "" in the namespace "mig-walkthrough": Forbidden
```

The same finding had been documented as snapshot walkthrough F2 six days prior with disposition "fix-in-walkthrough-PR" rather than architectural fix — operators were expected to `kubectl apply -k config/rbac -n <ns>` and then `kubectl patch rolebinding swiftletd-reporter` after every namespace creation. The manual step was missed in the Phase 2 walkthrough, exactly mirroring the snapshot walkthrough's symptom.

**Fix shipped in PR #30:** promoted the `swiftletd-reporter` Role to a cluster-scoped ClusterRole (`kubeswift-swiftletd-reporter`); SwiftGuest controller calls `EnsureSwiftletdRBAC` at the top of every Reconcile to idempotently create a per-namespace RoleBinding bound to the namespace's `default` ServiceAccount. Operators no longer apply per-namespace RBAC manually.

### W4 — Lease poller exited permanently after first patch failure

Compounded W3: even when the RBAC was applied later in the pod's lifetime, the lease poller had already terminated. `rust/swiftletd/src/lease.rs::spawn_lease_poller` had an unconditional `return` after the first `patch_pod_annotation` attempt regardless of result.

**Fix shipped in PR #30:** only `return` on patch SUCCESS. Transient errors (kube client unavailable, RBAC gap, apiserver flap) leave the poller alive for retry on the next 2 s tick, bounded by the existing 4-minute MAX_ATTEMPTS cap.

### Follow-up fix shipped on `main` (commit `e794471`)

PR #30's grant of just `get,create` on `rolebindings` was insufficient — controller-runtime's default cached client requires `list,watch` to populate the informer cache for any object type the controller reads via `Get`. Without them, every Reconcile in a workload namespace logged:

```
runtime.go:221] Failed to watch *v1.RoleBinding: rolebindings.rbac.authorization.k8s.io
  is forbidden: User "system:serviceaccount:kubeswift-system:controller-manager"
  cannot list resource "rolebindings" in API group "rbac.authorization.k8s.io"
  at the cluster scope
```

The cache layer never synced, so `EnsureSwiftletdRBAC`'s `Get` blocked indefinitely; SwiftGuest pods never got created. Fixed by extending the grant to `get, list, watch, create` in both the kustomize-based install and the Helm chart, then patching the live ClusterRole + restarting the controller.

After the live patch, the walkthrough proceeded:
- SwiftGuest `mig-walkthrough/mig-source` reached `phase=Running` with `primaryIP=10.244.125.13` after ~118 s.
- RoleBinding `mig-walkthrough/swiftletd-reporter` auto-created with correct `subjects: [{kind: ServiceAccount, name: default, namespace: mig-walkthrough}]`.
- Pod annotations: `kubeswift.io/guest-ip=10.244.125.13`, `guest-runtime-pid=44`, `guest-serial-socket=/var/lib/kubeswift/run/mig-walkthrough-mig-source/serial.sock`, `guest-hypervisor=cloud-hypervisor`.

These confirm the W3 + W4 fixes work end-to-end: zero manual RBAC steps, IP discovery survived the kube-client / RBAC race window.

---

## Sentinel write (pre-migration)

Sentinel written via the serial console using `socat` from inside the launcher container. Login credentials per the seed profile (`kubeswift` / `kubeswift`):

```
$ echo SPIKE-PHASE2-WT-1777503996 | sudo tee /root/sentinel.txt && sudo md5sum /root/sentinel.txt
SPIKE-PHASE2-WT-1777503996
88d94a051ea2db180606535a4125784d  /root/sentinel.txt
```

Pre-migration md5: **`88d94a051ea2db180606535a4125784d`**.

---

## Migration attempt

```bash
make migration-phase2-manual SWIFTGUEST=mig-source NAMESPACE=mig-walkthrough TARGET_NODE=boba
```

`source.sh` succeeded — captured source pod metadata (root PVC `swiftguest-root-mig-source`, intent CM `mig-source-runtime-intent`, no seed CM, no data disk) and source pod YAML. State written to `/tmp/kubeswift-migration-phase2-manual/state.env`.

`destination.sh` succeeded at template rendering and `kubectl apply` — the dst launcher pod `mig-source-launcher-mig-recv` was created on `boba` with the `migration-role: receiver` annotation, `KUBESWIFT_MIGRATION_ROLE=receiver` env var, and the same volume mounts as the source pod (matching the design doc §5.1's "same PVC at the same path" model).

Then the dst pod hung in `Init:0/1` for 120+ seconds. Inspection revealed:

```
Events:
  Warning  FailedAttachVolume  attachdetach-controller
    Multi-Attach error for volume "pvc-9a46752c-6b7e-449f-9fda-5202dd2a168c"
    Volume is already used by pod(s) mig-source
```

The source pod's network-init container was waiting on the kubelet to attach the root PVC, which Longhorn's RWO access mode refuses while the source pod still has it mounted on `miles`. The destination pod can never reach Ready in this configuration, so `destination.sh` timed out at the `kubectl wait pod ... --for=condition=Ready` step. `run.sh` was never invoked.

---

## Finding W6 — disk handoff design contradiction

### What happened

PR-C's design doc [`live-migration-phase-2.md`](live-migration-phase-2.md) §5.1.2 states:

> The Phase 2 destination-receive pod template assumes RWO PVC access mode... RWX volumes are NOT supported in Phase 2 because of the F2 source-auto-resume behaviour... With RWO storage, PVC attachment serializes between source and destination — the source cannot be both attached and resumed while the destination is also attaching.

This text accidentally conflates two different Phase 2 risks:

1. **Split-brain (F2 + RWX hazard, design intent):** with shared-write access, source CH and destination CH could both be Running against the same disk simultaneously after an F2 dst-kill cancel. RWO blocks the destination from attaching, which removes the split-brain risk — **but only because it also blocks the destination from running at all in a live-migration setup**.

2. **Live-migration disk handoff (the real Phase 2 requirement):** Cloud Hypervisor's `vm.send-migration` does NOT transfer disk contents (per [`live-migration.md`](live-migration.md) Constraint 4). Both source and destination CH must see the SAME disk data through their respective filesystem paths during the migration. With RWO, this requires either:
   - **RWX storage** (CephFS, NFS, Longhorn-RWX) — both pods mount the same PVC simultaneously; CH coordinates writes via its migration protocol.
   - **Phase 3 RWO-handoff controller** — detach from source, attach to destination during the brief stop-and-copy window (live-migration.md Constraint 4 "RWO with hand-off"). Adds storage detach+attach latency to downtime.
   - **CSI VolumeSnapshot clone-to-dst-PVC** — destination gets a writable snapshot of the source disk via the same shared-storage mechanism the snapshot Phase 2 already uses.

PR-C's manual demo implements **none** of these three options: `destination.sh` just copies the source pod's volume mounts (referencing the source PVC by name) and applies the result, hitting Multi-Attach on RWO.

The Phase 2 spike (`live-migration-phase-2-spike.md`) Q1 validated the wire protocol on a **kernel-boot guest with no PVC at all** — initramfs-only, root filesystem inside the migrated memory state. That setup avoided the disk-handoff problem entirely. The Q1 result is real (the wire protocol works) but it doesn't generalize to disk-boot guests on RWO storage.

### Disposition

- The §5.1.2 text in `live-migration-phase-2.md` is misleading. RWO is necessary (no split-brain) but not sufficient (no live-migration handoff). The correct statement: **Phase 2's manual demo requires either RWX storage OR a kernel-boot guest with no PVC.** The full Phase 3 controller implementation will add the RWO-handoff choreography to make RWO disk-boot guests work cross-node.
- The manual demo as written cannot complete end-to-end on the available cluster (Longhorn-RWO only). The wire-protocol-only spike covered this scenario in Q1; the Phase 2 manual demo doesn't add new wire-protocol evidence beyond the spike.
- This finding is the second time a Phase 2 design assumption broke when leaving the spike's controlled setup — the first being W3/W4 (fixed) which only manifested on multi-namespace clusters that the spike didn't exercise. **Pattern:** the spike's empirical findings under-constrain the Phase 2 design when they validate a simplified scenario but the design generalizes to a broader scenario without re-validation.

### Workarounds for completing a Phase 2 manual demo on this cluster

To actually exercise the wire-protocol + receiver-mode launch branch on this hardware, three follow-up paths are realistic:

1. **Kernel-boot guest** matching the spike's initramfs setup. swiftletd already supports kernel boot (via `kernelRef`); the manual demo would need a `dst-launcher-pod.yaml.template` variant for the kernel-boot case (no PVC mount). This recreates the spike's setup but on the real swiftletd binary. **Lowest cost; demonstrates wire protocol + W3/W4 fixes + receiver-mode swiftletd.**
2. **Provision RWX storage** (e.g., Longhorn RWX feature, CephFS, NFS provisioner) and configure the SwiftImage + cloneStrategy to produce an RWX PVC. **Higher cost (storage stack changes); demonstrates the live-migration intent.**
3. **Wait for Phase 3 controller's RWO-handoff** before attempting another live-migration demo on this cluster. Phase 3 controller integration is the natural home for the storage choreography anyway.

Recommendation: option 1 if a Phase 2 wire-protocol demo on the deployed swiftletd is needed before Phase 3; otherwise defer to Phase 3 controller integration.

---

## What the walkthrough actually validated

Despite W6 blocking the migration step, the walkthrough did successfully validate the post-PR-C swiftletd surface for the parts that don't depend on disk handoff:

| Capability | Status |
|---|---|
| PR #30 RBAC auto-bind across fresh namespaces | **PASS** — RoleBinding appeared, IP populated, no manual operator steps |
| PR #30 lease poller retry-on-failure | **PASS** — kept retrying through the controller's RBAC-cache-startup delay; eventually patched |
| `make migration-phase2-manual` orchestration scripts (source.sh, destination.sh up to apply) | **PASS** — metadata extraction + dst pod template rendering worked correctly |
| swiftletd image deployed (`sha-6fa2394`) carries PR-A + PR-B + PR-C + lease retry fix | **CONFIRMED** via image tag |
| Receiver-mode launch branch (run_ch_receive) | **NOT EXERCISED** — dst pod never reached Ready due to W6 |
| Cross-node send-migration wire protocol | **NOT EXERCISED** — never attempted post-W6 |
| Sentinel md5 survival across migration | **NOT EXERCISED** — never attempted post-W6 |

---

## Walkthrough verdict

The walkthrough resolved W3 and W4 (architecturally — both fixes shipped pre-walkthrough). It surfaced W6 (design contradiction in §5.1.2 of `live-migration-phase-2.md`). It did NOT capture timing measurements or end-to-end migration validation; that work is blocked on either the kernel-boot variant (option 1 above) or Phase 3 controller integration (option 3).

The same observation as the W5 process pattern from `kubeswift_context.md`: spike findings validated under simplified conditions can hide design contradictions that only surface under more realistic conditions. The Phase 2 spike used kernel-boot/initramfs guests; the operator-walkthrough used disk-boot guests. The disk handoff design gap was invisible until the broader scenario ran.

---

## Cleanup

The walkthrough namespace `mig-walkthrough` and the cluster-scoped `default` SwiftGuestClass remain in place as of this log; cleanup is the operator's responsibility (one of the Phase 3 controller's responsibilities will be to GC these alongside SwiftMigration).

To clean up manually:

```bash
kubectl delete sg mig-source -n mig-walkthrough
kubectl delete si ubuntu-noble -n mig-walkthrough
kubectl delete ssp minimal -n mig-walkthrough
kubectl delete sgc default
kubectl delete namespace mig-walkthrough
```

The auto-created `mig-walkthrough/swiftletd-reporter` RoleBinding will be garbage-collected by Kubernetes when the namespace is deleted.

---

## Cross-references

- [`docs/design/live-migration-phase-2.md`](live-migration-phase-2.md) — Phase 2 design doc (§5.1.2 contains the W6 contradiction)
- [`docs/design/live-migration-phase-2-spike.md`](live-migration-phase-2-spike.md) — Phase 2 spike findings (Q1 used kernel-boot, didn't exercise disk handoff)
- [`docs/design/live-migration.md`](live-migration.md) — overall live-migration design (Constraint 4 documents the RWX vs RWO-handoff choice)
- [`kubeswift_context.md`](../../kubeswift_context.md) — Phase 2 walkthrough findings section (W3, W4, W5, W6)
- PRs: #27 (PR-A foundations), #28 (PR-B action loop), #29 (PR-C receiver mode), #30 (W3+W4 RBAC auto-bind), commit `e794471` (rolebindings list/watch follow-up)

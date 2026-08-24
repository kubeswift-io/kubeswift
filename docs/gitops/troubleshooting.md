# GitOps troubleshooting

## Platform and CRDs

| Symptom | Likely cause | Fix |
|---|---|---|
| `kubeswift-infra` Kustomization fails with `no matches for kind "SwiftGuestClass"` | CRDs not installed yet — `dependsOn` mis-wired or HelmRelease not Ready | `flux get helmreleases`; fix the dependsOn chain (platform → infra → workloads) |
| New CR field "doesn't work" after a chart upgrade (silently ignored) | **Stale CRD** — the release upgraded the controllers but not the CRDs | Ensure `upgrade.crds: CreateReplace`; verify with `kubectl explain <kind>.spec.<field>` |
| CR create fails `connection refused ... :9443` | Webhook configurations exist but the controller runs webhook-disabled (cluster-state drift) | align `values.webhook.enabled` with the installed VWC/MWC, or `kubectl delete -k config/webhook` |
| HelmRelease fails on `monitoring.coreos.com` kinds | `monitoring.*` enabled without the Prometheus Operator CRDs present | install kube-prometheus-stack first, or leave monitoring off |
| Platform jumped a minor version unattended | `OCIRepository` `ref.semver` is a wide range such as `">=0.1.0"` | pin to a minor range (`"0.13.x"`) or an exact version |
| Web console is old after a chart upgrade | `ui.image.tag` is not chart-derived; `kubeswift-ui` releases separately | raise `ui.image.tag` deliberately; a chart bump never moves it |

## Admission and RBAC

| Symptom | Likely cause | Fix |
|---|---|---|
| Pods in a guest namespace rejected for naming a launcher ServiceAccount | `launcherSAGate` doing its job — only the controller may create launcher pods | expected; do not disable to work around it. Author a SwiftGuest instead of a raw Pod |
| The gate appears to do nothing on an older cluster | ValidatingAdmissionPolicy is GA in k8s 1.30+; below that the chart **silently skips** it | treat the namespace as the trust boundary, or upgrade the cluster |
| Launcher lost access right after a values change | `scopedLauncherRBAC.enabled` flipped to `true`, which **deletes** the shared namespace-wide binding | intended and one-way; confirm the per-pod Role/RoleBinding exist for the pod |
| SwiftGuest rejected for a hostPath it used to be allowed | `swiftGuest.allowedHostPathPrefixes` is empty by default and denies all | add the prefix explicitly, knowing it grants node-root to guest authors; requires `webhook.enabled: true` |

## Images, kernels and disks

| Symptom | Likely cause | Fix |
|---|---|---|
| Guests stuck `Pending` for minutes after bootstrap | SwiftImage still importing (normal) | `kubectl get swiftimages` — wait for `Ready`; guests proceed automatically |
| Infra Kustomization stuck `Ready=False` waiting forever | `wait: true` blocking on async image import | set `wait: false` on the infra Kustomization |
| Edit to a Ready SwiftImage rejected ("spec is immutable") | Image specs are immutable post-import by design | add a NEW SwiftImage with a new name; repoint guests |
| Edit to `cloneStrategy` or `importStorageClassName` rejected while the image is still importing | those two freeze as soon as the import leaves `Pending`, earlier than the whole-spec rule | delete and recreate the SwiftImage; you cannot move a started import to another class |
| A new kernel tag in Git never lands on the nodes | SwiftKernel re-pull is keyed on **(name, node)**, not on `spec.ociRef.image` — the pull Job already exists | give the new kernel a new SwiftKernel name, or delete `swiftkernel-pull-<name>-<node>` per node |
| Provisioning is copy-speed when `cloneStrategy: snapshot` was requested | volume-mode mismatch between the import PVC and the resolved guest storage; the controller downgrades to copy rather than build an unbootable disk | check controller logs; align the guest class volume mode with the image, or accept the copy path |

## Workloads

| Symptom | Likely cause | Fix |
|---|---|---|
| A guest you edited with `kubectl` keeps reverting | That's GitOps drift correction | make the change in Git (or remove the resource from Git management) |
| Migration "fights" Flux — guest bounces between nodes | `spec.nodeName` pinned in the Git manifest while the migration controller rewrites it | remove `nodeName` from the Git-managed spec |
| A sandbox runs over and over on the reconcile interval | A `SwiftSandbox` with `spec.ttl` is committed to Git: it reaches a terminal phase, the TTL deletes it, Flux recreates it | do not Git-manage SwiftSandbox. Commit a `SwiftSandboxPool` and create sandboxes imperatively or from a job |
| A committed sandbox shows `Completed` and never runs again | Expected: a SwiftSandbox runs once. Re-running means deleting it, which Git cannot express | same as above — sandboxes are not desired state |
| A snapshot or migration re-fires after it succeeded | Same loop, with `SwiftSnapshot` / `SwiftMigration` / `SwiftRestore` | commit a `SwiftSnapshotSchedule` instead; keep one-shot verbs out of Git |
| Pool replica count flaps every few minutes | Both Git and an autoscaler write the `scale` field (`spec.replicas`, or `spec.minWarm` on a sandbox pool) | pick one owner: scale from Git, or drop the field from the manifest |
| Warm GPU pool never reaches `minWarm` | Each warm slot holds a whole GPU; `minWarm` exceeds the free GPUs on the cluster | lower `minWarm` to at most the number of free GPUs |
| Whole fleet deleted after a merge | `prune: true` + manifests removed | restore the manifests in Git (guests recreate; root disks were pruned with the guests) — protect fleets with review rules and `prune: false` for stateful guests |

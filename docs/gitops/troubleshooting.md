# GitOps troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `kubeswift-infra` Kustomization fails with `no matches for kind "SwiftGuestClass"` | CRDs not installed yet — `dependsOn` mis-wired or HelmRelease not Ready | `flux get helmreleases`; fix the dependsOn chain (platform → infra → workloads) |
| New CR field "doesn't work" after a chart upgrade (silently ignored) | **Stale CRD** — HelmRelease upgraded the controllers but not the CRDs | Ensure `upgrade.crds: CreateReplace`; verify with `kubectl explain <kind>.spec.<field>` |
| Guests stuck `Pending` for minutes after bootstrap | SwiftImage still importing (normal) | `kubectl get swiftimages` — wait for `Ready`; guests proceed automatically |
| Infra Kustomization stuck `Ready=False` waiting forever | `wait: true` blocking on async image import | set `wait: false` on the infra Kustomization |
| Edit to a Ready SwiftImage rejected ("spec is immutable") | Image specs are immutable post-import by design | add a NEW SwiftImage with a new name; repoint guests |
| A guest you edited with `kubectl` keeps reverting | That's GitOps drift correction | make the change in Git (or remove the resource from Git management) |
| Migration "fights" Flux — guest bounces between nodes | `spec.nodeName` pinned in the Git manifest while the migration controller rewrites it | remove `nodeName` from the Git-managed spec |
| CR create fails `connection refused ... :9443` | Webhook configurations exist but the controller runs webhook-disabled (cluster-state drift) | align `values.webhook.enabled` with the installed VWC/MWC, or `kubectl delete -k config/webhook` |
| Whole fleet deleted after a merge | `prune: true` + manifests removed | restore the manifests in Git (guests recreate; root disks were pruned with the guests) — protect fleets with review rules and `prune: false` for stateful guests |

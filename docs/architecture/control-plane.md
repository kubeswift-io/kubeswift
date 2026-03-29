# Control Plane

The control plane runs in the **controller-manager** Deployment (`kubeswift-system`). It reconciles SwiftImage, SwiftGuest, and SwiftKernel and optionally serves admission webhooks.

## Controllers

### SwiftImage controller

| Action | Behavior |
|--------|----------|
| HTTP source | Creates Import Job to download image into PVC |
| PVC clone source | Clones or references existing PVC |
| Status | Sets `phase` (Importing → Ready/Failed), `preparedArtifact.pvcRef` |

[SwiftImage API](../api/swiftimage.md)

### SwiftGuest controller

| Step | Behavior |
|------|----------|
| Resolve | Fetches SwiftGuestClass + either SwiftImage (disk boot) or SwiftKernel (kernel boot) + optional SwiftSeedProfile; fails if refs missing or not Ready |
| Seed | Renders NoCloud user-data/meta-data/network-config from SwiftSeedProfile into ConfigMap `<guest>-seed` (disk boot only) |
| Intent | Builds runtime-intent JSON into ConfigMap `<guest>-runtime-intent`. Includes `kernelBoot` field for kernel boot or `rootDisk` for disk boot. |
| Pod (disk boot) | Creates pod with root-disk PVC, seed volume (optional), intent volume; launcher = swiftletd |
| Pod (kernel boot) | Creates pod with kernel-artifacts hostPath volume, intent volume, nodeSelector `kubeswift.io/kernel-node=true`; no PVC, no seed, no network-init |
| Status | Maps pod phase → SwiftGuest phase; swiftletd reports `GuestRunning` |

[SwiftGuest API](../api/swiftguest.md) · [Reconcile flow](../swiftguest-reconcile.md)

### SwiftKernel controller

| Step | Behavior |
|------|----------|
| List nodes | Lists nodes with label `kubeswift.io/kernel-node=true` |
| No nodes | Sets phase=Pending, condition Ready=False reason=NoKernelNodes; waits for node watch |
| Per-node pull | For each labeled node: checks pull Job status. If not started, creates pull Job with ORAS image targeting that node. |
| Phase | All nodes Ready → phase=Ready. Any node Failed → phase=Failed. Otherwise → phase=Pulling. |
| Node watch | Watches Node objects for label changes on `kubeswift.io/kernel-node`; enqueues all SwiftKernels when a node gets the label |

[SwiftKernel API](../api/swiftkernel.md)

## Admission webhooks (optional)

With `--webhook-enabled=true` and cert-manager:

- **Validating** — Required refs, runPolicy enum
- **Mutating** — Defaults

Without webhooks, create/update succeeds; validation happens at reconcile time.

## Deployment

- **Namespace:** `kubeswift-system`
- **Manifests:** `config/manager/`, `config/default/`
- **Webhook overlay:** `config/overlays/webhook/` (requires cert-manager)

[Install](../install/)

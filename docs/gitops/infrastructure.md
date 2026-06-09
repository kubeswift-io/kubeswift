# Layer 2 — infrastructure resources

`infrastructure/kubeswift/` holds the cluster's shared VM building blocks:
classes, images, seed profiles, GPU profiles (and NADs if you use multi-NIC /
multi-node L2). It is shared across environments by default — environment
differences belong in the workloads layer or in per-cluster patches.

Working examples: [`examples/gitops-flux/infrastructure/kubeswift/`](../../examples/gitops-flux/infrastructure/kubeswift/).

## SwiftImage lifecycle nuances under GitOps

- **Imports are slow and asynchronous** (download → qcow2-to-raw → resize).
  Keep `wait: false` on the infra Kustomization so workloads aren't blocked;
  guests referencing a not-yet-Ready image wait in `Pending`.
- **A Ready SwiftImage's spec is immutable** (webhook-enforced). To roll a new
  image version, add a NEW SwiftImage (e.g. `ubuntu-noble-2026-06`) and point
  guests/pools at it — don't edit the URL of a Ready image in Git; the webhook
  will reject the update and the Kustomization will show it.
  Metadata-only edits (labels/annotations) on Ready images are fine.
- **Pruning an image** deletes its prepared PVC (and with
  `cloneStrategy: snapshot`, its clone-seed). Ensure no guest references it;
  guests keep running but cannot be recreated from a pruned image.

## Classes and GPU profiles

SwiftGuestClass and SwiftGPUProfile are small and safe to manage in Git —
changes only affect newly created/recreated guests. The example ships a
`default` class and a `gpu-pcie` class with `coreScheduling: vm` (the
multi-tenant SMT mitigation) plus a Tier-1 `pcie-single` GPU profile.

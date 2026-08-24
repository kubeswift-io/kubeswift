# GitOps for KubeSwift

- [overview.md](overview.md) — the three-layer model, what does *not* belong in Git, trade-offs
- [quickstart.md](quickstart.md) — Flux install, bootstrap, and the platform layer
- [infrastructure.md](infrastructure.md) — Layer 2: classes, images, kernels, seeds, GPU profiles
- [workloads.md](workloads.md) — Layer 3: guests, pools, sandbox pools, snapshot schedules
- [oci-artifacts.md](oci-artifacts.md) — the registry as the artifact store: golden disks, kernels and snapshots alongside the chart
- [secrets.md](secrets.md) — keeping seed user-data credentials out of Git (SOPS)
- [troubleshooting.md](troubleshooting.md) — common failure modes

Reference repository: [`examples/gitops-flux/`](../../examples/gitops-flux/) —
every snippet in these docs comes from there.

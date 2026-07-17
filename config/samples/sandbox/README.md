# SwiftSandbox samples

Ephemeral OCI-rootfs microVMs. A `SwiftSandbox` runs an OCI image as the VM root
filesystem (read-only base + tmpfs overlay), boots in a second or two, and cleans
itself up.

- `swiftkernel-sandbox.yaml` — the `sandbox` kernel profile (pulled per node).
- `swiftkernel-gpu-sandbox.yaml` — the module-capable `gpu-sandbox` kernel profile
  (required for GPU sandboxes, either backend).
- `swiftsandbox.yaml` — a restricted shell sandbox and a network-isolated one.
- `swiftsandbox-verified.yaml` — a cosign-signature-verified sandbox
  (`verifyKeySecretRef`), plus a warm pool of verified slots.
- `swiftsandbox-virtiofs.yaml` — rootfs delivered over virtio-fs
  (`rootfsMode: virtiofs`) instead of the default block disk.
- `swiftsandbox-scratch-disk.yaml` — a secondary block disk
  (`spec.scratchDisk`): a sandbox-owned `blank` PVC and an existing-PVC
  (`pvcRef`) variant. See [`docs/sandbox/scratch-disks.md`](../../../docs/sandbox/scratch-disks.md).
- `swiftsandbox-gpu.yaml` — a GPU sandbox via the **DRA** backend
  (`gpuResourceClaim`) + its `ResourceClaimTemplate`.
- `swiftsandbox-gpu-native.yaml` — a GPU sandbox via the **native SwiftGPU**
  backend (`gpuProfileRef`) — no DRA driver needed.
- `swiftsandboxpool.yaml` — a warm pool of plain (non-GPU) slots.
- `swiftsandbox-pooled.yaml` — a sandbox that checks out from
  `swiftsandboxpool.yaml`.
- `swiftsandboxpool-gpu.yaml` — a warm **GPU** pool (every slot holds a GPU) +
  a sandbox that checks one out.
- `swiftsandboxpool-gpu-model.yaml` — a warm GPU pool with a preloaded model
  (`spec.model`) + a checkout that starts inference with no cold model load.

See [`docs/sandbox/gpu-sandboxes.md`](../../../docs/sandbox/gpu-sandboxes.md) and
[`docs/sandbox/warm-pool.md`](../../../docs/sandbox/warm-pool.md) for the GPU and
warm-pool samples' operator guides.

Prereqs:
- A node labeled `kubeswift.io/kernel-node=true`.
- The `sandbox` SwiftKernel `Ready` (its OCI artifact pulled to the node).

```
kubectl apply -f swiftkernel-sandbox.yaml
kubectl apply -f swiftsandbox.yaml
kubectl get sbox -w
```

Notes:
- `network.mode`:
  - `restricted` (default) — deny-ingress (nothing reaches the sandbox) **and**
    hardened egress: the guest reaches DNS + the public internet but **cannot** reach
    cluster-internal pods/services or the cloud metadata endpoint
    (`169.254.169.254`). The posture for untrusted code. Enforced by in-pod iptables
    on the VM's forwarded traffic (not a NetworkPolicy — that would also cut
    swiftletd's own status reporting).
  - `open` — deny-ingress but unrestricted egress (guest can reach the whole cluster
    + internet). Opt-in for trusted workloads that must talk to in-cluster services.
  - `none` — no network at all (detonation / pure compute).
- **DNS**: a networked guest resolves cluster service names (`kubernetes`, `svc.ns`,
  FQDNs) *and* external names — the controller injects the namespace search domains +
  `ndots:5` (matching a Kubernetes pod's resolver; `ip=dhcp` alone captures the
  nameserver but not the search list). `restricted` still blocks *connecting* to
  cluster IPs — names resolve, the egress rules decide reachability.
- `spec.command` / `spec.args` / `spec.env` / `spec.workingDir` follow k8s/OCI
  semantics (command overrides the image ENTRYPOINT, args the CMD, env is merged
  over the image env), delivered to the guest on a per-sandbox read-only config
  disk — never the kernel cmdline, so env stays out of `/proc/cmdline` and the
  host's `ps`/logs. A bare image runs its own entrypoint.
- **Exit status**: the workload runs as a supervised child, so its real exit code
  is surfaced as `status.exitCode` — a non-zero exit terminates the sandbox `Failed`,
  zero terminates it `Completed`.
- **Interacting** with a running sandbox:
  - `swiftctl sandbox logs <name>` (`-f` to follow) streams the workload console.
  - `swiftctl sandbox exec <name> -- cmd args...` runs a command inside the sandbox's
    OCI rootfs over a host↔guest vsock channel (an in-guest agent). stdout/stderr
    stream back live (no output cap) and the exit code is propagated. `-e KEY=VALUE`
    (repeatable) and `-w DIR` set environment variables and the working directory;
    `-i` forwards stdin (e.g. `exec -i <name> -- sh < script.sh`) and `-t` allocates
    an interactive TTY.
  - `swiftctl sandbox attach <name>` opens an interactive shell (shorthand for
    `exec -it -- /bin/sh`; pass `-- <cmd>` to run something else). Window resizes
    propagate; exit with Ctrl-D or `exit`. (The guest `tty` command may print
    "not a tty" — a chroot/devpts cosmetic quirk; the session is a real TTY, so
    shells, editors, and `top` behave interactively.)
- `spec.timeout` force-terminates a runaway sandbox; `spec.ttl` deletes the
  finished record (and frees the node rootfs cache reference) after it elapses.

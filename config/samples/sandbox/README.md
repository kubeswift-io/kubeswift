# SwiftSandbox samples

Ephemeral OCI-rootfs microVMs. A `SwiftSandbox` runs an OCI image as the VM root
filesystem (read-only base + tmpfs overlay), boots in a second or two, and cleans
itself up.

- `swiftkernel-sandbox.yaml` — the `sandbox` kernel profile (pulled per node).
- `swiftsandbox.yaml` — a restricted shell sandbox and a network-isolated one.

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
- `spec.command` / `spec.args` / `spec.env` / `spec.workingDir` follow k8s/OCI
  semantics (command overrides the image ENTRYPOINT, args the CMD, env is merged
  over the image env), delivered to the guest on a per-sandbox read-only config
  disk — never the kernel cmdline, so env stays out of `/proc/cmdline` and the
  host's `ps`/logs. A bare image runs its own entrypoint. (`workingDir` is
  best-effort in v1: honored when the image has a shell, else the workload starts
  in `/`.)
- `spec.timeout` force-terminates a runaway sandbox; `spec.ttl` deletes the
  finished record (and frees the node rootfs cache reference) after it elapses.

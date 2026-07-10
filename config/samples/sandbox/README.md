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
- `network.mode: restricted` (default) attaches the pod network with a
  deny-ingress NetworkPolicy; `none` attaches no network.
- `spec.command` overrides the image entrypoint (single path in v1; a bare image
  runs its own entrypoint).
- `spec.timeout` force-terminates a runaway sandbox; `spec.ttl` deletes the
  finished record (and frees the node rootfs cache reference) after it elapses.

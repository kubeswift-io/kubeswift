# Running sandboxes

KubeSwift runs ephemeral, strongly-isolated microVMs that boot an **OCI image
as the VM root filesystem** — a `SwiftSandbox`. This page is the operator
entry point; the sample manifests are linked inline.

> **Status: cluster-validated end-to-end (2026-07-11).** An alpine microVM
> boots, runs its workload to a terminal `Completed`/`Failed` phase, and
> supports interactive exec/attach over vsock — validated on both `restricted`
> and `open` network modes. First ships in **v0.9.0**.

## What it is

SwiftSandbox is a third boot mode alongside SwiftGuest's disk boot and kernel
boot: a direct-kernel boot + a read-only ext4 built from an OCI image + a
tmpfs overlay + a bridge-initramfs that execs the image's entrypoint (the
Firecracker/Kata model — not SwiftGuest's qcow2-disk pipeline). A SwiftSandbox
is ephemeral: it runs the workload to completion and holds no PVC. Once
terminal, it stays around for inspection until deleted (or `spec.ttl` cleans
it up).

## When to use it

- CI runners — a clean, disposable VM per job
- AI-agent / code-interpreter execution — a real kernel boundary around
  generated or untrusted code, not just a container
- Serverless / short-lived compute
- Untrusted code and security research

For bursts of same-image sandboxes where the ~15s cold boot dominates, a
[warm pool](warm-pool.md) keeps pre-booted slots ready for sub-second checkout.

## Prerequisites

- A node labeled `kubeswift.io/kernel-node=true`
- A `Ready` `SwiftKernel` named `sandbox` (OCI artifact
  `ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.13`, pulled per node)

The sandbox kernel is not a plain `kernelRef` SwiftGuest kernel — its
bridge-initramfs needs the OCI rootfs disk that the SwiftSandbox controller
supplies at launch, so it only boots as a SwiftSandbox.

## Quickstart

```bash
kubectl apply -f config/samples/sandbox/swiftkernel-sandbox.yaml
kubectl get swiftkernel sandbox -w        # wait for Ready

kubectl apply -f config/samples/sandbox/swiftsandbox.yaml
kubectl get sbox -w                       # Pending -> Materializing -> Running -> Completed
```

Ready-to-edit manifests and notes:
[`config/samples/sandbox/`](../../config/samples/sandbox/).

## CRD field reference

### Spec

| Field | Type | Default | Description |
|---|---|---|---|
| `image` | string | — | OCI image to run as the root filesystem. Required. A digest reference (`repo@sha256:...`) is preferred for reproducibility; a tag is accepted. |
| `imagePullSecret` | string | — | docker-registry Secret (same namespace) for pulling `image` from a private registry. |
| `verifyKeySecretRef.name` | string | — | Secret (same namespace) holding a cosign public key at key `cosign.pub`. When set, the image is cosign-verified before it materializes; an unsigned or tampered image fails and never boots. See [Signed images](#signed-images-verify-before-boot). |
| `cpu` | int32 | `1` | vCPU count. |
| `memory` | Quantity | `512Mi` | Guest RAM. |
| `command` | []string | — | Overrides the image's ENTRYPOINT. Empty uses the image's own Entrypoint+Cmd. |
| `args` | []string | — | Appended to `command` (or the image's CMD when `command` is empty). |
| `env` | []EnvVar | — | Merged over the image's config env. |
| `workingDir` | string | — | Overrides the image's working directory. Honored on both the cold-boot path (the bridge runs the workload via the guest agent's chroot+chdir+exec) and the warm-pool checkout path. Must exist in the image. |
| `timeout` | Duration | none | Wall-clock run cap. Past `startedAt + timeout` the controller force-terminates the sandbox to `Failed` (`DeadlineExceeded`). |
| `ttl` | Duration | none | Once the sandbox has been terminal (`Completed`/`Failed`) for at least `ttl`, the controller deletes it and frees the node's rootfs-cache reference. |
| `rootfsMode` | enum | `block` | How the OCI rootfs is delivered: `block` (read-only ext4 disk) or `virtiofs` (the unpacked tree over virtio-fs, tag `sandboxroot` — skips `mkfs.ext4` and the ext4 size floor, shares the host page cache). Same RO-base + writable tmpfs-overlay either way. |
| `network.mode` | enum | `restricted` | `restricted`, `open`, or `none`. See [Network modes](#network-modes). |
| `network.ports[]` | list | — | Guest ports exposed on the launcher pod: `name` (IANA service name) and `port` (TCP). See [Exposing ports](#exposing-ports). |
| `network.ingress.from[]` | list | any source | NetworkPolicy peers allowed to reach `network.ports`. |
| `readinessProbe` / `livenessProbe` | Probe | — | `httpGet` (HTTP) or `tcpSocket` against the guest. See [Workload probes](#workload-probes). |
| `podMetadata.labels` / `podMetadata.annotations` | map | — | Added to the launcher pod; `*kubeswift.io` keys and pod-network annotations refused. |
| `network.egress.allow[]` | list | — | Destinations a `restricted` sandbox may reach: `service: {name, namespace}` or `cidr`, with optional `ports: [{port, protocol}]`. See [Allowing specific destinations](#allowing-specific-destinations-under-restricted). |
| `kernelProfileRef.name` | string | `sandbox` | SwiftKernel to boot. |
| `nodeSelector` | map[string]string | — | Additional node constraints, merged with the required `kubeswift.io/kernel-node=true`. |
| `poolRef.name` | string | — | Check out a pre-booted slot from this `SwiftSandboxPool` (sub-second) instead of the cold materialize+boot path; falls back to cold on a miss. Same namespace. See [Warm pools](warm-pool.md). |
| `gpuResourceClaim` | GPUResourceClaimSpec | — | Pass a GPU through via Kubernetes **DRA**: the scheduler + a DRA driver allocate at pod-schedule time. Mutually exclusive with `gpuProfileRef` and with `poolRef`. See [GPU sandboxes](gpu-sandboxes.md). |
| `gpuProfileRef.name` | string | — | Pass a GPU through via the **native SwiftGPU** backend: the SwiftGPU controller allocates at controller time against this `SwiftGPUProfile` and pins the sandbox to the allocated node. Mutually exclusive with `gpuResourceClaim` and with `poolRef`. `tier: pcie` only. See [GPU sandboxes](gpu-sandboxes.md). |
| `scratchDisk` | SandboxScratchDisk | — | Attaches one secondary raw block disk (`blank`, sandbox-owned, or `pvcRef`, an existing PVC) for build caches, datasets, or checkpoints. See [Scratch / persistent disks](scratch-disks.md). |
| `model.imageRef` / `model.mountPath` | string / string (default `/model`) | — | Mounts a read-only, node-shared model artifact (an OCI image whose filesystem holds the weights) over virtio-fs. See [GPU sandboxes › Model preload](gpu-sandboxes.md#model-preload). |

Everything in `spec` except `ttl` is immutable after create — recreate the
sandbox to change image, resources, command, or network.

### Status

| Field | Type | Description |
|---|---|---|
| `phase` | enum | `Pending`, `Materializing`, `Running`, `Completed`, `Failed`. |
| `conditions[]` | []Condition | `Resolved`, `RootfsReady`, `GuestRunning`, `WorkloadReady` (while running: the readiness probe, or True once the guest runs), plus `GPUAllocated` (native GPU backend only) and `ScratchDiskReady` (when `spec.scratchDisk` is set). |
| `nodeName` | string | Node running the sandbox. |
| `podRef` | string | Launcher pod name (the claimed slot's pod name for a pool checkout). |
| `rootfs.digest` | string | Resolved image digest (`sha256:...`). |
| `rootfs.sizeBytes` | int64 | Materialized ext4 size. |
| `rootfs.cachePath` | string | Node-local rootfs artifact path. |
| `runtime.pid` | int64 | Hypervisor process PID (reported by swiftletd). |
| `runtime.hypervisor` | string | Always `cloud-hypervisor`. |
| `network.primaryIP` | string | Guest DHCP IP. Absent for `network.mode: none`. An address on the launcher pod's private nat network, so it repeats across sandboxes. |
| `network.primaryIPScope` | string | Always `Pod` when `primaryIP` is set: reachable only from inside the launcher pod. |
| `network.podIP` | string | IP of the launcher pod, unique in the cluster. |
| `network.egressAllowed[]` | list | The egress allowlist the launcher enforces: `cidr`, `protocol`, `port`, and `from` (the spec entry, e.g. `service inference/llm`). |
| `gpu.devices[]` / `gpu.nodeName` / `gpu.hypervisor` | []string / string / string | The native backend's allocation (PCI addresses, allocated node, resolved hypervisor). Absent for the DRA backend (the claim's ResourceClaim status carries device identity) and non-GPU sandboxes. |
| `scratchDisk.pvcName` / `scratchDisk.devicePath` / `scratchDisk.bound` | string / string / bool | The attached scratch disk once its PVC is Bound. Absent when `spec.scratchDisk` is unset. |
| `model.digest` / `model.mountPath` / `model.cachePath` | string / string / string | The resolved model artifact once materialized. Absent when `spec.model` is unset. |
| `startedAt` | Time | When the guest began running. |
| `terminalAt` | Time | When the sandbox first reached `Completed`/`Failed` — the anchor for `spec.ttl`. |
| `exitCode` | int32 | The workload's real exit code: `0` → `Completed`, non-zero → `Failed`. |
| `message` | string | Human-readable status detail. |

Until the SwiftKernel the sandbox boots (`kernelProfileRef`, or its default)
exists in the sandbox's namespace and is `Ready`, the sandbox stays `Pending`
with no launcher pod, and `Resolved` is `False` with reason `KernelNotFound` or
`KernelNotReady`. A sandbox pinned to a node (a native GPU sandbox, or a
`kubernetes.io/hostname` in `nodeSelector`) needs the kernel `Ready` on that
node only. The controller checks again every 10 seconds.

`kubectl get sbox` prints Phase, Image, Node, Guest IP, Pod IP, and Age.

## Network modes

| Mode | Ingress | Egress | Use for |
|---|---|---|---|
| `restricted` (default) | Denied — nothing reaches the sandbox | DNS and the public internet are allowed; `169.254.0.0/16` (cloud metadata) and RFC1918 cluster/pod/service CIDRs are blocked | Untrusted code |
| `open` | Denied | Unrestricted — the whole cluster and internet | Trusted workloads that must reach in-cluster services |
| `none` | No network at all | No network at all | Pure compute / detonation |

Ingress isolation is a NetworkPolicy shared by `restricted` and `open` (deny
all inbound). The `restricted` vs `open` difference is entirely **in-pod
iptables** on the VM's forwarded traffic, not a NetworkPolicy — a
NetworkPolicy that blocked cluster egress would also cut swiftletd's own
status reporting, since the VM's traffic and swiftletd's apiserver calls
share the pod IP after MASQUERADE.

A networked sandbox resolves cluster service names and external names alike
— the controller injects the namespace's search domains and `ndots:5`.
`restricted` still blocks *connecting* to cluster IPs; name resolution and
egress reachability are separate concerns.

### Allowing specific destinations under `restricted`

A sandbox that must reach one in-cluster service (an inference endpoint, a
collector, a database) does not need `open`. List the destinations under
`network.egress.allow`, each a Service or an IPv4 CIDR, optionally narrowed to
ports:

```yaml
spec:
  network:
    mode: restricted
    egress:
      allow:
        - service: {name: llm, namespace: inference}   # namespace defaults to the sandbox's
          ports: [{port: 8000}]                        # TCP unless protocol: UDP
        - cidr: 10.20.0.0/24
          ports: [{port: 5432}]
```

- A Service is allowed at its IPv4 ClusterIP, on the ports listed (each must be
  one the Service declares) or, with no ports, on every port it declares. The
  controller resolves it when the launcher is created and records the result in
  `status.network.egressAllowed`. A Service that is missing, headless or has no
  IPv4 ClusterIP keeps the sandbox `Pending` with a reason naming it
  (`EgressServiceNotFound`, `EgressServiceNoClusterIP`,
  `EgressServicePortNotFound`). If the Service is later recreated with a new
  ClusterIP, a running sandbox keeps the old one; recreate the sandbox.
- A `cidr` with no ports allows any port and protocol in the range.
- `169.254.0.0/16` stays blocked whatever is allowed: its rule comes first. A
  CIDR inside it is refused outright. IPv6 stays blocked.
- `egress` is valid with `restricted` only.
- On a cluster whose CNI replaces kube-proxy, traffic from the guest to a
  ClusterIP may not be translated; allow the backing pods' range with a `cidr`
  instead.
- The destination's own NetworkPolicies still apply. The guest's traffic leaves
  as the launcher pod's IP.

### Exposing ports

A sandbox that serves requests (an HTTP API, a tool server, a preview
environment) declares its ports under `network.ports`. Each becomes a named
`containerPort` on the launcher pod, forwarded to the same port in the guest,
and the only inbound traffic the sandbox's NetworkPolicy admits. KubeSwift does
not create a Service: put the sandbox behind your own, selecting it by a label
from `podMetadata` and targeting the port by name.

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata:
  name: hello
spec:
  image: registry.example.com/team/hello-http:1
  command: ["/hello"]
  network:
    mode: restricted
    ports:
      - name: http-app        # IANA service name, unique in the sandbox
        port: 3000            # pod port and guest port; TCP
    ingress:                  # optional; without it any source may connect
      from:
        - namespaceSelector: {matchLabels: {team: a}}
  podMetadata:
    labels: {app: hello}
---
apiVersion: v1
kind: Service
metadata:
  name: hello
spec:
  selector: {app: hello}
  ports: [{port: 80, targetPort: http-app}]
```

- Only the declared ports are reachable, and only in a networked mode (`ports`
  with `mode: none` is refused). The launcher's own listener, the guest's
  dnsmasq, is bound to the in-pod bridge and loopback, not the pod IP.
- `ingress.from` takes NetworkPolicy peers (`podSelector`,
  `namespaceSelector`, `ipBlock`). The NetworkPolicy is the enforcement, so a
  CNI without NetworkPolicy support admits every source.
- `restricted` egress is unchanged; replies to accepted connections are the
  only new outbound flow.
- `podMetadata.labels` and `podMetadata.annotations` go on the launcher pod.
  Keys under `kubeswift.io` or any `*.kubeswift.io` domain are refused, as are
  the pod-network annotations KubeSwift sets itself (`k8s.v1.cni.cncf.io/`,
  `v1.multus-cni.io/`, `k8s.ovn.org/`). The launcher is privileged: metadata
  that makes another controller mutate it, such as mesh sidecar injection, is
  not supported.
- A launcher that exposes ports carries a readiness gate: it is Ready, and in a
  Service's endpoints, only while the workload is (see
  [Workload probes](#workload-probes)). Without a readiness probe that is as
  soon as the guest runs.

### Workload probes

`Running` means the microVM booted. A readiness probe says whether the
workload inside serves; a liveness probe ends a workload that stops answering.
Both take the familiar probe shape, limited to what can run against the guest:

```yaml
spec:
  readinessProbe:
    httpGet: {path: /healthz, port: http-app}   # or tcpSocket
    periodSeconds: 5
  livenessProbe:
    tcpSocket: {port: 3000}
    failureThreshold: 3
```

- swiftletd runs them from inside the launcher against the guest's address, so
  the port can be a number that is not exposed, or the name of a
  `network.ports` entry. `httpGet` is plain HTTP (2xx and 3xx pass);
  `exec`, `grpc`, `host`, HTTPS and `terminationGracePeriodSeconds` are refused.
  Not valid with `mode: none`.
- Defaults and thresholds are the kubelet's (period 10s, timeout 1s, success 1,
  failure 3); `initialDelaySeconds` counts from when the guest has an address.
- Readiness is the `WorkloadReady` condition, with the probe's last result as
  its message, and the launcher pod's readiness gate.
- A liveness failure ends the sandbox `Failed` with reason
  `LivenessProbeFailed` and deletes its launcher, as `spec.timeout` does.
  Nothing restarts it; its owner decides.
- On a warm-pool checkout the probes travel with the workload and start when
  it does.
- Probe headers are stored in the spec in plain text; do not put credentials in
  them.

## Signed images (verify before boot)

Set `spec.verifyKeySecretRef.name` to a Secret holding a cosign public key
(key `cosign.pub`) to require a valid signature before the sandbox boots:

```bash
cosign generate-key-pair                 # cosign.pub + cosign.key
cosign sign --key cosign.key <registry>/<image>@sha256:...
kubectl create secret generic cosign-pub --from-file=cosign.pub
```

```yaml
spec:
  image: <registry>/<image>@sha256:...
  verifyKeySecretRef:
    name: cosign-pub
```

The `sandbox-materialize` init container resolves the image digest and runs
`cosign verify <repo>@<digest>` against the key **before** it materializes a
single layer. A missing or invalid signature fails that init container, so the
sandbox goes `Failed` and never runs an unverified rootfs. cosign speaks HTTPS
only — a signed image must come from a TLS registry.

A `SwiftSandboxPool` takes the same `spec.verifyKeySecretRef`; every warm slot
is verified, so a pool never warms an unverified image. This mirrors
`SwiftImage`'s `spec.source.oci.verifyKeySecretRef` for golden VM disks.

## Interacting with a sandbox

> Also available in **kubeswift-ui** (v0.8.0+): open the sandbox in the Explorer
> and use the **Logs** and **Shell** buttons — the same console tail and
> interactive vsock exec, in the browser (via the gateway `/sandbox-logs` and
> `/sandbox-exec` planes).

```bash
swiftctl sandbox logs <name> [-f]
swiftctl sandbox exec <name> [-e KEY=VALUE] [-w DIR] [-i] [-t] -- <cmd> [args...]
swiftctl sandbox attach <name> [-- <cmd>]
```

- `logs` streams the workload's console (`-f` to follow).
- `exec` runs a command inside the sandbox's OCI rootfs over a host↔guest
  vsock channel (an in-guest agent). stdout/stderr stream back live and the
  command's exit code is propagated. `-e` (repeatable) sets environment
  variables, `-w` sets the working directory, `-i` forwards stdin, `-t`
  allocates an interactive TTY.
- `attach` is shorthand for `exec -it -- /bin/sh` (pass `-- <cmd>` to run
  something else). Terminal resizes propagate; exit with Ctrl-D or `exit`.

## Lifecycle

`Pending` (resolving the image and kernel profile) → `Materializing` (the
rootfs init container builds the ext4) → `Running` (guest up) → `Completed`
(workload exited `0`) or `Failed` (boot/materialize failure, non-zero exit,
or `spec.timeout` exceeded).

`status.exitCode` carries the workload's real exit code. `spec.timeout`
bounds a runaway run; `spec.ttl` cleans up a finished sandbox's record once
you're done inspecting it.

## Troubleshooting

- **Stuck `Pending`** — `Resolved=False` with `KernelNotFound` or
  `KernelNotReady`: create the SwiftKernel in the sandbox's namespace, or wait
  until `kubectl get swiftkernel sandbox` reads `Ready`.
- **Stuck `Materializing`** — check `kubectl describe pod <name>`. Unscheduled
  usually means no node carries `kubeswift.io/kernel-node=true`. A failing
  `sandbox-materialize` init container usually means the image pull failed —
  check `spec.image` and `spec.imagePullSecret` and inspect the init
  container's logs.
- **`tty` reports "not a tty" inside an attached session** — a chroot/devpts
  cosmetic quirk. The session is a real TTY; shells, `vi`, and `top` all
  behave interactively.
- **No `status.network.primaryIP`** — expected for `network.mode: none`; by
  design there is no network to report.

## See also

- [Warm pools (fast start)](warm-pool.md) — pre-booted slots for sub-second checkout
- [GPU sandboxes](gpu-sandboxes.md) — pass a GPU into a sandbox via the native SwiftGPU or DRA backend
- [Scratch / persistent disks](scratch-disks.md) — attach a secondary block disk for build caches or datasets
- [Build your own](build-your-own.md) — custom sandbox kernels + base images (BYO / in-house library)
- [`config/samples/sandbox/`](../../config/samples/sandbox/) — sample manifests and notes
- [swiftctl reference](../swiftctl.md)
- [SwiftKernel reference](../swiftkernel.md)

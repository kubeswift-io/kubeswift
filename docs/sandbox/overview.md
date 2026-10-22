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
  `ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.16`, pulled per node)

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
| `artifacts[]` | list | — | OCI artifacts mounted read-only from the node cache. See [OCI artifacts](#oci-artifacts). |
| `secretFiles[]` | list | — | Secret keys written into the guest as files. See [Secret files](#secret-files). |
| `readinessProbe` / `livenessProbe` | Probe | — | `httpGet` (HTTP) or `tcpSocket` against the guest. See [Workload probes](#workload-probes). |
| `readinessStartup` | object | — | `interval` (default 100ms), `window` (default 30s): retry the readiness probe quickly until it first passes. See [Fast first readiness](#fast-first-readiness). |
| `podMetadata.labels` / `podMetadata.annotations` | map | — | Added to the launcher pod; `*kubeswift.io` keys and pod-network annotations refused. |
| `network.egress.allow[]` | list | — | Destinations a `restricted` sandbox may reach: `service: {name, namespace}` or `cidr`, with optional `ports: [{port, protocol}]`. See [Allowing specific destinations](#allowing-specific-destinations-under-restricted). |
| `kernelProfileRef.name` | string | `sandbox` | SwiftKernel to boot. |
| `nodeSelector` | map[string]string | — | Additional node constraints, merged with the required `kubeswift.io/kernel-node=true`. |
| `poolRef.name` | string | — | Check out a pre-booted slot from this `SwiftSandboxPool` (sub-second) instead of the cold materialize+boot path; falls back to cold on a miss. Same namespace. See [Warm pools](warm-pool.md). |
| `gpuResourceClaim` | GPUResourceClaimSpec | — | Pass a GPU through via Kubernetes **DRA**: the scheduler + a DRA driver allocate at pod-schedule time. Mutually exclusive with `gpuProfileRef` and with `poolRef`. See [GPU sandboxes](gpu-sandboxes.md). |
| `gpuProfileRef.name` | string | — | Pass a GPU through via the **native SwiftGPU** backend: the SwiftGPU controller allocates at controller time against this `SwiftGPUProfile` and pins the sandbox to the allocated node. Mutually exclusive with `gpuResourceClaim` and with `poolRef`. `tier: pcie` only. See [GPU sandboxes](gpu-sandboxes.md). |
| `scratchDisk` | SandboxScratchDisk | — | Attaches one secondary raw block disk (`blank`, sandbox-owned, or `pvcRef`, an existing PVC) for build caches, datasets, or checkpoints. See [Scratch / persistent disks](scratch-disks.md). |
| `model.imageRef` / `model.mountPath` | string / string (default `/model`) | — | Mounts a read-only, node-shared model artifact (an OCI image whose filesystem holds the weights) over virtio-fs. See [GPU sandboxes › Model preload](gpu-sandboxes.md#model-preload). |
| `termination.drainPeriod` | duration | `5s` with `network.ports`, else `0s` | How long a running workload keeps going after deletion has taken it out of Service endpoints, before the microVM stops (0s to 10m). See [Deleting a sandbox](#deleting-a-sandbox). |

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
| `artifacts[]` | list | Each `spec.artifacts` entry: `name`, `digest`, `mountPath`, `state` (Resolved, Fetching, Dispatched, Mounted, Skipped), `reason`, `message`. See [OCI artifacts](#oci-artifacts). |
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
- swiftletd writes the readiness verdict to the launcher pod's readiness gate
  itself, as a pod status condition that names the workload it belongs to. A
  verdict for another workload never counts.

#### Fast first readiness

Probe times are whole seconds, so a workload that answers 300 ms after it
starts is found ready at the first probe, a second or more later.
`readinessStartup` probes it quickly until it first passes:

```yaml
spec:
  readinessProbe:
    httpGet: {path: /healthz, port: http-app}
    periodSeconds: 10
  readinessStartup:
    interval: 100ms   # default; 10ms to 1s
    window: 30s       # default; 1s to 10m
```

- The probe starts with the workload (`initialDelaySeconds` does not apply)
  and is retried every `interval`. Failures before the first pass do not count
  toward `failureThreshold` and report nothing; `successThreshold` still
  applies.
- After the first pass, or once `window` has passed without one, the probe
  runs every `periodSeconds` with the usual thresholds.
- Attempts never overlap: one that hangs waits for `timeoutSeconds`.
- On a launcher that reports readiness itself (see below), the pod's `Ready`
  usually follows the verdict within tens of milliseconds; a verdict that
  lands less than a second after another change to the pod can wait up to a
  second for the kubelet's next refresh.

#### Readiness reporting and Service routing

- swiftletd writes the verdict straight to the launcher pod's readiness gate,
  one `pods/status` write, when the chart installs the admission policy
  `kubeswift-sandbox-launcher-status-gate` (on by default with
  `launcherSAGate.enabled`, Kubernetes 1.30 or later). The policy lets the
  launcher's own account change that one condition and nothing else in the
  pod's status. Without it, launchers report through annotations and the
  controller writes the gate.
- A Service routes to the pod once kube-proxy has programmed the endpoint.
  kube-proxy's `minSyncPeriod` (default 1 s in iptables and nftables mode)
  spaces its syncs, so for a workload that is ready within a second of its
  Service being created, routing can wait for that interval. This is
  ordinary Kubernetes behavior: the default is supported and KubeSwift does
  not require changing it. Lowering `minSyncPeriod` is an operator tuning
  decision that trades kube-proxy CPU for latency, and KubeSwift does not
  change it on install.
- Probe headers are stored in the spec in plain text; do not put credentials in
  them.

## Secrets

A workload variable can take its value from a Secret in the sandbox's
namespace:

```yaml
spec:
  env:
    - name: DATABASE_URL
      valueFrom:
        secretKeyRef: {name: db, key: url}   # optional: true to leave it unset when missing
```

- The value never passes through the controller's objects. The controller
  checks the Secret and key exist and grants the sandbox's launcher, which runs
  as its own ServiceAccount (`kubeswift-sandbox-launcher-<pod>`), `get` on
  exactly the referenced Secrets. swiftletd reads them and gives them to the
  guest: on a cold boot on the config disk, which then lives in a memory-backed
  volume; on a warm-pool checkout over vsock with the rest of the workload's
  environment. No value is written to the intent ConfigMap, the SwiftSandbox,
  annotations, events or logs.
- A missing Secret or key keeps the sandbox `Pending` with reason
  `SecretNotFound` or `SecretKeyNotFound`, naming them. If the launcher then
  cannot read it, the sandbox fails with `SecretUnavailable`.
- Values are read when the workload starts. A rotated Secret reaches a new
  sandbox, not a running one.
- Anyone who can create a SwiftSandbox that names a Secret can read it from
  inside the guest: the same trust as a Pod's Secret references.
- Only `secretKeyRef` is supported; `fieldRef`, `resourceFieldRef` and
  `configMapKeyRef` are refused.

### Secret files

Structured credentials (a registry config, TLS keys, a cloud credentials file)
go in as files instead:

```yaml
spec:
  secretFiles:
    - secretName: registry-auth
      mode: 0400                       # default; an item may set its own
      items:
        - key: config.json
          path: /run/secrets/registry/config.json
```

- They travel like secret env: the launcher's own account reads them, and they
  reach the guest on the in-memory config disk (cold boot) or over vsock
  (warm-pool checkout). The guest writes them into its writable layer, which
  is in memory. A missing Secret or key keeps the sandbox `Pending` unless the
  entry sets `optional: true`.
- A path must be absolute and clean, unique, and not under `/proc`, `/sys`,
  `/dev`, the model mount or an artifact mount. All of a sandbox's secret
  files together may be at most 2 MiB.
- **Needs a newer sandbox kernel**: `kernels/sandbox:6.6.14` or
  `kernels/gpu-sandbox:6.6.3` and later, whose bridge writes the files. On an
  older kernel the sandbox fails with reason `KernelUnsupported` before it
  boots, instead of running without them. Changing an existing SwiftKernel's
  tag pulls nothing by itself; delete its pull Jobs afterwards
  (`kubectl -n <ns> delete job -l kubeswift.io/swiftkernel=<name>`).

## OCI artifacts

Content that is not part of the root filesystem (an application bundle,
plugins, a dataset, a policy bundle, a WebAssembly application) can be mounted
read-only from any OCI registry:

```yaml
spec:
  artifacts:
    - name: app
      ref: registry.example.com/team/hello-http@sha256:...   # a tag is resolved to a digest
      mountPath: /run/artifacts/app
      layout: oci                     # default; or unpacked
      pullSecretRef: {name: team-registry}    # optional; spec.imagePullSecret when unset
      verifyKeySecretRef: {name: team-cosign} # optional cosign public key (cosign.pub)
```

- `layout: oci` mounts an OCI image layout (`oci-layout`, `index.json`,
  `blobs/sha256/...`) of the manifest exactly as pushed, image, index or any
  artifact type, which a tool in the guest reads offline. `layout: unpacked`
  mounts the image's filesystem layers extracted into a tree, as `spec.model`
  does; its layers must be tar.
- The node pulls it, with its pull Secret (which never enters the guest),
  verifies it at its digest when a key is set, and caches it by digest: every
  sandbox on the node mounting the same digest shares one copy. A sandbox with
  `network.mode: none` can read it.
- `status.artifacts` records the digest mounted. A ref the registry refuses
  (malformed, not found, not readable with the pull Secret) fails the sandbox
  with `ArtifactResolveFailed`; a failed pull or signature check with
  `ArtifactMaterializeFailed`, naming the artifact. A registry that cannot be
  reached keeps it `Pending` and retries (see Troubleshooting).
- Mounts are read-only, at clean absolute paths that do not overlap each other,
  the model mount, `/proc`, `/sys` or `/dev`.
- **Platform verification.** An administrator can require, per namespace and
  repository, that artifacts carry a signature by a platform key with given
  signed claims (a cluster-scoped `SwiftArtifactVerificationPolicy`). It
  applies in addition to `verifyKeySecretRef`, and nothing in the sandbox can
  turn it off. See [Artifact verification policy](artifact-verification-policy.md).
- **Optional artifacts.** `optional: true` marks an artifact the workload can
  run without, such as a cache. When it cannot be resolved, fetched or
  verified in time, the workload starts without it: nothing is mounted at its
  `mountPath` (the path shows whatever the image has there), and
  `status.artifacts` records `state: Skipped` with a `reason`. A Warning Event
  (`ArtifactSkipped`, or `ArtifactVerificationFailed`) says why. Verification
  is never skipped: an optional artifact that fails it is not mounted.
  `fetchTimeout` (default `10s`, at most `5m`, optional artifacts only) bounds
  how long it may delay the workload: at checkout, counted from the sandbox's
  creation; on a cold start, waiting for its registry is counted from the
  sandbox's creation and fetching it from when the launcher pod is created.
  `0s` uses it only when it is already on the node. A required artifact (the
  default) behaves as before.

  | Optional artifact | Result |
  |---|---|
  | Registry refuses the ref | Skipped, `ResolveRefused` |
  | Registry not answering (cold start: until `fetchTimeout`; checkout: at claim time) | Skipped, `RegistryUnavailable` / `ResolvePending` |
  | Signature check fails | Skipped, `VerificationFailed` |
  | Pull or extraction fails, size limit | Skipped, `MaterializeFailed` |
  | Not on the node and `fetchTimeout` ran out (or is `0s`) | Skipped, `FetchTimeout` / `NotOnNode` |
  | Checkout and no warm slot can mount artifacts | Skipped, `KernelUnsupported` (the sandbox still takes a warm slot) |

- `status.artifacts[].state` is `Resolved`, `Fetching`, `Dispatched` (sent
  with the workload; the launcher mounts it before the workload starts, or the
  start fails), `Mounted` (the workload has run with it; `reason` `Cached` or
  `Fetched`) or `Skipped`. An entry without a state was written by an earlier
  release and was mounted.
- With `poolRef`, a sandbox with artifacts takes a warm slot and gets its
  artifacts at checkout, without a reboot, on a slot kernel with the
  `warm-mounts` feature (`kernels/sandbox:6.6.15`, `kernels/gpu-sandbox:6.6.4`
  or later); otherwise it boots cold. See
  [Artifacts on checkout](warm-pool.md#artifacts-on-checkout).
- Every cached file is readable by any guest user and writable by none
  (files 0444, executables in an unpacked tree 0555, directories 0555).
- An artifact is capped at 10 GiB (an unpacked tree extracted, an OCI layout's
  blobs); a larger one fails with `ArtifactMaterializeFailed`. An unpacked
  artifact with a sparse member is refused.
- **Node cache limits** (chart `sandboxMaterialize.artifactCache`, off by
  default): `maxBytes` and/or `maxPercent` of the cache filesystem (the
  smaller applies). After an artifact is written, the least recently used
  entries that no running sandbox holds, and that were not used within
  `minAge` (default 10m), are evicted until the cache fits; a sandbox using an
  evicted digest later has it fetched again. If entries in use keep the cache
  over the limit, the new artifact is refused (`ArtifactMaterializeFailed`;
  an optional one is skipped). Enable limits only once every sandbox launcher
  on the cluster runs v0.17 or later: older launchers do not hold use locks.
  Entries written before v0.17 are counted but never evicted, so a node whose
  cache is already over the limit refuses new artifacts until
  `artifactCache.evictLegacy: true` (set it once no pre-v0.17 launcher runs). Metrics:
  `kubeswift_sandbox_artifact_cache_bytes{node,kind}`,
  `kubeswift_sandbox_artifact_cache_evictions_total{node}`,
  `kubeswift_sandbox_artifact_cache_over_limit{node}`.
- **Needs a newer sandbox kernel**: `kernels/sandbox:6.6.14` or
  `kernels/gpu-sandbox:6.6.3` and later, whose bridge mounts the shares. On an
  older kernel the sandbox fails with `KernelUnsupported` before it boots.
  Changing an existing SwiftKernel's tag pulls nothing by itself; delete its
  pull Jobs afterwards (`kubectl -n <ns> delete job -l kubeswift.io/swiftkernel=<name>`).

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

## Registries behind a private CA

Sandbox images, models, artifacts, placements and output exports are pulled
and pushed over TLS, trusting the system CA roots. For a registry whose
certificate comes from a private CA, give its CA to the chart:

```yaml
sandboxMaterialize:
  registryCABundle: |
    -----BEGIN CERTIFICATE-----
    ...
    -----END CERTIFICATE-----
```

It is added to the system roots, never replacing them, for the controller's
own registry requests, every materialize init container, export Jobs and
cosign. Only the administrator sets it: a SwiftSandbox cannot add a CA, since
node caches are shared between namespaces. Snapshots use the storage
location's CA bundle instead.

## Node caches

A node keeps one copy of each root filesystem, model and artifact, by digest,
under `/var/lib/kubeswift/sandbox-rootfs`, `sandbox-models` and
`sandbox-artifacts`. Every namespace on the node shares them.

A digest says what the content is, not who may read it: a manifest copied into
another repository resolves to the same digest. So an entry is used for a
sandbox only once the repository its image reference names has served that
content. A pull from that repository counts. Otherwise the materializer reads
every layer from that repository once, checking each digest, and the sandbox
fails if the repository does not serve them. After that, the entry is used for
that repository without contacting the registry for layers.

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

### Deleting a sandbox

Deleting a sandbox whose workload still runs stops it in this order:

1. The sandbox gets the `Terminating` condition (reason `Draining`) and
   `WorkloadReady` turns False. The launcher pod's readiness gate is set
   False and the pod is deleted, so its Service endpoints stop taking new
   connections.
2. The workload keeps running for `spec.termination.drainPeriod`, or until
   it exits. Connections already open keep working. The default is `5s` for
   a sandbox that exposes `network.ports` and `0s` for one that does not.
3. swiftletd stops the microVM (`Terminating` reason `Stopping`). The
   launcher pod's grace period is the drain plus 15 s, so the kubelet only
   kills a launcher that failed to stop the microVM itself.
4. Once the launcher pod has stopped, its NetworkPolicy is removed and the
   sandbox goes.

```yaml
spec:
  termination:
    drainPeriod: 5s
```

The drain must cover the time your cluster takes to stop routing to a
removed endpoint, plus your longest request. kube-proxy applies endpoint
changes at most once per `minSyncPeriod` (1 s by default), so the default
`5s` leaves room on most clusters. A shorter drain refuses connections that
arrive before kube-proxy has caught up. The microVM is stopped, not shut
down: the workload gets no signal, so it must finish its work within the
drain. The launcher pod then ends `Succeeded`.

The sandbox's NetworkPolicy, and the egress rules inside the launcher pod,
apply for as long as the launcher pod exists: the policy carries the
`kubeswift.io/launcher-isolation` finalizer, released only when no pod it
selects is still running. That also holds when the namespace is deleted, or
the pool of a warm slot. Force-deleting a launcher pod
(`--grace-period=0 --force`) removes the pod object while its microVM may
still run, and with it this guarantee.

The sandbox itself carries the `kubeswift.io/sandbox-termination`
finalizer while its launcher stops. If the launcher pod is still there 30 s
after its grace period (its node is unreachable), the sandbox is released;
the NetworkPolicy stays until the pod is gone.

## Troubleshooting

- **Stuck `Pending`** — `Resolved=False` with `KernelNotFound` or
  `KernelNotReady`: create the SwiftKernel in the sandbox's namespace, or wait
  until `kubectl get swiftkernel sandbox` reads `Ready`.
- **`Pending` with `Resolving` or `RegistryUnavailable`**: the controller is
  waiting for the registry that serves `spec.image`, `spec.model` or an
  artifact. `RegistryUnavailable` means a request failed without an answer
  (unreachable, timed out, overloaded, a 5xx or 429); its message carries the
  error and when the next attempt is, 5 s doubling to 5 min. Each request is
  bounded (30 s), runs outside the reconcile loop, and at most four run
  against one registry at a time, so a stalled registry holds only the
  sandboxes and pools that use it. A refusal (malformed ref, not found, not
  authorized) fails the sandbox instead: retrying would not change it.
- **`Pending` with `PreviousLauncherStopping`**: a sandbox by this name was
  deleted and its launcher pod (also named after the sandbox) has not gone
  yet. The new sandbox creates its own launcher once it has.
  `LauncherPodConflict` means a running pod by that name is not this
  sandbox's: delete it, or use another name.
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

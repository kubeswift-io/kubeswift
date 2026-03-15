# SwiftGuest Networking and SSH Access — Design

## Context

KubeSwift has a working first-boot path: SwiftGuest controller creates a pod envelope, swiftletd launches Cloud Hypervisor, and the VM boots. The VM currently has **no network interface**—Cloud Hypervisor is launched without `--net`. Console access via serial is incomplete (GRUB patch, getty), so operators lack a reliable way to access running guests. The next milestone is to attach the guest to the pod network and enable SSH via cloud-init.

**Current architecture (smoke-tested):**
- **Controller** (`internal/controller/swiftguest/`): Reconciles SwiftGuest → Resolver → seed Render → ConfigMaps (seed, intent) → BuildPod → status from pod
- **Pod envelope**: One pod per SwiftGuest; volumes: run (emptyDir), root-disk (PVC), runtime-intent (ConfigMap), seed (ConfigMap when HasSeed), dev-kvm; single launcher container
- **Runtime intent** (`internal/runtimeintent/`): rootDisk, seedPath, cpu, memory, lifecycle, guestId; built from ResolvedGuest; serialized to ConfigMap
- **Seed** (`internal/seed/`): Render resolves userData/metaData/networkData from SwiftSeedProfile; BuildConfigMap with keys user-data, meta-data, network-config
- **swiftletd** (`rust/swiftletd/`): Reads intent from `/var/lib/kubeswift/intent/runtime-intent.json`; builds NoCloud from seed ConfigMap; spawns CH via swift-ch-client; reports GuestRunning via kube patch
- **swift-ch-client** (`rust/swift-ch-client/`): VmConfig → CH args (--disk, --serial socket=, --console off, --kernel); no --net

**Constraints:**
- One network per guest (MVP); use existing pod network
- No Multus, SR-IOV, or secondary NICs
- Use KubeSwift's seed/cloud-init model for SSH; no separate bootstrap path
- Namespace-aware; align with github.com/projectbeskar/kubeswift naming

## Goals / Non-Goals

**Goals:**
- Guest VM receives network connectivity via the pod network
- Guest gets an IP on the pod subnet (DHCP or static)
- SSH keys delivered via cloud-init `ssh_authorized_keys`
- SwiftGuest status exposes guest IP for operator discovery
- Sample manifests and docs for boot + SSH workflow
- Smoke test can validate networking and SSH (or follow-up extension)

**Non-Goals:**
- Multus, SR-IOV, secondary NICs, advanced network policy
- Kubernetes Services or LoadBalancers for guests
- VNC/SPICE, migration, snapshot
- Redesign of runtime architecture

## Deferred (Explicitly Out of Scope)

The following are **not** part of this change and must not be implemented:

- **Multus / secondary networks** — Only the default pod network is supported
- **SR-IOV / hardware passthrough** — Virtio-net only
- **Static IP without DHCP** — MVP uses DHCP; static can be added via networkData later
- **Kubernetes Service for guest** — No ClusterIP/NodePort/LoadBalancer for VM
- **swiftctl ssh command** — Operators use `ssh` directly with discovered IP
- **Multiple NICs** — One network interface per guest

## Decisions

### 1. Pod-network attachment: bridge + TAP

**Choice:** Create a Linux bridge in the pod's network namespace, move the pod's primary interface (eth0) onto the bridge, create a TAP device, add the TAP to the bridge, and pass the TAP to Cloud Hypervisor via `--net tap=<name>`.

**Flow:**
1. Init container runs network-init.sh before launcher
2. Create bridge `br0`, add eth0 to br0 (bridge inherits pod IP; pod remains reachable)
3. Create TAP device `tap0`, add to br0; init exits
4. Launcher starts dnsmasq, then swiftletd; swiftletd passes `--net tap=tap0` to Cloud Hypervisor
5. VM sees virtio-net; traffic flows over bridge to pod network

**Alternatives considered:**
- **Macvlan:** Simpler but may not work with all CNIs; bridge is more widely compatible
- **Slirp/user-mode:** No TAP; CH may not support; bridge is standard
- **Pre-created TAP by CNI:** Would require custom CNI; bridge in pod is self-contained

**Rationale:** Bridge + TAP is the common pattern for VM-in-pod networking (KubeVirt, Kata). Works with default CNI (Calico, Flannel, Cilium). No cluster-wide changes.

### 2. Guest IP: DHCP server in pod

**Choice:** Run a minimal DHCP server (e.g., dnsmasq) in the launcher pod. The server listens on the bridge, hands out an IP from the pod's subnet to the VM. Seed network-config instructs cloud-init to use DHCP.

**Flow:**
1. Launcher entrypoint derives pod subnet from br0 (e.g., 10.244.1.0/24)
2. Choose a small range for VMs (e.g., 10.244.1.10–10.244.1.20)
3. Start dnsmasq on br0 with that range before exec swiftletd
4. Seed network-config: `version: 2` with `dhcp4: true` on the primary interface
5. VM boots, cloud-init configures DHCP, VM gets IP from dnsmasq

**Alternatives considered:**
- **Static IP:** Would require injecting pod subnet into seed at runtime; controller doesn't know pod IP until pod is scheduled; adds complexity
- **CNI DHCP:** Pod network usually has no DHCP; we need a local server

**Rationale:** DHCP is flexible; VM gets IP without controller knowing pod IP in advance. dnsmasq is small and well-understood.

### 3. Guest IP discovery: DHCP lease file

**Choice:** dnsmasq writes DHCP leases to a file (e.g., `/var/lib/kubeswift/run/<guest-id>/dnsmasq.leases`). swiftletd (or a sidecar) reads the lease file after the VM boots, extracts the VM's IP, and reports it to the control plane via SwiftGuest status patch.

**Flow:**
1. dnsmasq configured with `--dhcp-leasefile=<path>`
2. When VM requests DHCP, dnsmasq writes lease (MAC, IP, hostname, expiry)
3. swiftletd polls the lease file; when VM MAC appears, extract IP
4. swiftletd patches **pod annotation** `kubeswift.io/guest-ip` with the IP
5. Controller (on pod update via Owns) reads annotation in MapPodToStatus and sets status.Network

**Alternatives considered:**
- **Cloud-init report-back:** Would require guest to call an API; adds guest-side logic
- **Assume first lease:** Fragile; lease file is reliable

**Rationale:** Lease file is standard; no guest modification. swiftletd already runs in the pod and can patch status (or delegate to controller with a different mechanism).

### 4. SSH keys: cloud-init ssh_authorized_keys

**Choice:** Operators add `ssh_authorized_keys` to the cloud-init user in SwiftSeedProfile userData. Cloud-init writes keys to `~/.ssh/authorized_keys` for the configured user. No new API fields; userData is the extension point.

**Example userData:**
```yaml
#cloud-config
users:
  - name: kubeswift
    passwd: $6$...
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - "ssh-ed25519 AAAA... user@host"
```

**Flow:**
1. Operator creates SwiftSeedProfile with userData containing `ssh_authorized_keys`
2. Or uses `userDataFrom` to reference a Secret/ConfigMap with the same content
3. Seed ConfigMap is mounted into pod; swift-seed builds NoCloud with user-data
4. VM boots; cloud-init creates user and writes authorized_keys
5. Operator SSHs with `ssh kubeswift@<guest-ip>` using the matching private key

**Alternatives considered:**
- **New spec field `sshKeys`:** Would duplicate cloud-init; userData is the standard
- **Separate bootstrap ISO:** Overkill; cloud-init already supports this

**Rationale:** Cloud-init is the standard; no new APIs. Operators already use SwiftSeedProfile for userData.

### 5. SwiftGuest status: network block

**Choice:** Add `status.network` to SwiftGuest with fields such as `primaryIP`, `interface`, `ready` (or a condition). Populated by swiftletd when the guest IP is discovered.

**Example:**
```yaml
status:
  phase: Running
  network:
    primaryIP: "10.244.1.12"
    interface: "eth0"
    ready: true
```

**Flow:**
1. swiftletd discovers IP from DHCP lease
2. swiftletd patches SwiftGuest `status.network` (requires RBAC for status subresource)
3. Operator runs `kubectl get swiftguest <name> -o jsonpath='{.status.network.primaryIP}'`

**Rationale:** Status is the Kubernetes-native place for discovered state. Operators use `kubectl get` or similar to discover IP.

### 6. Runtime intent: add network flag

**Choice:** Extend RuntimeIntent with `network: true` (or omit for backward compatibility; default true when seed present). swiftletd checks this to decide whether to set up bridge/TAP and pass `--net`. Controller includes it when building intent from ResolvedGuest.

**Rationale:** Keeps intent minimal; network is enabled when seed is present (typical for SSH). Could be explicit `network: true` for clarity.

### 7. Init container + launcher wrapper for network setup

**Choice:** Use an **init container** for bridge/TAP setup only. Use a **launcher entrypoint script** to start dnsmasq before swiftletd. dnsmasq cannot run in the init container (it would be killed when init exits).

**Flow:**
1. **Init container** (when HasSeed): Create br0, add eth0 to br0, create tap0, add tap0 to br0. Exit. Bridge and tap persist in pod netns.
2. **Launcher entrypoint** (when HasSeed): Read runtime intent for guest_id; derive pod subnet from eth0/br0; start dnsmasq on br0 with DHCP range and lease file at `/var/lib/kubeswift/run/<guest-id>/dnsmasq.leases`; exec swiftletd.
3. **swiftletd**: Launches CH with `--net tap=tap0`; polls lease file; patches pod annotation when IP found.

### 8. Minimum viable operator workflow

| Step | Action |
|------|--------|
| 1 | Create SwiftSeedProfile with userData containing `ssh_authorized_keys` and user (e.g., kubeswift) |
| 2 | Create SwiftGuest referencing SwiftImage, SwiftGuestClass, and SwiftSeedProfile |
| 3 | Wait for `status.phase=Running` and `status.network.ready=true` |
| 4 | Get IP: `kubectl get swiftguest <name> -o jsonpath='{.status.network.primaryIP}'` |
| 5 | SSH: `ssh kubeswift@<primaryIP>` (use private key matching authorized_keys) |

**Documentation:** `docs/guest-networking.md` and `docs/ssh-access.md` (or combined) will describe this workflow, how to provide SSH keys, and how to discover the guest IP.

### 9. Smoke test extension

**Choice:** Extend the existing smoke test to optionally validate networking and SSH. If a SwiftSeedProfile with SSH keys is provided (e.g., via env or fixture), the smoke test creates a SwiftGuest, waits for `status.network.primaryIP`, and runs `ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 kubeswift@<IP> echo ok`. Success means networking and SSH work.

**Alternative:** Separate follow-up change for smoke extension if networking implementation is large. Design defers to tasks for concrete scope.

## Risks / Trade-offs

| Risk | Mitigation |
|------|-------------|
| Bridge setup breaks pod networking | Init container must correctly move eth0 to bridge; test on multiple CNIs |
| dnsmasq not in base image | Add dnsmasq to swiftletd image or use a minimal DHCP impl |
| IP discovery race (VM not booted yet) | Poll lease file with backoff; status.network.ready=false until IP found |
| RBAC for swiftletd to patch status | Controller may need to own status updates; swiftletd reports via another channel (e.g., pod annotation) that controller reads |
| Multiple guests on same node, same subnet | Each pod has its own bridge; VM IPs are in pod's subnet range; no collision across pods |

**Trade-off:** swiftletd patching SwiftGuest status requires cluster-scoped or namespace-scoped RBAC. If the launcher pod cannot patch SwiftGuest, the controller must derive status from another source (e.g., pod annotations written by swiftletd, or a separate reporting path). Design leaves this to implementation; controller-based status update is acceptable.

## Migration Plan

1. Add init container to SwiftGuest pod spec for bridge/TAP/dnsmasq setup
2. Extend swift-ch-client and swiftletd to pass `--net tap=tap0` when network enabled
3. Extend runtime intent and controller to include network flag
4. Implement IP discovery (lease file) and status update (controller or swiftletd)
5. Add SwiftGuest status.network types and controller logic
6. Add sample SwiftSeedProfile with ssh_authorized_keys; update docs
7. Extend smoke test or add follow-up task for SSH validation

**Rollback:** Remove init container, remove `--net` from CH args; guests revert to no network. No data migration.

## File and Package Plan

### CRD and API

| Path | Change |
|------|--------|
| `api/swift/v1alpha1/swiftguest_types.go` | Add `Network *GuestNetworkStatus` to SwiftGuestStatus; add struct `GuestNetworkStatus { PrimaryIP string, Interface string, Ready bool }` |
| `config/crd/bases/swift.kubeswift.io_swiftguests.yaml` | Regenerate (make generate) for new status fields |

### Controller

| Path | Change |
|------|--------|
| `internal/controller/swiftguest/pod.go` | Add network-setup init container when rg.HasSeed(); add init container volume mounts (run for lease file); use same launcher image or dedicated network-setup image |
| `internal/controller/swiftguest/status.go` | In MapPodToStatus, read pod.Annotations["kubeswift.io/guest-ip"] and set status.Network |
| `internal/controller/swiftguest/constants.go` | Add PodAnnotationGuestIP = "kubeswift.io/guest-ip" |

### Runtime Intent

| Path | Change |
|------|--------|
| `internal/runtimeintent/types.go` | Add `Network bool` to RuntimeIntent |
| `internal/runtimeintent/build.go` | Set Network: rg.HasSeed() in Build() |
| `internal/runtimeintent/constants.go` | No change |

### Seed Rendering

| Path | Change |
|------|--------|
| `internal/seed/configmap.go` | When networkData empty and networking enabled (caller passes flag), inject default network-config (version 2, dhcp4: true) |
| `internal/seed/render.go` | No change; Render returns empty networkData when SwiftSeedProfile has none |
| `internal/controller/swiftguest/controller.go` | When rg.HasSeed() and networkData empty, pass default network-config to BuildConfigMap |

### Rust: swift-ch-client

| Path | Change |
|------|--------|
| `rust/swift-ch-client/src/config.rs` | Add `tap_name: Option<String>` to VmConfig; in to_args(), when Some, append `--net tap=<name>` |

### Rust: swiftletd

| Path | Change |
|------|--------|
| `rust/swiftletd/src/intent.rs` | Add `network: Option<bool>` to RuntimeIntent (default true when seed present) |
| `rust/swiftletd/src/launch.rs` | When intent.has_network(), pass tap_name: Some("tap0") to VmConfig |
| `rust/swiftletd/src/report.rs` | No change for GuestRunning; add report_guest_ip() or extend report to patch pod annotation |
| `rust/swiftletd/src/main.rs` | Add DHCP lease polling; when IP discovered, patch pod annotation kubeswift.io/guest-ip |

### Network Setup

| Path | Change |
|------|--------|
| `images/swiftletd/scripts/network-init.sh` (new) | Init container script: create br0, add eth0 to br0, create tap0, add tap0 to br0. Exit. |
| `images/swiftletd/scripts/launcher-entrypoint.sh` (new) | When network enabled: read intent for guest_id; create runtime dir; derive subnet from br0; start dnsmasq with lease file; exec swiftletd. Otherwise exec swiftletd directly. |
| `images/swiftletd/Containerfile` | Add ip, bridge-utils, dnsmasq; add scripts; CMD/ENTRYPOINT uses launcher-entrypoint.sh |

### Samples and Docs

| Path | Change |
|------|--------|
| `config/samples/swiftseedprofile-ssh.yaml` (new) | SwiftSeedProfile with ssh_authorized_keys in userData |
| `config/samples/README.md` or `docs/first-boot.md` | Add SSH workflow section |
| `docs/guest-networking-ssh.md` (new) | Single doc: pod network model, SSH key injection, IP discovery, operator workflow |

### Tests

| Path | Change |
|------|--------|
| `internal/controller/swiftguest/controller_test.go` | Test BuildPod includes init container when HasSeed |
| `internal/runtimeintent/build_test.go` | Test Network true when HasSeed |
| Smoke test | Follow-up task or extend `docs/smoke-verification.md` with SSH validation steps |

### RBAC

| Path | Change |
|------|--------|
| `config/rbac/` | Launcher pod (swiftletd) needs patch pods (for annotation). Or: use emptyDir shared with init; init writes lease; launcher patches pod. Launcher already has patch swiftguests/status for GuestRunning. For pod annotation: launcher needs patch pods. Add to swiftletd ServiceAccount role. |

## Data Flow Summary

1. **Controller** (reconcile): Resolve → render seed (with default network-config if empty) → build intent (Network: HasSeed) → BuildPod (init container when HasSeed) → create/update pod
2. **Pod start**: Init container runs network-init.sh (bridge + tap) → exits
3. **Launcher**: Entrypoint runs → start dnsmasq → exec swiftletd
4. **swiftletd**: Read intent → build NoCloud → launch CH with --net tap=tap0 → poll lease file → patch pod annotation when IP found
5. **Controller** (on pod update): MapPodToStatus reads annotation → sets status.Network
6. **Operator**: `kubectl get sg <name> -o jsonpath='{.status.network.primaryIP}'` → `ssh kubeswift@<IP>`

## Resolved Decisions

- **Status update path:** swiftletd patches **pod annotation** `kubeswift.io/guest-ip` when IP discovered. Controller reads annotation in MapPodToStatus and sets status.Network. Controller already reconciles on pod updates (Owns Pod).
- **DHCP:** dnsmasq runs in launcher (before swiftletd). Init container does bridge+tap only.
- **Network-config default:** When SwiftSeedProfile has no networkData and HasSeed, inject default `version: 2` with `dhcp4: true` so cloud-init uses DHCP.

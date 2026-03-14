## Context

Prerequisite changes implement isolated components. No integration path exists: mount paths may mismatch, runtime intent format may not align with swiftletd, and status fields may be inconsistent. This change fixes those gaps, adds sample manifests and a smoke test, and delivers the first bootable guest. Scope: integration only—no new components.

**Repository:** github.com/projectbeskar/kubeswift

## Control-plane vs node-runtime

### Control-plane (Go, cluster)

| Component | Package / binary | Responsibility |
|-----------|------------------|----------------|
| SwiftImage controller | internal/controller/swiftimage/ | Reconcile SwiftImage to status.phase Ready; set status.preparedArtifact.pvcRef |
| SwiftGuest resolver | internal/resolved/ | Produce ResolvedGuest from SwiftGuest, SwiftImage, SwiftSeedProfile |
| Seed renderer | internal/controller/swiftguest/ or internal/seed/ | Create ConfigMap with user-data, meta-data, network-config |
| SwiftGuest controller | internal/controller/swiftguest/ | Create pod envelope, runtime intent ConfigMap; map pod status to SwiftGuest status |
| Runtime intent builder | internal/runtimeintent/ | Build RuntimeIntent from ResolvedGuest; serialize to JSON |

Control-plane does NOT: exec into nodes, spawn Cloud Hypervisor, or connect to Cloud Hypervisor API.

### Node-runtime (Rust, pod on node)

| Component | Crate / binary | Responsibility |
|-----------|----------------|---------------|
| swiftletd | rust/swiftletd/ (binary) | Read runtime intent, orchestrate launch, report status |
| swift-seed | rust/swift-seed/ | Generate NoCloud media from seed inputs |
| swift-ch-client | rust/swift-ch-client/ | Cloud Hypervisor API client (Unix socket) |
| swift-runtime | rust/swift-runtime/ | Per-guest runtime directory setup |
| Cloud Hypervisor | External binary (cloud-hypervisor) | VMM; launched by swiftletd |

Node-runtime does NOT: schedule pods, resolve references, or create ConfigMaps.

## Data flow

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│ CONTROL PLANE (cluster)                                                           │
├─────────────────────────────────────────────────────────────────────────────────┤
│  1. User creates SwiftImage, SwiftSeedProfile, SwiftGuest                         │
│  2. SwiftImage controller → status.phase Ready, status.preparedArtifact.pvcRef     │
│  3. Resolver → ResolvedGuest (imageRef resolved, seed resolved if present)          │
│  4. Seed renderer → ConfigMap (user-data, meta-data, network-config)              │
│  5. SwiftGuest controller → Pod + runtime intent ConfigMap                         │
│     - Volume: image PVC (from preparedArtifact.pvcRef)                            │
│     - Volume: seed ConfigMap                                                       │
│     - Volume: runtime intent ConfigMap (runtime-intent.json)                       │
│  6. Kubernetes schedules Pod to node                                              │
│  7. SwiftGuest controller watches Pod; updates SwiftGuest status from Pod phase    │
└─────────────────────────────────────────────────────────────────────────────────┘
                                        │
                                        │ Pod starts; container runs swiftletd
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│ NODE RUNTIME (pod on node)                                                        │
├─────────────────────────────────────────────────────────────────────────────────┤
│  8. swiftletd reads runtime intent from /var/lib/kubeswift/intent/runtime-intent.json │
│  9. swiftletd (via swift-runtime) creates /var/lib/kubeswift/run/<guest-id>/      │
│ 10. swiftletd (via swift-seed) reads seed from /var/lib/kubeswift/seed,           │
│     generates NoCloud media → runtime dir                                          │
│ 11. swiftletd (via swift-ch-client) spawns Cloud Hypervisor with disk, memory,    │
│     cpu, network from intent; --api-socket Unix path                               │
│ 12. swiftletd monitors CH process; on VM running → patch SwiftGuest GuestRunning=True │
│ 13. On CH exit (stop/crash) → patch SwiftGuest GuestRunning=False                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

## Mount paths (canonical)

Controller and swiftletd MUST use the same paths. This change aligns them.

| Artifact | Mount path | Consumer |
|----------|------------|----------|
| Root disk (image PVC) | /var/lib/kubeswift/disks/root | swiftletd, runtime intent rootDisk.path |
| Seed ConfigMap | /var/lib/kubeswift/seed | swiftletd, swift-seed |
| Runtime intent | /var/lib/kubeswift/intent/runtime-intent.json | swiftletd |

**Packages to update:** internal/controller/swiftguest/ (pod spec), rust/swiftletd/ (intent path config).

## Runtime intent format (contract)

Runtime intent JSON MUST include fields swiftletd expects. This change aligns internal/runtimeintent/ output with rust/swiftletd/ parsing.

| Field | Type | Source |
|-------|------|--------|
| rootDisk.path | string | Pod volume mount path for image |
| rootDisk.format | string | ResolvedGuest / SwiftImage format |
| seedPath | string | Mount path for seed, or "" if no seed |
| cpu | int | ResolvedGuest.Resources |
| memory | int (MiB) | ResolvedGuest.Resources |
| lifecycle | "start" \| "stop" | runPolicy |
| guestId | string | For runtime dir, logging |

Deterministic JSON (sorted keys) for hashability. Stored in ConfigMap key `runtime-intent.json`.

## Status conditions and failure handling

### SwiftImage

| Phase | Condition | Failure |
|-------|-----------|---------|
| Ready | — | status.phase Failed; condition with reason (import error, validation failed, etc.) |
| Failed | — | User sees reason; must fix spec or source and recreate |

### SwiftGuest

| Condition | True when | False when |
|-----------|-----------|------------|
| Resolved | Resolver produced ResolvedGuest | Resolution failed (missing ref, image not Ready) |
| ImageReady | SwiftImage status.phase Ready | SwiftImage not Ready or missing |
| PodScheduled | Pod phase Running | Pod Pending (scheduling) or Failed |
| GuestRunning | swiftletd reported VM running | VM not started, stopped, or crashed |

| Phase | Meaning |
|-------|---------|
| Pending | Not resolved or pod not created |
| Scheduling | Pod Pending (waiting for node) |
| Running | Pod Running, GuestRunning=True |
| Stopped | VM shut down (lifecycle=stop or graceful exit) |
| Failed | Pod Failed, or swiftletd reported VM crash |

### Failure handling

| Failure | Control-plane response | Node-runtime response |
|---------|------------------------|------------------------|
| Resolution fails | Resolved=False; condition reason | — |
| SwiftImage not Ready | Block pod creation; ImageReady=False | — |
| Pod fails to schedule | PodScheduled=False; phase Scheduling | — |
| Pod fails (OOMKilled, etc.) | PodScheduled=False; phase Failed | — |
| swiftletd: intent missing | — | Exit with error; no CH launch |
| swiftletd: CH fails to start | — | Report GuestRunning=False; exit or retry (MVP: exit) |
| swiftletd: CH crashes | — | Report GuestRunning=False; set condition reason |
| Status patch fails (RBAC) | Controller may infer from Pod | Log; no retry for MVP |

## Package and file layout

```
github.com/projectbeskar/kubeswift/

config/samples/
├── swiftimage-http.yaml
├── swiftseedprofile-minimal.yaml
├── swiftguest-sample.yaml
└── README.md

docs/
└── first-boot.md

test/smoke/
├── boot-test.sh
└── Makefile              # make smoke-test

# Integration fixes (existing packages):
internal/controller/swiftguest/   # Mount paths, runtime intent format, status mapping
internal/controller/swiftimage/   # preparedArtifact usable by SwiftGuest controller
internal/runtimeintent/          # JSON format matches swiftletd expectations
rust/swiftletd/                  # Intent path, seed path, error handling
```

## Goals / non-goals

**Goals:** Align mount paths, runtime intent format, status fields; add samples, docs, smoke test; first bootable guest.

**Non-goals:** Live migration, snapshots, VFIO, vhost-user, serial console, multi-disk, multi-network, production hardening.

## Risks / trade-offs

| Risk | Mitigation |
|------|------------|
| Sample image URL stale | Document; use stable URL (e.g., Ubuntu cloud images) |
| Image import slow | Document timeline; smoke test timeout (e.g., 15 min) |
| Node lacks Cloud Hypervisor | Prerequisite; swiftletd exits with clear error |
| Smoke test flaky | Minimal assertions; document known flakiness |

## Rollback

Revert patches to internal/controller/, internal/runtimeintent/, rust/swiftletd/; delete config/samples/, docs/first-boot.md, test/smoke/. No CRD changes. Existing SwiftGuest/SwiftImage unchanged.

## Open questions

- Exact sample image URL (Ubuntu 24.04 cloud image or smaller test image)
- Smoke test: CI integration or manual only

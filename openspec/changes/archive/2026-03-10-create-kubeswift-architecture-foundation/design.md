## Context

KubeSwift is a new Kubernetes-native virtualization platform. There is no existing implementation; this change establishes the architecture foundation and initial repository scaffolding. The platform targets Cloud Hypervisor as the sole VMM, avoids libvirt entirely, and keeps VM semantics explicit. Kubernetes remains responsible for scheduling, networking, and storage orchestration; a node-local daemon (`swiftletd`) realizes VMs via Cloud Hypervisor.

**Constraints:** Single monorepo at github.com/projectbeskar/kubeswift; Go for control plane, Rust for node runtime; no KubeVirt naming or compatibility.

## Goals / Non-Goals

**Goals:**

- Document control plane vs. node plane boundaries and data flow.
- Define initial repository structure (packages, binaries, docs).
- Establish runtime handoff model (controllers → pod envelope → swiftletd).
- Record validation, status conditions, and failure-handling approach.
- Create API scaffolding and architecture docs.

**Non-Goals:**

- Full controller implementation.
- Full swiftletd implementation.
- Live migration, snapshots, Windows guests.
- KubeVirt compatibility or multi-hypervisor support.

## Decisions

### 1. Language split: Go control plane, Rust node runtime

**Rationale:** Go aligns with Kubernetes ecosystem (controller-runtime, client-go, Kubebuilder). Rust suits low-level VM handling, Cloud Hypervisor API integration, and memory safety for privileged node operations.

**Alternatives considered:** All-Go (rejected: weaker fit for low-level VM code); All-Rust (rejected: controller ecosystem is Go-native).

### 2. Monorepo layout

**Rationale:** Single source of truth; simpler CI and versioning; shared tooling.

**Structure:**

```
github.com/projectbeskar/kubeswift/
├── cmd/
│   └── manager/          # Go control plane entrypoint
├── pkg/                   # Go packages (internal)
│   ├── apis/
│   │   ├── swift/         # swift.kubeswift.io
│   │   ├── image/         # image.kubeswift.io
│   │   └── seed/          # seed.kubeswift.io
│   └── controllers/
├── internal/              # Go internal packages
│   └── resolved/          # Resolved-spec model
├── swiftletd/             # Rust crate for node daemon (scaffold)
│   └── src/
├── config/                # CRD manifests, webhooks, RBAC
├── docs/
│   └── architecture/      # Architecture documentation
├── go.mod
├── go.sum
└── Cargo.toml             # Workspace root for Rust
```

### 3. Runtime model: one guest per pod, swiftletd on node

**Rationale:** Pod envelope provides scheduling, resource accounting, networking, and storage mounting. swiftletd runs as a DaemonSet or static binary on each node, receives runtime intent, and drives Cloud Hypervisor via local Unix sockets.

**Data flow:**

1. Controller reconciles SwiftGuest → creates/updates Pod (envelope).
2. Controller writes runtime intent (resolved spec) into a well-known location (e.g., ConfigMap or mounted file) for the pod.
3. swiftletd (or init container) reads intent, launches Cloud Hypervisor, manages VM lifecycle.
4. swiftletd reports status back (e.g., via status subresource or conditions).

### 4. Image and initialization as separate subsystems

**Rationale:** Image import/preparation (SwiftImage) is independent of guest boot-time initialization (SwiftSeedProfile, seed media). SwiftImage is immutable once ready; seed media is generated per-guest from SwiftSeedProfile.

**Alternatives considered:** Combined image+seed (rejected: violates immutability and separation of concerns).

### 5. Cloud-init via datasource media only

**Rationale:** KubeSwift generates NoCloud (and later ConfigDrive, Ignition) datasource media. It does not reimplement cloud-init; the guest’s cloud-init runs against the provided media.

### 6. Resolved-spec model

**Rationale:** Controllers resolve SwiftGuest + SwiftGuestClass + SwiftImage + SwiftSeedProfile into an internal resolved-spec before handoff. This decouples API evolution from runtime format and simplifies validation.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Go 1.25 not yet released | Use Go 1.22+ for now; document 1.25 as target when available |
| Rust/Go boundary complexity | Clear API contract (resolved-spec format); minimal cross-language surface |
| Over-scoped scaffolding | Limit to API types, manager skeleton, swiftletd stub; no full reconciliation |
| Architecture doc drift | Keep docs in `docs/architecture/` and update with implementation changes |

## Migration Plan

1. Create repository structure (directories, go.mod, Cargo.toml).
2. Add CRD manifests and API types (no controllers).
3. Add manager binary skeleton (no reconciliation logic).
4. Add swiftletd Rust crate with placeholder binary.
5. Add architecture documentation.
6. **Rollback:** Remove added files; no runtime impact.

## Open Questions

- Exact format of runtime intent (JSON, protobuf, or structured file) for handoff to swiftletd.
- Whether swiftletd runs as sidecar, DaemonSet, or static node binary (design doc can note options; implementation will decide).

## Context

SwiftGuest references SwiftSeedProfile for cloud-init seed data. The resolver (add-swiftguest-resolver) produces ResolvedGuest.Seed with userData, metaData. The SwiftGuest controller must create a Kubernetes artifact that the node runtime (swiftletd) consumes to build local NoCloud seed media. KubeSwift delivers cloud-init-compatible datasource content; it does not reimplement cloud-init. The guest's cloud-init reads the NoCloud media; KubeSwift only provides the media.

**Constraints:** NoCloud only; text-based artifact (no ISO in API server); local seed media generation in swiftletd or rust/swift-seed; add-swiftguest-resolver prerequisite.

## Goals / Non-Goals

**Goals:**

- Resolve user-data, meta-data, network-data from SwiftGuest and SwiftSeedProfile
- Support Secret and ConfigMap references
- Create ConfigMap with NoCloud-standard text keys
- Mount ConfigMap into pod for node runtime
- Keep artifact text-based (no ISO blobs)
- Add Rust helper or swiftletd logic to build local NoCloud media from ConfigMap
- Simple, deterministic templating

**Non-Goals:**

- ConfigDrive, Ignition, Windows unattended
- Reimplementing cloud-init
- Storing ISO in API server

## Decisions

### 1. KubeSwift provides datasource delivery, not cloud-init

KubeSwift creates the NoCloud datasource media (user-data, meta-data, network-config files). The guest's cloud-init (inside the VM) discovers and processes this media. KubeSwift does NOT parse, validate, or execute cloud-init user-data. It only delivers the bytes.

**Rationale:** cloud-init is complex; reimplementing it is out of scope. KubeSwift's job is to get the right content to the right place in the format cloud-init expects.

### 2. Control plane creates ConfigMap; node runtime builds seed media

**Control plane (Go):**

- Resolves user-data, meta-data, network-data from ResolvedGuest.Seed (which came from SwiftSeedProfile)
- Resolves Secret/ConfigMap refs: fetches Secret or ConfigMap, extracts value by key
- Creates a ConfigMap with keys: `user-data`, `meta-data`, `network-config` (NoCloud standard names)
- Values are plain text (or base64 for binary—avoid for MVP)
- Mounts ConfigMap into the pod envelope at a well-known path (e.g., `/var/lib/kubeswift/seed/<guest>/`)

**Node runtime (Rust, swiftletd or rust/swift-seed):**

- Reads ConfigMap contents from mounted path
- Builds NoCloud directory: `openstack/latest/user_data`, `openstack/latest/meta_data.json`, `openstack/latest/network_config.json` (or NoCloud v2 layout)
- Optionally creates ISO from directory for CDROM attachment
- Passes path or ISO to Cloud Hypervisor

**Rationale:** Control plane has API access for Secret/ConfigMap; node runtime has local filesystem for building media. Separation keeps concerns clear.

### 3. Artifact is text-based; no ISO in API server

The ConfigMap contains text only. Keys and values are strings. No binary blobs (ISO) stored in etcd. The node runtime builds the ISO locally if needed.

**Rationale:** ConfigMap size limits; etcd not suited for large binaries; ISO generation is node-local work.

### 4. Secret and ConfigMap references

SwiftSeedProfile (or SwiftGuest override) can specify:

- `userData`: inline string OR `secretKeyRef` / `configMapKeyRef`
- `metaData`: inline string OR ref
- `networkData`: inline string OR ref (for network-config)

Control plane resolver fetches the ref, extracts the value, and writes it into the ConfigMap. If ref is invalid (Secret not found, key missing), resolution fails.

### 5. NoCloud key naming

NoCloud expects specific filenames. ConfigMap keys map to them:

| ConfigMap key | NoCloud file / content |
|---------------|------------------------|
| user-data | user_data (or openstack/latest/user_data) |
| meta-data | meta_data (or meta_data.json) |
| network-config | network_config (or network_config.json) |

The node runtime copies ConfigMap keys to the correct NoCloud layout. NoCloud v1 uses a flat directory; v2 uses `openstack/latest/`. Design chooses one layout; rust/swift-seed implements it.

### 6. Package and file layout

**Control plane:**

```
internal/seed/
├── render.go       # Resolve user-data, meta-data, network-data from ResolvedGuest.Seed
├── resolve_refs.go # Fetch Secret/ConfigMap, extract by key
└── configmap.go    # Build ConfigMap from resolved content
```

Or: integrate into SwiftGuest controller (internal/controller/swiftguest/) as a seed-rendering step. Design prefers internal/seed/ for separation.

**Node runtime:**

```
rust/swift-seed/
├── src/
│   ├── lib.rs      # NoCloud builder
│   ├── nocloud.rs  # NoCloud directory layout, optional ISO
│   └── configmap.rs # Read from mounted ConfigMap path
```

swiftletd calls swift-seed to build media before launching VM.

### 7. Templating: simple and deterministic

If SwiftSeedProfile supports templating (e.g., `{{.InstanceID}}`), it MUST be simple and deterministic. No arbitrary code execution. Supported variables: instance-id, hostname, etc. If no templating in MVP, userData/metaData are passed through as-is.

**MVP:** Pass-through only. No templating. Content from SwiftSeedProfile (or ref) is used verbatim. Templating can be added later with a strict variable set.

### 8. Data flow

1. SwiftGuest controller reconciles SwiftGuest.
2. Resolver produces ResolvedGuest (includes Seed: userData, metaData, networkData or refs).
3. Seed renderer resolves refs (fetch Secret/ConfigMap), produces final strings.
4. Controller creates ConfigMap with keys user-data, meta-data, network-config.
5. Controller adds ConfigMap volume to pod spec, mounts at `/var/lib/kubeswift/seed/<guest-name>/`.
6. Pod scheduled; swiftletd starts.
7. swiftletd (or init) calls swift-seed: read ConfigMap mount, build NoCloud dir, optionally ISO.
8. swiftletd passes seed path/ISO to Cloud Hypervisor.
9. VM boots; cloud-init finds NoCloud, runs.

### 9. When no SwiftSeedProfile

If SwiftGuest does not reference SwiftSeedProfile, no seed ConfigMap is created. No seed volume is mounted. VM boots without cloud-init (or with default behavior). This is valid.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Secret/ConfigMap ref in different namespace | Use same-namespace refs for MVP; cross-namespace later |
| Large user-data in ConfigMap | ConfigMap 1MB limit; use Secret for large content |
| NoCloud layout version | Pick v1 or v2; document; rust/swift-seed implements |
| Node runtime read before mount ready | Init container or swiftletd retry |

## Migration Plan

1. Add internal/seed/ renderer
2. Extend SwiftSeedProfile API for Secret/ConfigMap refs if needed
3. SwiftGuest controller creates seed ConfigMap when SeedProfile ref present
4. Add rust/swift-seed NoCloud builder
5. swiftletd integrates swift-seed
6. **Rollback:** Remove seed ConfigMap creation; guests without seed continue; guests with seed fail until restored

## Open Questions

- NoCloud v1 vs v2 layout (v1 is simpler; v2 is more common in cloud images)
- Whether network-config is required or optional (many images work with DHCP only)

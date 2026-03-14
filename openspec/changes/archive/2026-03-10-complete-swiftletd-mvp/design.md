## Context

swiftletd already has: intent parsing (rust/swiftletd/src/intent.rs), path handling, and NoCloud generation via rust/swift-seed. The main flow reads intent, calls build_nocloud_dir when seed is present, then exits. rust/swift-ch-client and rust/swift-runtime are empty placeholders. implement-swiftletd-mvp defined the design; this change implements the remaining pieces: runtime directory, CH launch, lifecycle monitoring, status reporting.

**Constraints:** rust/ workspace; local Unix sockets only; Cloud Hypervisor binary prerequisite; complete-swiftguest-controller (pod, intent) prerequisite.

## Goals / Non-Goals

**Goals:**

- Implement rust/swift-runtime: create per-guest runtime directory
- Implement rust/swift-ch-client: spawn CH process, Unix socket client, VM config from intent
- Integrate runtime dir and CH launch into swiftletd main flow
- Monitor CH process lifecycle (exit detection, state: Running/Stopped/Failed)
- Report VM state to control plane (patch SwiftGuest status, GuestRunning condition)
- Use local Unix sockets only; no TCP

**Non-Goals:**

- Live migration, snapshots, VFIO, vhost-user
- TCP exposure of CH API

## Decisions

### 1. Per-guest runtime directory layout

rust/swift-runtime creates `/var/lib/kubeswift/run/<guest-id>/` with:
- `seed/` — NoCloud output (from swift-seed)
- `ch.sock` — Cloud Hypervisor API socket path
- Optional: logs, pid file

guest-id from intent.guest_id (e.g., namespace/name). Base path configurable via env (default /var/lib/kubeswift/run).

**Rationale:** Isolates per-guest artifacts; socket path known before CH spawn.

### 2. Cloud Hypervisor launch: spawn + args

swiftletd spawns `cloud-hypervisor` (or `cloud-hypervisor-static`) as child process. Args: `--api-socket <path>`, `--disk path=<image>`, `--memory size=<mem>Mi`, `--cpus num=<cpu>`, `--kernel` (skip for direct disk boot), `--console off` or serial. For disk-only boot (no kernel): `--disk path=...` is sufficient; CH boots from disk. MVP: one root disk, one virtio net (default tap or similar).

**Rationale:** CH CLI is the stable interface; JSON API can be added later for shutdown.

### 3. swift-ch-client: process spawn + socket connect

rust/swift-ch-client provides:
- `spawn_ch(args) -> Child` — spawn CH process
- `connect(socket_path) -> Client` — connect to Unix socket when ready (poll until socket exists)
- `shutdown_vm(client)` — send shutdown via CH API (optional for MVP; process wait may suffice)

MVP: spawn CH, wait for socket, connect for shutdown on lifecycle=stop. For lifecycle=start: spawn and monitor.

**Rationale:** Separates spawn from API client; socket path in runtime dir.

### 4. NoCloud output in runtime dir

swift-seed build_nocloud_dir output goes to `<runtime-dir>/seed/` instead of hardcoded `/var/lib/kubeswift/seed-built`. CH gets `--disk path=<runtime-dir>/seed` as CDROM or `--fs` for virtio-fs. MVP: use `--disk` with path to seed directory as CDROM (CH supports directory as disk for NoCloud). Check CH docs: `--disk path=...` for file; for directory may need `--fs` or create ISO. Simpler: create ISO via genisoimage or similar; `--disk path=<iso>`. Or: CH may accept directory for cloud-init. For MVP: create minimal ISO from NoCloud dir if CH requires file; or use `--fs` if supported.

**Pragmatic choice:** Use swift-seed to write to runtime dir. CH `--disk` for root; for seed, use `--disk path=<nocloud.iso>` if we generate ISO, or `--fs` if CH supports directory. Document: MVP uses `--disk` for root only; seed as second `--disk` (ISO) or `--fs`. Implement: generate ISO in swift-seed or swift-runtime when seed present; pass to CH.

**Simplified:** NoCloud dir as `--fs` backend if CH supports it; else generate ISO. MVP: generate ISO (genisoimage or pure Rust crate) for seed media.

### 5. Lifecycle monitoring

swiftletd waits on CH child process (tokio::process or std::process::Child::wait). On exit:
- Exit 0: VM shut down gracefully → state Stopped
- Non-zero: VM crashed or error → state Failed

Report state to control plane after exit. While running: state Running.

**Rationale:** Simple; no need for CH API polling for MVP.

### 6. Status reporting

swiftletd patches SwiftGuest status using kube-rs (or similar). In-cluster config from `/var/run/secrets/kubernetes.io/serviceaccount/`. RBAC: patch `swiftguests/status` in guest namespace. SwiftGuest name/namespace from pod ownerReference (read from downward API or mounted pod spec). On VM running: patch GuestRunning=True. On VM stopped/failed: patch GuestRunning=False, set condition reason.

**Rationale:** Controller infers from pod phase; swiftletd provides VM-level granularity (GuestRunning).

### 7. Crate layout

```
rust/
├── swiftletd/       # main.rs, intent.rs, launch.rs, report.rs
├── swift-seed/      # (exists) build_nocloud_dir
├── swift-ch-client/ # spawn, connect, config builder
└── swift-runtime/   # create_runtime_dir, cleanup
```

### 8. Main flow

1. Read intent (existing)
2. Create runtime dir (swift-runtime)
3. If has_seed: build NoCloud to runtime dir (swift-seed)
4. If lifecycle=stop: skip launch; report Stopped
5. Spawn CH (swift-ch-client)
6. Report Running
7. Wait on CH process
8. On exit: report Stopped or Failed
9. Cleanup runtime dir (optional on exit)

## Risks / Trade-offs

| Risk | Mitigation |
|------|-------------|
| CH binary not on node | Document prerequisite; fail with clear error |
| CH API version drift | Pin CH version; validate |
| Status patch fails (RBAC) | Log; controller infers from pod; retry |
| CH crash leaves orphan state | Reap process; report Failed |
| Seed ISO generation | Use genisoimage or Rust crate; document |

## Migration Plan

1. Implement swift-runtime create_runtime_dir
2. Implement swift-ch-client spawn, connect, config
3. Update swiftletd main flow
4. Add report module (kube-rs)
5. Add RBAC for swiftletd
6. Container image with CH binary
7. **Rollback:** Use placeholder container

## Open Questions

- Exact CH CLI args for disk-only boot (no kernel)
- Seed media: ISO vs directory for CH
- Cloud Hypervisor version to target

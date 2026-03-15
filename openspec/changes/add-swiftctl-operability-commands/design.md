# swiftctl Operability Commands — Design

## Context

KubeSwift has a working first-boot path: SwiftGuest controller creates a pod, swiftletd launches Cloud Hypervisor, and the VM boots. The `cmd/swiftctl/main.go` binary exists but is a no-op. Operators need a CLI for day-1/day-2 operability. KubeVirt's virtctl provides a useful **UX reference** for console and lifecycle operations; KubeSwift must **not** clone virtctl—it must align with SwiftGuest APIs and the current smoke-tested runtime model.

**Current SwiftGuest model (real-cluster verified):**
- `spec.runPolicy`: Running (default) or Stopped
- `status.phase`: Pending, Scheduling, Running, Stopped, Failed
- Pod name equals SwiftGuest name; pod has label `swift.kubeswift.io/guest=<name>`
- swiftletd runs in the launcher container; when `lifecycle=stop` in runtime intent, it exits without launching CH
- Controller does not delete the pod when runPolicy changes; pod must be deleted to force recreation with new intent

**Current console state:** Cloud Hypervisor is launched without `--console`; VM output is not captured. swift-ch-client does not pass console options.

## Goals / Non-Goals

**Goals:**
- Implement swiftctl as the canonical operator CLI for SwiftGuest operability
- Lifecycle commands (start, stop, restart) that map exactly to SwiftGuest spec/status
- Console access that works with the current launcher/runtime path—no invented endpoints or transport
- Namespace-aware usage; kubeconfig/context handling
- Integrate swiftctl into build, packaging, release docs, version stamping
- Prioritize operator usability and correctness over breadth

**Non-Goals:**
- VNC/SPICE, image upload, migration, snapshot commands
- kubectl plugin/krew packaging
- Rewriting the control plane or adding new APIs

## Decisions

### 1. Lifecycle commands: exact mapping to SwiftGuest

| Command | SwiftGuest operations | Pod operations |
|---------|------------------------|----------------|
| **start** | Patch `spec.runPolicy=Running` | Delete pod if exists (Completed or Running). Controller recreates pod with `lifecycle=start`; swiftletd launches CH. |
| **stop** | Patch `spec.runPolicy=Stopped` | Delete pod. Controller recreates pod with `lifecycle=stop`; swiftletd exits without launching. Phase → Stopped. |
| **restart** | No spec change (runPolicy must already be Running) | Delete pod. Controller recreates pod; swiftletd launches CH. VM restarts. |

**Rationale:** runPolicy and lifecycle already exist; controller and swiftletd honor them. Pod deletion forces recreation with updated intent. No controller changes.

### 2. Console transport: exec + tail of VM console file

**Choice:** swiftctl console uses `kubectl exec` into the launcher pod to run `tail -f` on a VM console file. The file is produced by Cloud Hypervisor when launched with `--console file=<path>`.

**Exact flow:**
1. swiftletd passes `--console file=<runtime-dir>/console.log` to Cloud Hypervisor (path: `/var/lib/kubeswift/run/<guest-id>/console.log`; guest-id = `namespace-name` per swift-runtime sanitization)
2. VM serial/virtio-console output is written to that file
3. swiftctl console: resolve SwiftGuest → pod → exec into launcher container → `tail -f /var/lib/kubeswift/run/<namespace>-<name>/console.log`

**No invented mechanisms:** Uses existing exec, existing runtime dir layout, Cloud Hypervisor's native `--console file=` support.

### 3. Guest discovery

- Accept guest name as positional arg
- Resolve SwiftGuest in namespace (from `-n` or kubeconfig default)
- Get pod from `status.podRef` or label `swift.kubeswift.io/guest=<name>`

### 4. CLI structure (cobra)

```
swiftctl [global flags] <command> [command flags] <guest-name>
```

**Global flags:** `-n, --namespace`, `--kubeconfig`, `--context`

**Commands:** `start`, `stop`, `restart`, `console`

### 5. Error handling and exit codes

| Situation | Message | Exit code |
|-----------|---------|-----------|
| SwiftGuest not found | `swiftguest "X" not found in namespace Y` | 1 |
| Pod not found (console) | `pod for guest X not found` | 1 |
| Guest not Running (console) | `guest X is not Running (phase: Stopped)` | 1 |
| Exec failed | `failed to attach to console: ...` | 1 |
| Patch/delete failed | `failed to stop guest: ...` | 1 |
| Success | — | 0 |

Errors to stderr; stdout for console stream only.

### 6. swiftletd change: console file path

Add `--console file=<runtime-dir>/console.log` to CH args in swift-ch-client. Path: `/var/lib/kubeswift/run/<guest-id>/console.log`. Extend `VmConfig` with optional `console_path`; swiftletd passes it when building config.

### 7. Release integration

| Area | Current state | Change |
|------|---------------|--------|
| **Build flow** | `make build-go` builds `./cmd/...` | swiftctl is under cmd/; already built. Add explicit `build-swiftctl` target if desired for clarity. |
| **Packaging flow** | Images + Helm chart | swiftctl is client-side; build binary. Add swiftctl binary to GitHub Release artifacts (release-stable). |
| **Release documentation** | docs/releases.md | Add swiftctl section: how to obtain (go install, Release download), version stamping. |
| **Version stamping** | internal/version via ldflags | swiftctl uses same ldflags; `swiftctl --version` prints Version, GitCommit. |

### 8. Deferred (non-MVP)

- `swiftctl list` — use `kubectl get swiftguests`
- `swiftctl ssh` — use cloud-init SSH keys
- VNC/SPICE, image upload, migration, snapshot, plugin packaging

## File and package plan

| Path | Purpose |
|------|---------|
| `cmd/swiftctl/main.go` | Entry point; cobra root; wires commands |
| `cmd/swiftctl/root.go` | Root command, global flags (namespace, kubeconfig, context) |
| `cmd/swiftctl/start.go` | `swiftctl start` |
| `cmd/swiftctl/stop.go` | `swiftctl stop` |
| `cmd/swiftctl/restart.go` | `swiftctl restart` |
| `cmd/swiftctl/console.go` | `swiftctl console` |
| `internal/cli/guest.go` | Helper: resolve SwiftGuest, get pod, guest-id for console path |
| `internal/cli/kubeconfig.go` | Helper: load rest.Config from flags/env |
| `rust/swift-ch-client/src/config.rs` | Add `console_path`; `--console file=` in to_args |
| `rust/swiftletd/src/launch.rs` | Pass console_path when building VmConfig |
| `docs/swiftctl.md` | Command reference, SwiftGuest/pod operations, console transport |
| `docs/releases.md` | Add swiftctl section |
| `Makefile` | Optional `build-swiftctl`; ensure build-go includes swiftctl |
| `.github/workflows/release-stable.yaml` | Build swiftctl, attach to GitHub Release |

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Exec disabled in cluster | Document; suggest SSH via cloud-init |
| Console file not yet created (VM starting) | Fail with clear message or brief retry |
| runPolicy change + pod delete race | Controller reconciles; eventual consistency |

## Migration Plan

1. Add `--console file=` to swift-ch-client and swiftletd; rebuild swiftletd image
2. Implement swiftctl commands in Go
3. Add release integration (workflow, docs)
4. Document in docs/swiftctl.md

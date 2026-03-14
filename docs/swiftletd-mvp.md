# swiftletd MVP

swiftletd is the node-side launcher that runs inside each SwiftGuest pod. It reads the runtime intent, creates a per-guest runtime directory, builds NoCloud seed media (when present), launches Cloud Hypervisor, and reports VM state to the control plane.

## Flow

1. **Read intent** – Load `runtime-intent.json` from `KUBESWIFT_INTENT_PATH` (default `/var/lib/kubeswift/intent/runtime-intent.json`).
2. **Create runtime dir** – Per-guest directory at `KUBESWIFT_RUN_DIR/<guest-id>/` with `seed/` subdir and `ch.sock` socket path.
3. **Build NoCloud** – If seed is present, copy and transform seed ConfigMap into `runtime_dir/seed/` via `swift-seed`.
4. **Lifecycle check** – If `lifecycle=stop`, report Stopped and exit.
5. **Launch CH** – Spawn Cloud Hypervisor with `--api-socket`, `--disk` (root + optional seed), `--memory`, `--cpus`.
6. **Wait for socket** – Poll until CH creates the API socket.
7. **Report running** – Patch SwiftGuest status with `GuestRunning=True`.
8. **Monitor process** – Wait on CH process; on exit, report Stopped (exit 0) or Failed (non-zero).

## Mount paths

The pod must mount:

| Volume       | Mount path                      | Purpose                          |
|--------------|----------------------------------|----------------------------------|
| root-disk    | `/var/lib/kubeswift/disks/root` | Root disk image (PVC)            |
| seed         | `/var/lib/kubeswift/seed`       | Seed ConfigMap (when present)    |
| runtime-intent | `/var/lib/kubeswift/intent`   | Runtime intent ConfigMap          |

The runtime directory is created under `KUBESWIFT_RUN_DIR` (default `/var/lib/kubeswift/run`). Ensure this path is writable (e.g. emptyDir or hostPath).

## Environment

| Variable               | Default                               | Description                          |
|------------------------|----------------------------------------|--------------------------------------|
| `KUBESWIFT_INTENT_PATH`| `/var/lib/kubeswift/intent/runtime-intent.json` | Path to runtime intent JSON   |
| `KUBESWIFT_RUN_DIR`    | `/var/lib/kubeswift/run`               | Base path for per-guest runtime dirs |
| `KUBESWIFT_CH_BINARY`  | `cloud-hypervisor`                     | Cloud Hypervisor binary path          |
| `POD_NAME`             | (from downward API)                   | SwiftGuest name for status reporting |
| `POD_NAMESPACE`        | (from downward API)                   | SwiftGuest namespace for reporting    |

## Cloud Hypervisor requirements

- **Version**: v37.0+ (CLI format: `--api-socket path=`, `--disk path=`, etc.)
- **Binary**: Included in the swiftletd container image at `/usr/local/bin/cloud-hypervisor`
- **Socket**: Unix socket only; no TCP binding

## Seed media

Cloud Hypervisor expects a **disk image** (ISO or vfat) for cloud-init NoCloud, not a directory. The MVP passes the NoCloud **directory** path; some CH builds may accept it. If the guest boots without cloud-init, consider adding ISO generation (e.g. `genisoimage` or a Rust crate) in a future change. See `rust/swift-ch-client/README.md` for details.

## Status reporting

swiftletd patches SwiftGuest status with the `GuestRunning` condition:

- **Running**: `GuestRunning=True`, reason `VmRunning`
- **Stopped**: `GuestRunning=False`, reason `VmStopped`
- **Failed**: `GuestRunning=False`, reason `VmFailed`

Requires in-cluster Kubernetes config (service account) and RBAC: `patch swiftguests/status`. Apply `config/rbac/` in each namespace where SwiftGuests run.

## Container image

Build from `images/swiftletd/Containerfile`:

```bash
docker build -f images/swiftletd/Containerfile rust/ -t swiftletd:local
```

The image includes the swiftletd binary and the Cloud Hypervisor static binary.

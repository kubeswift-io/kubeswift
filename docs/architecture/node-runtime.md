# Node Runtime

**swiftletd** runs inside each SwiftGuest pod as the launcher container. It reads the runtime intent, builds NoCloud seed (when present), and launches Cloud Hypervisor. There is no separate node daemon—swiftletd is per-pod.

## swiftletd flow

| Step | Action |
|------|--------|
| 1 | Read `runtime-intent.json` from `/var/lib/kubeswift/intent/` |
| 2 | Create runtime dir under `KUBESWIFT_RUN_DIR` (default `/var/lib/kubeswift/run`) |
| 3 | If seed present: build NoCloud layout via swift-seed into runtime dir |
| 4 | If `lifecycle=stop`: report Stopped, exit |
| 5 | Spawn Cloud Hypervisor with `--api-socket`, `--disk`, `--memory`, `--cpus` |
| 6 | Poll until CH creates API socket |
| 7 | Patch SwiftGuest `GuestRunning=True` |
| 8 | Wait on CH process; on exit, report Stopped (0) or Failed (non-zero) |

## Mount paths

The pod must mount:

| Volume       | Mount path                      | Purpose                          |
|--------------|----------------------------------|----------------------------------|
| root-disk    | `/var/lib/kubeswift/disks/root` | Root disk image (PVC)            |
| seed         | `/var/lib/kubeswift/seed`       | Seed ConfigMap (when present)    |
| runtime-intent | `/var/lib/kubeswift/intent`   | Runtime intent ConfigMap         |

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

- **Version:** v37.0+ (CLI format: `--api-socket path=`, `--disk path=`, etc.)
- **Binary:** Included in the swiftletd container image at `/usr/local/bin/cloud-hypervisor`
- **Socket:** Unix socket only; no TCP binding

## Status reporting

swiftletd patches SwiftGuest status with the `GuestRunning` condition:

- **Running:** `GuestRunning=True`, reason `VmRunning`
- **Stopped:** `GuestRunning=False`, reason `VmStopped`
- **Failed:** `GuestRunning=False`, reason `VmFailed`

Requires RBAC: `patch swiftguests/status`. Apply `config/rbac/` in each namespace where SwiftGuests run.

## Related docs

- [swiftletd MVP](../swiftletd-mvp.md) — Original design notes
- [Seed rendering](../seed-rendering.md) — NoCloud control-plane vs node flow
- [Operator checklist](../operator/operator-checklist-ubuntu-x86_64.md) — Host prerequisites including `/dev/kvm`

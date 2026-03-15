# swiftctl

swiftctl is the canonical KubeSwift operator CLI for SwiftGuest operability. It provides lifecycle commands (start, stop, restart) and console access.

## Installation

**From source:**
```bash
go install github.com/projectbeskar/kubeswift/cmd/swiftctl@latest
```

**From release:** Download the `swiftctl` binary from the [GitHub Release](https://github.com/projectbeskar/kubeswift/releases) for your platform.

**From build:**
```bash
make build-go
# Binary: ./swiftctl (in repo root)
```

## Commands

### Lifecycle

| Command | Description |
|---------|-------------|
| `swiftctl start <guest>` | Set `spec.runPolicy=Running`, delete pod if exists. Controller recreates pod with `lifecycle=start`; swiftletd launches the VM. |
| `swiftctl stop <guest>` | Set `spec.runPolicy=Stopped`, delete pod. Controller recreates pod with `lifecycle=stop`; swiftletd exits without launching. |
| `swiftctl restart <guest>` | Delete pod (requires `runPolicy=Running`). Controller recreates pod; VM restarts. |

### Console

| Command | Description |
|---------|-------------|
| `swiftctl console <guest>` | Stream VM serial/console output. Execs into the launcher pod and runs `tail -f` on the VM console file. Requires guest phase=Running. Use Ctrl+C to exit. |

## Global flags

| Flag | Description |
|------|-------------|
| `-n`, `--namespace` | Kubernetes namespace (default: default) |
| `--kubeconfig` | Path to kubeconfig (default: KUBECONFIG or ~/.kube/config) |
| `--context` | Kubernetes context |
| `-v`, `--version` | Print version and exit |

## Examples

```bash
# Start a guest
swiftctl start sample
swiftctl -n myns start my-guest

# Stop a guest
swiftctl stop sample

# Restart a guest
swiftctl restart sample

# Attach to VM console
swiftctl console sample
swiftctl -n myns console my-guest
```

## Console transport

swiftctl console uses **exec + tail** of the VM console file. It does not use port-forward, websocket, or any custom transport:

1. swiftletd passes `--console file=<path>` to Cloud Hypervisor when launching the VM
2. VM serial/virtio-console output is written to `/var/lib/kubeswift/run/<namespace>-<name>/console.log`
3. swiftctl execs into the launcher container and runs `tail -f` on that file, streaming to stdout

**Prerequisites:** The guest must be Running. Clusters that restrict `pods/exec` will not support console access; use SSH via cloud-init as an alternative.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Error (guest not found, pod not found, patch/delete failed, etc.) |

Errors are printed to stderr.

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
| `swiftctl console <guest>` | Attach to VM serial console for interactive keyboard access. Execs into the launcher pod and connects via socat to the serial socket. Requires guest phase=Running. Use Ctrl+C to exit. |

### Debug

| Command | Description |
|---------|-------------|
| `swiftctl debug <guest>` | Run diagnostics: runtime dir contents, serial socket status, Cloud Hypervisor process and args. Use to troubleshoot console or boot issues. |
| `swiftctl debug <guest> --shell` | Drop into an interactive shell in the launcher container. |

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

# Troubleshoot (runtime dir, CH process, serial socket)
swiftctl debug sample
swiftctl debug sample --shell
```

## Console transport

swiftctl console uses **exec + socat** for interactive serial console access. It does not use port-forward, websocket, or any custom transport:

1. swiftletd passes `--serial socket=<path>` to Cloud Hypervisor when launching the VM
2. Cloud Hypervisor creates a Unix socket at `/var/lib/kubeswift/run/<namespace>-<name>/serial.sock`
3. swiftctl execs into the launcher container and runs `socat -,crnl UNIX-CONNECT:<path>` for bidirectional keyboard access

**Prerequisites:** The guest must be Running. The swiftletd image includes socat. Clusters that restrict `pods/exec` will not support console access; use SSH via cloud-init as an alternative.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Error (guest not found, pod not found, patch/delete failed, etc.) |

Errors are printed to stderr.

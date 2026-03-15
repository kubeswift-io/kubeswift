# swift-ch-client

Cloud Hypervisor API client for KubeSwift. Spawns the `cloud-hypervisor` process and connects to its Unix socket API.

## Cloud Hypervisor version

Tested with Cloud Hypervisor v37.0+. CLI arguments (`--api-socket`, `--disk`, `--memory`, `--cpus`) follow the format documented at https://cloudhypervisor.org/docs/prologue/commands.

## Seed media (cloud-init NoCloud)

Cloud Hypervisor expects a **disk image** (ISO or vfat) for the second `--disk` argument, not a directory. The cloud-init NoCloud datasource typically uses:

- **ISO**: Volume label `cidata` (or `CIDATA`) with `user-data`, `meta-data`, optionally `network-config`
- **vfat**: Same layout, vfat filesystem with `cidata` label

**Implementation**: swiftletd builds the NoCloud directory via `swift-seed`, then creates `seed.iso` with `genisoimage` (volume label `cidata`). The ISO path is passed as the second `--disk` argument.

## Environment

- `KUBESWIFT_CH_BINARY`: Override the Cloud Hypervisor binary (default: `cloud-hypervisor`)

## Security

Uses local Unix sockets only. No TCP binding. The API socket path is in the per-guest runtime directory.

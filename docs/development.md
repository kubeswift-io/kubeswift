# Development Guide

This guide covers building, testing, and contributing to KubeSwift.

## Repository structure

```
api/
  image/v1alpha1/       SwiftImage types
  kernel/v1alpha1/      SwiftKernel types
  seed/v1alpha1/        SwiftSeedProfile types
  swift/v1alpha1/       SwiftGuest, SwiftGuestClass types
  gpu/v1alpha1/         SwiftGPUProfile, SwiftGPUNode types
  shared/               Common types

cmd/
  swiftctl/             CLI (cobra commands)
  controller-manager/   Main entry point
  webhook-server/       Admission webhook entry point

internal/
  controller/swiftguest/   SwiftGuest controller
  controller/swiftimage/   SwiftImage controller
  controller/swiftkernel/  SwiftKernel controller
  controller/swiftgpu/     SwiftGPU controller (allocation, VFIO, FM)
  runtimeintent/           VM launch spec builder
  resolved/                Ref resolution and merge logic
  seed/                    cloud-init ConfigMap builder
  scheme/                  Scheme registration (all API groups)
  cli/                     CLI helpers (GuestResolver, etc.)

rust/
  Cargo.toml            Workspace manifest
  swiftletd/            VM launcher (all boot paths)
  swift-runtime/        RuntimeDir management
  swift-ch-client/      Cloud Hypervisor spawn + HTTP API client
  swift-qemu-client/    QEMU spawn + QMP client
  swift-seed/           NoCloud ISO builder

build/
  kernels/
    faas-minimal/       Buildroot tree for faas-minimal kernel profile

images/
  swiftletd/Containerfile    (includes qemu-system-x86_64 + OVMF)
  swiftletd/scripts/
    network-init.sh          Bridge/tap setup (init container)
    gpu-init.sh              VFIO bind + FM partition activate (init container)
    launcher-entrypoint.sh   Starts dnsmasq, execs swiftletd

config/
  crd/bases/            Generated CRD YAML (source of truth)
  samples/              Sample manifests
  rbac/                 RBAC rules

charts/kubeswift/       Helm chart
  crds/                 CRD copies (must be kept in sync with config/crd/bases/)

test/smoke/             Smoke test
test/gpu/               GPU passthrough smoke test
```

## Prerequisites

| Tool | Version | Purpose |
|------|---------|---------|
| Go | 1.21+ | controller-manager, swiftctl |
| Rust | stable | swiftletd, swift-ch-client, swift-qemu-client |
| Docker or Podman | — | Building images |
| kubectl | — | Deploy and test |
| kind or minikube | 1.28+ | Local cluster |
| Helm | 3+ | Chart packaging |
| controller-gen | matching go.mod | CRD generation |

## Build commands

### Go

```bash
make build           # build all Go binaries
make build-go        # same (alias)
go build ./...       # build without Makefile
go test ./...        # run all Go tests
```

### Rust

```bash
cd rust
cargo build --release    # build all Rust crates
cargo test               # run all Rust tests
cargo build -p swiftletd # build one crate
```

### Container images

```bash
make build-images    # build controller-manager and swiftletd images
make push-images     # push to ghcr.io/projectbeskar/kubeswift/
```

## Deploy

```bash
make deploy
```

This runs:
1. `controller-gen` to regenerate CRDs from Go types
2. `kubectl apply -k config/crd` and waits for CRDs to be Established
3. Deploys the controller-manager

For local kind/minikube:

```bash
make build-images
make load-images    # loads images into kind/minikube
make deploy
```

## CRD workflow

**After any change to Go types in `api/`:**

```bash
make generate
cp config/crd/bases/*.yaml charts/kubeswift/crds/
make deploy
```

The Kubernetes API server silently drops unknown fields if the CRD schema is out of sync with Go types. Never skip this step.

`controller-gen` reads `+kubebuilder:` marker comments in the Go types to generate:
- CRD YAML with OpenAPI v3 schema in `config/crd/bases/`
- `zz_generated.deepcopy.go` DeepCopy implementations

The Helm chart CRDs at `charts/kubeswift/crds/` must be kept in sync with `config/crd/bases/` manually.

## Testing

### Smoke test

```bash
make smoke-test
```

Runs `test/smoke/boot-test.sh`. Success criteria:
- SwiftImage reaches `phase=Ready`
- SwiftGuest reaches `phase=Running` with `GuestRunning=True`
- `status.network.primaryIP` is populated

Custom timeouts:

```bash
./test/smoke/boot-test.sh --timeout-image 15 --timeout-guest 5 --timeout-network 5
```

### Unit tests

```bash
go test ./...        # Go unit tests
cargo test           # Rust unit tests
```

### SwiftKernel quick test

```bash
kubectl label node <node> kubeswift.io/kernel-node=true
kubectl apply -f config/samples/swiftkernel-faas.yaml
kubectl get swiftkernel faas-minimal -w    # wait for Ready

kubectl apply -f config/samples/swiftguest-faas.yaml
kubectl get swiftguest faas-test -w        # wait for Running + primaryIP
```

### GPU quick test (requires GPU hardware)

```bash
kubectl label node <node> kubeswift.io/gpu-node=true
kubectl get swiftgpunode <node> -o yaml    # verify discovery

kubectl apply -f config/samples/swiftgpuprofile-pcie.yaml
kubectl apply -f config/samples/swiftguest-gpu.yaml
kubectl get swiftguest gpu-test -w

swiftctl ssh gpu-test -- nvidia-smi
```

## Design principles

When making changes, follow these:

1. **Minimal changes** — One fix at a time, verified with real cluster output. Do not combine unrelated changes.
2. **No speculative fixes** — Diagnose with actual logs before patching. Never assume a fix worked without confirming output.
3. **No silent failures** — Status fields must reflect real system state. Never drop errors silently.
4. **Verified before merged** — Always confirm with real cluster output before considering a fix complete.
5. **Cloud Hypervisor first** — CH is the default runtime. QEMU is only introduced when GPU hardware requires it.
6. **Kubernetes-native** — Everything must be observable via `kubectl`. Status fields must be accurate.

## Adding a new controller

1. Create `internal/controller/<name>/controller.go` following existing patterns
2. Register the controller in `cmd/controller-manager/main.go`
3. Add RBAC rules to `config/rbac/role.yaml`
4. Add scheme registration in `internal/scheme/` if a new API group is introduced
5. Run `make generate` if new CRD types are added
6. Copy CRDs: `cp config/crd/bases/*.yaml charts/kubeswift/crds/`

## Adding a new API type

1. Create `api/<group>/v1alpha1/` with `types_<name>.go`, `groupversion_info.go`, `doc.go`
2. Add `+kubebuilder:` marker comments for all fields
3. Register the scheme in `internal/scheme/scheme.go`
4. Run `make generate` to produce CRD YAML and DeepCopy
5. Copy generated CRDs to `charts/kubeswift/crds/`
6. Add RBAC rules for the new resource

## Adding a new Rust crate

1. Create `rust/<crate-name>/` with `Cargo.toml` and `src/lib.rs`
2. Add to `rust/Cargo.toml` workspace members:
   ```toml
   [workspace]
   members = ["swiftletd", "swift-ch-client", "swift-qemu-client", "swift-runtime", "swift-seed", "<crate-name>"]
   ```
3. Add dependency to crates that use it
4. Use `anyhow` for error handling in applications, `thiserror` in libraries
5. Use `tokio` as the async runtime
6. Use `serde_json::json!` for JSON — not `format!`

## Debugging a SwiftGuest

```bash
# Check controller logs
kubectl -n kubeswift-system logs deployment/kubeswift-controller-manager -f

# Check launcher pod logs
swiftctl logs <guest> -f

# Check pod annotations (where swiftletd reports status)
kubectl get pod <pod-name> -o jsonpath='{.metadata.annotations}' | jq .

# Inspect runtime directory inside pod
swiftctl debug <guest>

# Interactive shell in launcher container
swiftctl debug <guest> --shell

# Inside debug shell: inspect runtime directory
ls -la /var/lib/kubeswift/run/default-<guestname>/
cat /var/lib/kubeswift/intent/runtime-intent.json

# Check hypervisor process
cat /proc/<pid>/cmdline | tr '\0' ' '

# Test serial socket manually
socat -,raw,echo=0 UNIX-CONNECT:/var/lib/kubeswift/run/default-<guestname>/serial.sock
```

## Image pipeline debugging

```bash
# Check import job
kubectl get jobs -l swift.kubeswift.io/image=<name>
kubectl logs job/<import-job-name>

# Check SwiftImage status
kubectl get swiftimage <name> -o yaml
kubectl describe swiftimage <name>
```

## Known version constraints

| Component | Version | Notes |
|-----------|---------|-------|
| Cloud Hypervisor | v51.1 | Current production version |
| rust-hypervisor-firmware | v0.5.0 | PVH ELF loader via `--kernel`, not `--firmware` |
| faas-minimal kernel tag | `6.6.1` | Use this. `6.6.0` has broken networking |
| ORAS | v1.3.1 | Used in SwiftKernel pull jobs |
| Guest OS (disk boot) | Ubuntu Focal 20.04 | Noble (24.04) incompatible with rust-hypervisor-firmware |

## Commit guidelines

- One logical change per commit
- Include real cluster output in PR description when fixing runtime behavior
- CRD changes always require `make generate` + copy to charts + redeploy
- Rust workspace changes: add files explicitly to `git add` (cargo outputs are in `target/` which is gitignored, but Cargo.lock changes matter)

## Prometheus metrics

swiftletd exposes these metrics (read by the controller-manager Prometheus endpoint):

| Metric | Type | Description |
|--------|------|-------------|
| `kubeswift_guest_running_total` | Gauge | Number of currently running guests |
| `kubeswift_vm_boot_seconds` | Histogram | VM boot duration |
| `kubeswift_vm_failures_total` | Counter | VM launch failures |
| `kubeswift_image_import_seconds` | Histogram | Image import duration |

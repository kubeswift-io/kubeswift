# Build

## Binaries

```bash
make build          # Go + Rust
make build-go      # controller-manager, swiftctl
make build-rust    # swiftletd, swift-runtime, swift-seed, swift-ch-client
```

Go output: `./cmd/controller-manager/`, `./cmd/swiftctl/`. Rust output: `rust/target/`.

## Container images

```bash
make build-images
```

Builds controller-manager and swiftletd. Default tag: `ghcr.io/kubeswift-io/kubeswift/<component>:latest`.

```bash
make build-controller-image   # controller-manager only
make build-swiftletd-image    # swiftletd only
make build-images IMAGE_TAG=v0.1.0
```

## Generate (CRDs, deepcopy)

```bash
make generate
```

Requires `controller-gen` (controller-tools).

## Makefile targets

| Target | Description |
|--------|-------------|
| `build` | Go + Rust |
| `build-go` | Go binaries |
| `build-rust` | Rust crates |
| `build-images` | Both container images |
| `generate` | CRDs, deepcopy |
| `print-version` | Version metadata |

Release targets: [releases](../releases.md).

[Local cluster](../install/local-cluster.md)

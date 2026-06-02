# KubeSwift Releases

KubeSwift uses three release types: **dev** (main branch), **RC** (release candidates), and **stable**. All produce OCI images and Helm chart; CI runs on push/tag.

## Release types (dev / RC / stable)

| Type | Trigger | Image tag | Chart version | Use case |
|------|---------|-----------|---------------|----------|
| **Dev** | Push to `main` | `sha-<shortsha>` | `0.0.0-dev.<shortsha>` | Bleeding edge; e.g. `0.0.0-dev.a1b2c3d` |
| **RC** | Tag `v*.*.*-rc.*` | `vX.Y.Z-rc.N` | `X.Y.Z-rc.N` | Pre-release; e.g. `0.1.0-rc.1` |
| **Stable** | Tag `v*.*.*` (no `-rc`) | `vX.Y.Z` | `X.Y.Z` | Production; e.g. `0.1.0` |

**Install examples:**

```bash
# Dev (latest main)
helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift --version 0.0.0-dev.a1b2c3d -n kubeswift-system --create-namespace

# Stable
helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift --version 0.1.0 -n kubeswift-system --create-namespace
```

## CI workflows

| Workflow | Trigger |
|----------|---------|
| `release-dev` | Push to `main` |
| `release-rc` | Tag `v*.*.*-rc.*` |
| `release-stable` | Tag `v*.*.*` (excluding `-rc`) |

Each workflow builds images, pushes to ghcr.io, packages the Helm chart, and pushes to OCI (`oci://ghcr.io/projectbeskar/charts` — parent repo; Helm appends chart name). `release-stable` also creates a GitHub Release. See [Helm OCI](install/helm-oci.md#push-vs-install-reference) for push vs install reference.

### Adding a NEW image to the build/push matrix — one-time publicize step

New ghcr container packages are created **private** by default (GitHub's
default for org packages), and **GitHub provides no REST API to change
package visibility** — the release workflow cannot automate it. After the
first push of a new image, a maintainer must publicize the package **once**:

> `https://github.com/orgs/projectbeskar/packages/container/<package>/settings`
> → **Danger Zone → Change visibility → Public** (confirm by typing the name).

Otherwise pods pulling the image stick at `ImagePullBackOff` / `401
Unauthorized`. The existing public packages (`controller-manager`,
`swiftletd`, `gpu-discovery`, `migration-stunnel`) each went through this
once. (Alternatively, deploy an `imagePullSecret` — but publicizing matches
the other packages.) Surfaced by the Phase 3c PR 5 walkthrough (TFU #26).

## Manual release

```bash
# Dev (from main)
make release-dev

# RC (checkout tag first)
git checkout v0.1.0-rc.1
make release-rc

# Stable
git checkout v0.1.0
make release-stable
```

Requires `docker login` to ghcr.io.

## Version stamping

Binaries and images include VERSION, GIT_COMMIT, BUILD_DATE. Shown in `--version` output, logs, OCI labels, chart metadata.

```bash
make print-version
```

## swiftctl

swiftctl is the operator CLI for SwiftGuest lifecycle and console access. It is built with `make build-go` and included in stable GitHub Releases as a downloadable binary.

| Obtain | Command |
|--------|---------|
| From source | `go install github.com/projectbeskar/kubeswift/cmd/swiftctl@latest` |
| From build | `make build-go` → `./swiftctl` |
| From release | Download from [GitHub Releases](https://github.com/projectbeskar/kubeswift/releases) |

Version stamping: `swiftctl --version` prints the same VERSION and GIT_COMMIT as controller-manager and swiftletd. See [swiftctl](swiftctl.md) for usage.

## Makefile targets

| Target | Description |
|--------|-------------|
| `release-dev` | Build, push images + chart (dev) |
| `release-rc` | Build, push images + chart (RC tag) |
| `release-stable` | Build, push images + chart + GitHub Release |
| `push-images` | Push images only |
| `package-chart` | Package Helm chart |
| `push-chart` | Push chart to OCI |
| `print-version` | Print version info |

[Helm OCI](install/helm-oci.md) · [Build](developer/build.md)

# KubeSwift Releases

This document describes the release workflow, versioning, and how to produce releases.

## Version stamping

All binaries (controller-manager, swiftletd) are built with version metadata:

- **Version**: Semantic version or `0.0.0-dev.<shortsha>` for dev builds
- **Git commit**: Full SHA
- **Build date**: ISO 8601 UTC

This metadata appears in:

- Binary `--version` output
- Startup logs
- OCI image labels (`org.opencontainers.image.version`, `org.opencontainers.image.revision`)
- Chart metadata
- GitHub Release notes

## Version helpers

| Script | Purpose |
|--------|---------|
| `hack/version.sh` | Emits VERSION, IMAGE_TAG, CHART_VERSION, GIT_COMMIT, etc. Source or eval to use. |
| `hack/chart-version.sh` | Returns chart version for dev, rc, or stable. |

```bash
# Print version info
make print-version

# Or directly
eval $(./hack/version.sh)
echo $VERSION $IMAGE_TAG $CHART_VERSION
```

## Release types

| Type | Trigger | Image tag | Chart version | Workflow |
|------|---------|-----------|---------------|----------|
| **Dev** | Push to `main` | `sha-<shortsha>` | `0.0.0-dev.<shortsha>` | `release-dev.yaml` |
| **RC** | Tag `v*.*.*-rc.*` | `vX.Y.Z-rc.N` | `X.Y.Z-rc.N` | `release-rc.yaml` |
| **Stable** | Tag `v*.*.*` (no `-rc`) | `vX.Y.Z` | `X.Y.Z` | `release-stable.yaml` |

## Makefile targets

| Target | Description |
|--------|-------------|
| `build-images` | Build controller-manager and swiftletd images (with version stamping) |
| `push-images` | Push images to registry (requires `docker login` to ghcr.io) |
| `package-chart` | Package Helm chart (uses `hack/chart-version.sh dev` by default) |
| `push-chart` | Push chart to OCI registry |
| `release-dev` | Build, push images + chart (dev version) |
| `release-rc` | Build, push images + chart (requires RC tag) |
| `release-stable` | Build, push images + chart + GitHub Release (requires stable tag) |
| `print-version` | Print version info from `hack/version.sh` |

## Local release (manual)

For manual releases, ensure you are on the correct ref and logged in:

```bash
# Dev (from main)
make release-dev

# RC (from tag v0.1.0-rc.1)
git checkout v0.1.0-rc.1
make release-rc

# Stable (from tag v0.1.0)
git checkout v0.1.0
make release-stable
```

For `release-stable`, the Makefile prints a reminder to create the GitHub Release; CI does this automatically when the workflow runs on tag push.

## CI workflows

GitHub Actions run automatically:

- **release-dev**: On every push to `main`
- **release-rc**: On tag push matching `v*.*.*-rc.*`
- **release-stable**: On tag push matching `v*.*.*` (excluding `-rc`)

See [deploy.md](deploy.md) for install instructions.

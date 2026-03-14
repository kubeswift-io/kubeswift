## Context

KubeSwift has local build and deploy flows: `make build-images`, `make deploy`, `make load-images` for kind/minikube. Images are built from `images/controller-manager/Containerfile` and `images/swiftletd/Containerfile`. Webhooks are served by the controller-manager when `--webhook-enabled=true`; there is no separate webhook binary or image. Kustomize manifests live in `config/` (namespace, manager, daemonset, webhook overlay). Remote clusters need published OCI artifacts. This design adds OCI-native release and distribution via GHCR and Helm.

## Locked OCI Distribution Strategy

**This is the sole release model for remote clusters.** No competing distribution approaches (raw manifests from releases, alternate registries, alternate chart repos) are supported. The primary install path for remote clusters is: OCI images in GHCR + OCI Helm chart in GHCR. Local-cluster workflows (`make build-images`, `make deploy`, `make load-images`) remain the sole path for kind/minikube and are unchanged.

## Goals / Non-Goals

**Goals:**

- Publish controller-manager and swiftletd images to GHCR (webhooks served by controller-manager; no separate webhook image)
- Add versioning for dev, RC, and stable releases
- Add an installable Helm chart and publish it to GHCR as OCI
- Add GitHub Actions workflows for dev, RC, and stable publishing
- Wire image tags and chart versions to the versioning model
- Keep local-cluster workflows intact

**Non-Goals:**

- Migration, snapshots, or new runtime features
- Changing CRD semantics except where packaging requires it
- Replacing local-cluster workflows
- Adding a second distribution format (raw YAML releases, alternate registries, etc.)

## Decisions

### 1. Image names (exact)

| Component | Full image name |
|-----------|-----------------|
| controller-manager | `ghcr.io/projectbeskar/kubeswift/controller-manager` |
| swiftletd | `ghcr.io/projectbeskar/kubeswift/swiftletd` |

**Registry:** `ghcr.io/projectbeskar/kubeswift`. No alternate registries for published releases.

### 2. Image tag conventions (exact)

| Release type | Trigger | Tag format | Example |
|--------------|---------|------------|---------|
| Dev | Push to `main` | `sha-<short-sha>` (7-char git SHA) | `sha-a1b2c3d` |
| RC | Tag `v*.*.*-rc.*` | Exact tag as pushed | `v0.1.0-rc.1` |
| Stable | Tag `v*.*.*` (no `-rc`) | Exact tag as pushed | `v0.1.0` |

**Rules:** Immutable tags only. No `latest` for published images. Local `make build-images` may use `latest` or `IMAGE_TAG`; that is separate from OCI publishing.

### 3. Chart location and naming (exact)

| Attribute | Value |
|-----------|-------|
| Chart name | `kubeswift` |
| OCI chart location | `oci://ghcr.io/projectbeskar/charts/kubeswift` |
| Install command | `helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift --version <version>` |

**Chart versioning:** Chart version SHALL match the release:
- Dev: `0.0.0-dev.<short-sha>` (e.g. `0.0.0-dev.a1b2c3d`)
- RC: `X.Y.Z-rc.N` (e.g. `0.1.0-rc.1`)
- Stable: `X.Y.Z` (e.g. `0.1.0`)

**Chart image defaults:** `values.yaml` SHALL define `controllerManager.image.registry`, `controllerManager.image.tag`, and likewise for `swiftletd`. Default registry: `ghcr.io/projectbeskar/kubeswift`. Default tag: matches chart version (e.g. chart `0.1.0` → tag `v0.1.0`). Operators MAY override for air-gapped installs.

**CRDs:** Bundled in chart at `crds/` (no Helm dependency). Chart includes webhook resources as an optional install mode (default: minimal, no webhook); when enabled, controller-manager runs with `--webhook-enabled=true` and cert mount.

**Namespace:** Namespace creation is optional. Operators MAY use `helm install --create-namespace`; the chart MAY omit a namespace template or make it conditional via values.

### 4. GitHub Actions workflow responsibilities (exact)

| Workflow file | Trigger | Responsibilities |
|---------------|---------|-----------------|
| `.github/workflows/publish-dev.yaml` | `push` to `main` | Build controller-manager, swiftletd; push to GHCR with tag `sha-<short-sha>`; build chart with version `0.0.0-dev.<short-sha>`; push chart to `oci://ghcr.io/projectbeskar/charts/kubeswift` |
| `.github/workflows/publish-rc.yaml` | `push` tag `v*.*.*-rc.*` | Build controller-manager, swiftletd; push with tag = git tag (e.g. `v0.1.0-rc.1`); build chart with version = tag sans `v` (e.g. `0.1.0-rc.1`); push chart. No GitHub Release. |
| `.github/workflows/publish-release.yaml` | `push` tag `v*.*.*` (exclude tags containing `-rc`) | Build controller-manager, swiftletd; push with tag = git tag; build and push chart; create GitHub Release with install instructions and release notes |

**No other workflows** publish images or charts. CI (`.github/workflows/ci.yaml`) remains for build/test only; it does not push to GHCR. Use `oci://ghcr.io/projectbeskar/charts/kubeswift` consistently for chart references.

**GitHub Release (stable only):** SHALL include chart install command and image references. OCI chart is the primary artifact; Release documents it.

### 5. config/ manifests → Helm chart templates

**Decision:** The Helm chart SHALL be derived from existing `config/` manifests. Mapping:

| config/ source | Chart template |
|----------------|----------------|
| `config/namespace/namespace.yaml` | `templates/namespace.yaml` (optional; or document `helm install --create-namespace`) |
| `config/manager/serviceaccount.yaml` | `templates/controller-manager/serviceaccount.yaml` |
| `config/manager/controller-manager-rbac.yaml` | `templates/controller-manager/rbac.yaml` |
| `config/manager/deployment.yaml` | `templates/controller-manager/deployment.yaml` (webhook conditionals inlined) |
| `config/manager/service.yaml` | `templates/controller-manager/service.yaml` |
| `config/daemonset/*` | `templates/swiftletd/*` |
| `config/webhook/*` (Certificate, Issuer, ValidatingWebhook, MutatingWebhook) | `templates/webhook/*` (conditional; enabled via values) |
| `config/crd/bases/*` | `crds/` (chart CRDs) |

**Webhook conditionals:** Do NOT use a separate `deployment-patch.yaml`. Put webhook conditionals directly in `templates/controller-manager/deployment.yaml`: when `webhook.enabled` is true, add `--webhook-enabled=true` to args, add volumeMounts for cert, add volumes for cert Secret. Reference `config/overlays/webhook/deployment-patch.yaml` for the logic to inline.

**Template parameterization:** Image tags, registry overrides, replicas, resources SHALL be values. The chart SHALL support `--set` overrides for air-gapped or custom registry installs.

**Rationale:** Single source of truth; config/ remains the canonical manifest layout. Chart templates are generated or hand-maintained from config/ to avoid drift.

### 6. Local workflows preserved

**Decision:** `make build-images`, `make deploy`, `make load-images` SHALL remain unchanged. Local images MAY use `latest` or `IMAGE_TAG`. No removal or replacement of local-cluster workflows.

**Rationale:** Developers and smoke tests rely on local flows; OCI distribution is additive for remote clusters.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| GHCR auth in Actions | Use `GITHUB_TOKEN` or `registry` permissions; document for forks |
| Tag immutability | Use unique tags (sha, version); avoid overwriting |
| Chart/values drift | Derive from config/; add CI check for sync |
| Webhook optional in chart | Chart MAY include webhook optionally; minimal path uses controller-manager only; webhooks served in-process by controller-manager |

### 7. Workflow file plans (exact)

**publish-dev.yaml:**
- Trigger: `push` to `main` (branches)
- Jobs: build controller-manager, swiftletd; push to `ghcr.io/projectbeskar/kubeswift/*` with tag `sha-${{ github.sha }}` (use 7-char short SHA); build Helm chart with version `0.0.0-dev.${{ github.sha }}` (use 7-char short SHA); push chart to `oci://ghcr.io/projectbeskar/charts/kubeswift`
- No webhook-server build or push

**publish-rc.yaml:**
- Trigger: `push` tag `v*.*.*-rc.*`
- Jobs: build controller-manager, swiftletd; push with tag = git tag; build chart with version = tag sans `v`; push chart to `oci://ghcr.io/projectbeskar/charts/kubeswift`
- No GitHub Release

**publish-release.yaml:**
- Trigger: `push` tag `v*.*.*` (exclude `-rc`)
- Jobs: build controller-manager, swiftletd; push with tag = git tag; build and push chart; create GitHub Release with install command `helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift --version X.Y.Z --create-namespace`

### 8. Chart layout plan (exact)

```
charts/kubeswift/
├── Chart.yaml
├── values.yaml
├── .helmignore
├── crds/
│   └── (from config/crd/bases/)
└── templates/
    ├── namespace.yaml          # optional; or omit and document --create-namespace
    ├── controller-manager/
    │   ├── deployment.yaml      # webhook conditionals inlined (args, volumeMounts, volumes)
    │   ├── service.yaml
    │   ├── serviceaccount.yaml
    │   └── rbac.yaml
    ├── swiftletd/
    │   ├── daemonset.yaml
    │   └── serviceaccount.yaml
    └── webhook/                 # conditional on values.webhook.enabled
        ├── certificate.yaml
        ├── issuer.yaml
        ├── validating-webhook.yaml
        └── mutating-webhook.yaml
```

No `deployment-patch.yaml`. Webhook Service is in `config/manager/service.yaml` and included in controller-manager templates.

### 9. Docs update plan

**docs/deploy.md:** Add section "Install from OCI (remote clusters)" with:
- Prerequisites: Helm 3+, cluster access
- Install command: `helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift --version <version> --create-namespace`
- Version selection: dev `0.0.0-dev.<shortsha>`, RC `X.Y.Z-rc.N`, stable `X.Y.Z`
- Optional webhook: `--set webhook.enabled=true` (requires cert-manager)
- Keep existing local build/deploy/load-images sections

**docs/repo-layout.md:** Update config layout to clarify webhook is served by controller-manager; remove or correct "webhook-server Deployment" if present; add `charts/kubeswift/` to directory table; add OCI chart reference `oci://ghcr.io/projectbeskar/charts/kubeswift`

## Migration Plan

1. Add Helm chart scaffold at `charts/kubeswift/`
2. Add workflow for dev image publishing (push to main)
3. Add workflow for RC (tag push)
4. Add workflow for stable (tag push + GitHub Release)
5. Add chart OCI publish to each workflow
6. Document install in README or docs
7. **Rollback:** Revert workflows; local flows unaffected

## Resolved (no competing approaches)

- **CRDs:** Bundled in chart at `crds/`. No Helm dependency.
- **Webhook:** Served by controller-manager when `--webhook-enabled=true`. No separate webhook image. Chart includes webhook resources as optional (values-driven); minimal install omits them.

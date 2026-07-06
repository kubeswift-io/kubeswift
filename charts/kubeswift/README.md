# KubeSwift Helm chart

Installs the KubeSwift operator (controller-manager + CRDs) and, opt-in, the
GPU discovery / DRA driver, the multi-cluster UI backend (gateway) + web console,
observability, and admission webhooks.

```bash
helm install kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift \
  --version <version> -n kubeswift-system --create-namespace
```

The chart installs CRDs (`crds/`) on first install only; Helm does **not** upgrade
them — re-apply `config/crd/bases/*.yaml` after a chart upgrade that changes a CRD.

**Prerequisites**

| For | Requirement |
|---|---|
| Core operator | Kubernetes ≥ 1.28 |
| `webhook.enabled`, `migration.mtls.enabled`, `ingress.tlsAuto` | cert-manager installed cluster-side |
| `monitoring.enabled` | Prometheus Operator CRDs (e.g. kube-prometheus-stack); dashboards need a Grafana with the dashboard sidecar |
| `gpuDiscovery.enabled` / `dra.enabled` | nodes labeled `kubeswift.io/gpu-node=true`; DRA also needs CDI enabled in the runtime + the `vfio-pci` module |

**Image tags** — the operator images default to `tag: latest`, which the chart
rewrites to the chart's own `v<appVersion>` for a release (or `sha-<commit>` for a
dev build). Only override for locally-built images. The `kubeswift-ui` image is
released from a separate repo and is **not** chart-derived — pin `ui.image.tag`.
See [`docs/install/helm-oci.md`](../../docs/install/helm-oci.md).

## Common configurations

```bash
# Minimal operator only (default)
helm install kubeswift … 

# Management hub: gateway + web console + self-register this cluster
helm install kubeswift … --set federation.role=hub

# Federated member (edge): mints a join credential; NOTES prints the hub manifest
helm install kubeswift … --set federation.role=edge \
  --set 'federation.edge.operatorGroups={kubeswift-operators}'

# Observability (kube-prometheus-stack)
helm upgrade kubeswift … --set monitoring.enabled=true \
  --set monitoring.serviceMonitor.additionalLabels.release=kube-prometheus-stack \
  --set monitoring.prometheusRule.additionalLabels.release=kube-prometheus-stack
```

`helm upgrade` reuses previously-set values only when you pass none; pass
`--reset-values` to drop old overrides.

## Values

### Core

| Parameter | Description | Default |
|---|---|---|
| `namespace` | Namespace for KubeSwift resources | `kubeswift-system` |
| `createNamespace` | Create the namespace (else use `--create-namespace`) | `false` |
| `controllerManager.image.registry` | Controller image registry | `ghcr.io/kubeswift-io/kubeswift` |
| `controllerManager.image.tag` | Controller image tag (`latest` → `v<appVersion>`) | `latest` |
| `controllerManager.replicas` | Controller replicas | `1` |
| `controllerManager.resources` | Requests/limits | `100m`/`128Mi` … `500m`/`512Mi` |
| `swiftletd.daemonset.enabled` | Standalone swiftletd DaemonSet — leave off; swiftletd runs as the launcher container inside guest pods | `false` |
| `swiftletd.image.registry` | Launcher image registry | `ghcr.io/kubeswift-io/kubeswift` |
| `swiftletd.image.tag` | Launcher image tag (as above) | `latest` |
| `swiftletd.resources` | Requests/limits | `100m`/`128Mi` … `2`/`2Gi` |

### Snapshot backend images

These only set an image reference; no workload is deployed. The controller passes
each to the per-snapshot upload/download Job.

| Parameter | Description | Default |
|---|---|---|
| `snapshotS3.image.registry` / `.tag` | Tier C (S3 / object-storage) snapshot Job image | `ghcr.io/kubeswift-io/kubeswift` / `latest` |
| `snapshotORAS.image.registry` / `.tag` | OCI-registry (ORAS) snapshot Job image | `ghcr.io/kubeswift-io/kubeswift` / `latest` |

### Live migration

| Parameter | Description | Default |
|---|---|---|
| `migrationStunnel.image.registry` / `.tag` | mTLS sidecar image (injected when `migration.mtls.enabled`) | `ghcr.io/kubeswift-io/kubeswift` / `latest` |
| `migration.mtls.enabled` | Provision a per-namespace cert-manager PKI and run the controller with `--migration-mtls-enabled` (per-node leaf certs). Requires cert-manager | `false` |

### GPU

The native backend (`gpuDiscovery`, uses `SwiftGPUProfile`/`SwiftGPUNode`) and the
DRA backend (`dra`, uses `SwiftGuest.spec.gpuResourceClaim`) are independent — a
DRA-only cluster runs `dra.enabled=true` with `gpuDiscovery.enabled=false`.

| Parameter | Description | Default |
|---|---|---|
| `gpuDiscovery.enabled` | Native GPU discovery DaemonSet | `false` |
| `gpuDiscovery.image.registry` / `.tag` | Discovery image | `ghcr.io/kubeswift-io/kubeswift` / `latest` |
| `gpuDiscovery.interval` | Rediscovery interval | `60s` |
| `gpuDiscovery.resources` | Requests/limits | `10m`/`64Mi` … `100m`/`128Mi` |
| `dra.enabled` | Reference DRA GPU driver DaemonSet (`gpu.kubeswift.io`) | `false` |
| `dra.image.registry` / `.tag` | Driver image | `ghcr.io/kubeswift-io/kubeswift` / `latest` |
| `dra.deviceClass.create` | Create the cluster-scoped DeviceClass (`false` if managed out-of-band) | `true` |
| `dra.deviceClass.name` | DeviceClass a ResourceClaim references | `kubeswift-vfio-gpu` |
| `dra.resources` | Requests/limits | `10m`/`64Mi` … `100m`/`128Mi` |

### Federation (multi-cluster)

`role` presets the component toggles; an explicit `gateway.enabled`/`ui.enabled`
still adds that component in any role. See [`docs/ui/gateway.md`](../../docs/ui/gateway.md).

| Parameter | Description | Default |
|---|---|---|
| `federation.role` | `standalone` (single cluster, nothing federation-related) · `hub` (management plane: presets gateway + UI + self-registers this cluster) · `edge` (a federated member) | `standalone` |
| `federation.selfRegister.enabled` | (role=hub) Register this cluster as a local member via the gateway's in-cluster SA — no credential Secret | `true` |
| `federation.selfRegister.displayName` | UI label for the self entry | `""` (release name) |
| `federation.selfRegister.prometheusEndpoint` | Explicit telemetry endpoint for the self entry (else auto-discovered) | `""` |
| `federation.edge.applyMemberRBAC` | (role=edge) Apply the member-RBAC (impersonator + VM-reader + Prometheus-discovery) at install | `true` |
| `federation.edge.operatorGroups` | IdP groups (the impersonated subjects) bound to the VM-reader role on this edge | `[]` |
| `federation.edge.displayName` | UI label the hub shows for this edge | `""` (release name) |

### Gateway (UI backend)

| Parameter | Description | Default |
|---|---|---|
| `gateway.enabled` | Deploy the UI backend gateway (auto-on for `role=hub`) | `false` |
| `gateway.image.registry` / `.tag` | Gateway image | `ghcr.io/kubeswift-io/kubeswift` / `latest` |
| `gateway.replicas` | Gateway replicas | `1` |
| `gateway.port` | Container port | `8080` |
| `gateway.authMode` | End-user auth: `oidc` (verify IdP token, impersonate) · `token` (bearer via TokenReview) · `insecure` (no per-user impersonation — **dev/lab only**) | `insecure` |
| `gateway.oidc.issuerURL` | (authMode=oidc) IdP issuer, reachable from browser **and** gateway | `""` |
| `gateway.oidc.clientID` | Client ID / audience the ID token must carry | `""` |
| `gateway.oidc.usernameClaim` | Claim used as the impersonated username | `email` |
| `gateway.oidc.groupsClaim` | Claim used as the impersonated groups | `groups` |
| `gateway.oidc.usernamePrefix` / `.groupsPrefix` | Mirror the apiserver `--oidc-*-prefix` flags | `""` |
| `gateway.oidc.caSecret` | Secret (in `namespace`) holding a private IdP CA for the JWKS fetch | `""` |
| `gateway.oidc.caKey` | Key within `caSecret` | `ca.crt` |
| `gateway.corsAllowOrigin` | Allowed browser origin (`*` ok for token auth; N/A in `ui.gateway.mode=proxy`) | `*` |
| `gateway.prometheusDiscovery.namespaces` | Member namespaces scanned for an in-cluster Prometheus when `Cluster.spec.prometheusEndpoint` is empty (`[]` disables) | `[monitoring, kube-prometheus-stack, observability, prometheus]` |
| `gateway.service.type` / `.port` | Gateway Service | `ClusterIP` / `8080` |
| `gateway.ingress.enabled` | Create an Ingress for the gateway | `false` |
| `gateway.ingress.className` | `ingressClassName` | `""` |
| `gateway.ingress.host` | Ingress host | `kubeswift-gateway.example.com` |
| `gateway.ingress.tlsAuto.*` | cert-manager TLS shortcut — see [Ingress TLS](#ingress-tls) | disabled |
| `gateway.ingress.annotations` | Extra ingress annotations | `{}` |
| `gateway.ingress.tls` | Raw `tls[]` escape hatch (honored only when `tlsAuto.enabled=false`) | `[]` |
| `gateway.resources` | Requests/limits | `50m`/`64Mi` … `500m`/`256Mi` |

### UI (web console)

The `kubeswift-ui` app talks to the gateway, so enable `gateway.enabled` too (or
point `ui.gateway.url` at an externally reachable gateway).

| Parameter | Description | Default |
|---|---|---|
| `ui.enabled` | Deploy the web console (auto-on for `role=hub`) | `false` |
| `ui.image.repository` | UI image repo (a top-level package, not chart-derived) | `ghcr.io/kubeswift-io/kubeswift-ui` |
| `ui.image.tag` | Published UI tag — **pin a version for production** | `latest` |
| `ui.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `ui.imagePullSecrets` | Pull Secret(s) if the UI package is private | `[]` |
| `ui.replicas` | UI replicas | `1` |
| `ui.gateway.mode` | How the browser reaches the gateway: `proxy` (UI's nginx reverse-proxies to the in-cluster gateway — one origin, no CORS) · `url` (browser calls the gateway directly) | `proxy` |
| `ui.gateway.service` / `.port` | (proxy mode) In-cluster gateway Service name + port | `kubeswift-gateway` / `8080` |
| `ui.gateway.url` | (url mode) Absolute, browser-reachable gateway URL | `""` |
| `ui.oidc.issuer` | Browser login issuer — must match `gateway.oidc.issuerURL` | `""` |
| `ui.oidc.clientId` | Public OIDC client the browser logs in with | `""` |
| `ui.service.type` / `.port` | UI Service | `ClusterIP` / `80` |
| `ui.ingress.enabled` | Create an Ingress for the UI | `false` |
| `ui.ingress.className` | `ingressClassName` | `""` |
| `ui.ingress.host` | Ingress host | `kubeswift-ui.example.com` |
| `ui.ingress.tlsAuto.*` | cert-manager TLS shortcut — see [Ingress TLS](#ingress-tls) | disabled |
| `ui.ingress.annotations` | Extra ingress annotations | `{}` |
| `ui.ingress.tls` | Raw `tls[]` escape hatch | `[]` |
| `ui.resources` | Requests/limits | `10m`/`32Mi` … `200m`/`128Mi` |

### Admission webhook

| Parameter | Description | Default |
|---|---|---|
| `webhook.enabled` | Enable admission webhooks (runs the controller with `--webhook-enabled=true`). Requires cert-manager | `false` |

### Observability

| Parameter | Description | Default |
|---|---|---|
| `monitoring.enabled` | Ship the monitoring resources. Requires the Prometheus Operator CRDs | `false` |
| `monitoring.serviceMonitor.enabled` | ServiceMonitor scraping the controller `/metrics` | `true` |
| `monitoring.serviceMonitor.interval` | Scrape interval | `30s` |
| `monitoring.serviceMonitor.additionalLabels` | Labels for Prometheus instances that select by label (e.g. `release: kube-prometheus-stack`) | `{}` |
| `monitoring.dashboards.enabled` | Grafana dashboards as sidecar-discovered ConfigMaps | `true` |
| `monitoring.dashboards.label` | Sidecar discovery label | `grafana_dashboard` |
| `monitoring.dashboards.namespace` | ConfigMap namespace (empty = KubeSwift namespace) | `""` |
| `monitoring.dashboards.annotations` | Extra annotations, e.g. `grafana_folder: KubeSwift` | `{}` |
| `monitoring.prometheusRule.enabled` | Starter warning-biased alert pack | `true` |
| `monitoring.prometheusRule.additionalLabels` | Labels for Prometheus instances that select rules by label | `{}` |

## Ingress TLS

Both `ui.ingress` and `gateway.ingress` accept a `tlsAuto` block. Set
`tlsAuto.enabled=true` plus **one** issuer and the chart writes the `tls[]` entry
and the cert-manager annotation for you:

| Parameter | Description | Default |
|---|---|---|
| `<ingress>.tlsAuto.enabled` | Emit `tls[]` + the cert-manager annotation | `false` |
| `<ingress>.tlsAuto.clusterIssuer` | cert-manager `ClusterIssuer` name (→ `cert-manager.io/cluster-issuer`) | `""` |
| `<ingress>.tlsAuto.issuer` | Namespaced `Issuer` name (→ `cert-manager.io/issuer`; mutually exclusive with `clusterIssuer`) | `""` |
| `<ingress>.tlsAuto.secretName` | Cert Secret name | `""` (`<host>-tls`) |

Leave `tlsAuto.enabled=false` and hand-write `<ingress>.annotations` +
`<ingress>.tls` for full control.

## See also

- [`docs/install/helm-oci.md`](../../docs/install/helm-oci.md) — install reference and image-tag rules
- [`docs/ui/gateway.md`](../../docs/ui/gateway.md) — gateway, federation roles, auth modes
- [`docs/observability/README.md`](../../docs/observability/README.md) — dashboards and alerts
- [`docs/gpu/dra-allocation.md`](../../docs/gpu/dra-allocation.md) — DRA GPU allocation

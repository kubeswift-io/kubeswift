# Phase 0: read-only reconnaissance

Run 2026-09-24, 22:50–23:15 UTC, from `validation/v0.15.0-candidate` @ `53d74c1`.
**Nothing was changed on any cluster.** Every command was a read (`get`,
`describe`, `helm list`, `helm get values`, events, logs). The only `exec` was
a read-only `test -c /dev/kvm` plus a `grep` of `/proc/cpuinfo`, run inside the
Longhorn CSI plugin pods that already exist (they mount the host `/dev`), to
establish KVM capability per node.

Node hostnames are replaced with `cp-1` / `worker-1` / `worker-2` per cluster
(the mapping is kept locally). Public hostnames, node IPs and MACs are redacted.

## Heads-up for the driver (read these before the Phase 1 go-ahead)

1. **Nothing is in progress. Phase 1 can start.** No namespace was created in
   the last 6 h on any cluster, and no SwiftSnapshot, SwiftRestore or SwiftMigration
   is non-terminal anywhere. The most recent activity was the v0.14.1 fleet upgrade
   (~21:40 UTC today; the helm revisions below), followed on dev by a v0.14.1
   post-upgrade check: guest `val141` and sandbox `val141-sbx` in `default`,
   22:09–22:26 UTC. It has finished and cleaned up. Only Completed
   `node-debugger-*` pods from it remain in `default`.
2. **The Phase 1 `--set` list leaves out `snapshotS3`.** All three clusters pin
   `snapshotS3.image.tag: v0.14.1` in their user values, and the chart still has
   the key (`charts/kubeswift/values.yaml` @ `2d146eb`). With `--reuse-values` and
   only the eight listed `--set`s, `KUBESWIFT_SNAPSHOT_S3_IMAGE` stays at
   `snapshot-s3:v0.14.1`. I will follow the plan as written unless the go-ahead
   adds `--set snapshotS3.image.tag=sha-<sha>`.
3. **The Phase 1 CRD step is needed on every cluster.** The installed
   `swiftsnapshots` has `status.guestSpec` but no `guestSpec.primaryIP`; the
   candidate CRD at `2d146eb` adds it. `swiftsnapshotschedules` also changed since
   v0.14.1 (`git diff --stat v0.14.1 origin/main -- charts/kubeswift/crds/`: 2 files).
4. **ntx is already degraded, before any candidate change. This is the baseline
   for N1–N3.**
   - The CAPI worker guest `capi-udn/ks-udn-md0-sz2ql-bz9zs` gets a new launcher
     pod about every 5 minutes and never starts. Its Longhorn root volume
     (`pvc-5eda465e-…`) is stuck `state: attaching`, `robustness: unknown` on
     worker-1. The attachment ticket is `satisfied: false`, and every pod gets
     `FailedMount … volume … hasn't been attached yet`. `GuestRunning=False` since
     21:32:25Z, which is **before** the v0.14.1 helm upgrade at 21:41:45Z.
   - The CAPI Cluster `ks-udn` is `Available=False` (0/1 control plane available,
     0/1 workers). The CP Machine is `Ready=Unknown`.
   - The CP guest `ks-udn-cp-54klw` is `Running`. Its launcher pod dates from
     2026-09-21T21:59:01Z, has 0 restarts, and still runs `swiftletd:v0.13.14`
     (uid `a5420720-…`). This is the "no restart" reference for N3.
   - The three Longhorn `instance-manager` pods are 3 days old. This looks like
     the known ntx stale-netns Longhorn condition. I did not touch it.
5. **D7: `test/migration/migration-test.sh` is not a live-migration test.** Its
   header says "Phase 1: offline migration". It runs
   `swiftctl migrate … --allow-ip-change` on a guest of class `default` (RWO,
   default SC). Also, on dev:
   - Its worker picker (a jsonpath with a negated filter) does not parse with the
     local `kubectl` v1.32.3: `error parsing jsonpath … unterminated filter`. It
     would then find 0 workers and exit 2.
   - Even when the picker parses, it keeps every node without the control-plane
     *taint*. dev's cp-1 is untainted and KVM-capable, so cp-1 would count as a
     "worker". Sorted by name, it would pair worker-2 with cp-1.
   - It uses `guestClassRef: default`, and no SwiftGuestClass `default` exists on
     dev. `boot-test.sh` creates one and deletes it again on cleanup.
   - It waits for phase `Completed`. The plan says `Succeeded`.
   - It exits with `kubectl uncordon --all`.
   - It hard-codes `~/.ssh/id_ed25519`. That key exists locally and matches the
     seed key.

   To test #653 with a **live** migration, D7 needs a hand-made live
   SwiftMigration on an RWX Block class (as D8 does), or a driver decision.
6. **D5: dev has no default VolumeSnapshotClass.** Neither `longhorn-snapshot-vsc`
   nor `ceph-block-vsc` carries `snapshot.storage.kubernetes.io/is-default-class`.
   `snapshot-test.sh` and `clonestrategy-test.sh` stop with `no default
   VolumeSnapshotClass` unless `--vsclass` is passed, and the make targets pass no
   arguments. Proposal: run the scripts directly with
   `--vsclass longhorn-snapshot-vsc` (it matches the default SC `longhorn`).
   Needs the driver's OK.
7. **D12 needs an interactive OIDC sign-in.** dev's gateway runs
   `authMode: oidc`, and the test needs two users, with and without the Console
   capability. That takes a browser login by the operator; I cannot do it
   unattended.
8. **VAPs expected after Phase 1.** This is a prediction from the templates at
   `2d146eb`, not an observation. All three get
   `kubeswift-launcher-sa-tokenrequest-gate`. `kubeswift-gateway-exec-gate`
   renders only where `kubeswift.gatewayConsole` is true (VAP API served, and
   either an edge with member RBAC or a self-registered hub): **dev (hub) and sov
   (edge), not ntx (standalone)**.
9. **The `--reuse-values` nil-pointer risk from past upgrades does not apply to
   #656.** At `2d146eb`, `deployment.yaml` and `metrics-readers.yaml` guard the
   new `controllerManager.metrics` block with `and .Values.controllerManager.metrics …`.
   Under `--reuse-values` it renders `--metrics-secure=false`, as the plan expects.

---

## 1. Per-cluster inventory

### dev

- **Kubernetes** `v1.34.3+k0s`. 3 nodes, all Ready, **all KVM-capable**.

| Node | Role | kubeswift labels | Taints | Allocatable cpu / mem | /dev/kvm | Notes |
|---|---|---|---|---|---|---|
| cp-1 | control-plane (k0s controller + worker) | — | none (schedulable) | 8 / ~62 GiB | yes | runs the UI pod |
| worker-1 | worker | `basedisk-node=true`, `kernel-node=true` | none | 8 / ~54 GiB | yes | hugepages-2Mi 8 Gi; runs controller + gateway |
| worker-2 | worker | `gpu-node=true`, `kernel-node=true` | none | 8 / ~62 GiB | yes | the GPU node; gpu-discovery + DRA driver; hosts guest `innercp` |

- **Helm release:** `kubeswift` in `kubeswift-system`, chart `kubeswift-0.14.1`,
  app `0.14.1`, revision 42, deployed 2026-09-24 21:40:27 UTC.
- **User-supplied values** (no secrets present; hosts redacted):

```yaml
controllerManager: {image: {tag: v0.14.1}}
dra: {deviceClass: {create: false}, enabled: true, image: {tag: v0.14.1}}
federation: {role: hub}
gateway:
  authMode: oidc
  corsAllowOrigin: https://<redacted UI host>
  enabled: true
  image: {tag: v0.14.1}
  oidc: {clientID: kubeswift-gateway, issuerURL: https://<redacted IdP host>/realms/kubeswift}
gpuDiscovery: {enabled: true, image: {tag: v0.14.1}}
migrationStunnel: {image: {tag: v0.14.1}}
monitoring:
  enabled: true
  prometheusRule: {additionalLabels: {release: monitoring}}
  serviceMonitor: {additionalLabels: {release: monitoring}}
sandboxMaterialize: {image: {tag: v0.14.1}}
snapshotORAS: {image: {tag: v0.14.1}}
snapshotS3: {image: {tag: v0.14.1}}
swiftGuest: {}
swiftletd: {image: {tag: v0.14.1}}
ui:
  enabled: true
  image: {tag: v0.12.4}
  ingress:
    className: nginx
    enabled: true
    host: <redacted UI host>
    tlsAuto: {clusterIssuer: letsencrypt-prod, enabled: true, secretName: kubeswift-ui-tls}
  oidc: {clientId: kubeswift-gateway, issuer: https://<redacted IdP host>/realms/kubeswift}
webhook: {enabled: true}
```

- **Controller-manager:** `ghcr.io/kubeswift-io/kubeswift/controller-manager:v0.14.1`,
  1/1 Ready, 0 restarts, age 74 m. Args: `--leader-elect --webhook-enabled=true`.
  There is no `--metrics-secure` flag at v0.14.1. Env images are all `:v0.14.1`
  (`KUBESWIFT_LAUNCHER_IMAGE`, `_SNAPSHOT_S3_IMAGE`, `_SNAPSHOT_ORAS_IMAGE`,
  `_MIGRATION_STUNNEL_IMAGE`, `_SANDBOX_MATERIALIZE_IMAGE`).
  `KUBESWIFT_BASEDISK_POOL_SIZE=40Gi`.
- **Other kubeswift workloads:** gateway `v0.14.1` 1/1; UI `kubeswift-ui:v0.12.4` 1/1;
  gpu-discovery `v0.14.1` 1/1 DS; kubeswift-dra-driver `v0.14.1` 1/1 DS.

| Component | Installed? | Evidence |
|---|---|---|
| webhook | **yes** | `kubeswift-validating-webhook` (10 webhooks), `kubeswift-mutating-webhook` (3); Certificate `kubeswift-webhook-cert` Ready |
| gateway | **yes** | Deployment `kubeswift-gateway` v0.14.1, `authMode: oidc` |
| UI | **yes** | Deployment `kubeswift-ui` v0.12.4, ingress + cert-manager TLS |
| cert-manager | **yes** | helm `cert-manager-v1.21.1`; ClusterIssuers `kubeswift-selfsigned-issuer`, `letsencrypt-prod` Ready |
| Prometheus Operator | **yes** | helm `kube-prometheus-stack-88.2.0` (operator v0.93.0); ServiceMonitor `kubeswift-controller-manager` + PrometheusRule `kubeswift` in `kubeswift-system` |
| migration mTLS issuer | **no** | `migration.mtls.enabled` not set (default false); no Issuer or Certificate for migration in `kubeswift-system` (only `kubeswift-ui-tls`, `kubeswift-webhook-cert`) |

- **federation.role:** `hub`. Fleet `Cluster` CRs: `kubeswift` (self) READY, `sov` READY.
- **VAPs:** `kubeswift-launcher-sa-gate`, `kubeswift-launcher-sa-token-secret-gate`
  (both bound with `[Deny]`).
- **Storage classes:**

| Class | Provisioner | Default | RWX Block / migratable | Replicas | Snapshot class |
|---|---|---|---|---|---|
| `longhorn` | driver.longhorn.io | **yes** | no | 3 | `longhorn-snapshot-vsc` (not default-annotated) |
| `longhorn-migratable` | driver.longhorn.io | no | **yes** (`migratable: true`) | 3 | `longhorn-snapshot-vsc` |
| `longhorn-vm` | driver.longhorn.io | no | no | 2 | `longhorn-snapshot-vsc` |
| `longhorn-static` | driver.longhorn.io | no | no | – | – |
| `ceph-block` | rook-ceph.rbd.csi.ceph.com | no | **yes** (RBD Block RWX) | – | `ceph-block-vsc` (not default-annotated) |

  The VolumeSnapshot CRDs and both VolumeSnapshotClasses are present (snapshot-capable).
  Rook CephCluster: `Ready`, `HEALTH_WARN` (`MON_DISK_LOW: mon b is low on available space`).
  Longhorn: all 3 nodes Ready and Schedulable; volumes 2 healthy, 4 `unknown`
  (detached). Free disk: worker-2 286 Gi, worker-1 114 Gi, cp-1 59 Gi.
- **CRDs:** the 15 core kubeswift CRDs, all `v1alpha1`, plus `gpucellpools.cells.kubeswift.io`
  from the separate gpucellpool chart. `swiftsnapshots` `status.guestSpec`:
  **present**. Keys: `cpu, dataDisks, guestAgent, hasDataDisks, hasSeed, imageName,
  interfaceNames, memoryMi, network, osType, rootDiskSize, storage`. **No `primaryIP`.**
  `status.memorySnapshot` has `handle, sizeBytes`.

### sov

- **Kubernetes** `v1.35.3+k0s`. 3 nodes (cp-1, worker-1, worker-2), all Ready.
  **None is KVM-capable** (`/dev/kvm` is not a character device, and `vmx|svm`
  appears 0 times in `/proc/cpuinfo` on all three). No kubeswift node labels, no taints.
- **Helm release:** `kubeswift` in `kubeswift-system`, chart `kubeswift-0.14.1`,
  app `0.14.1`, revision 31, deployed 2026-09-24 21:41:22 UTC.
- **User-supplied values** (no secrets present):

```yaml
controllerManager: {image: {tag: v0.14.1}}
dra: {image: {tag: v0.14.1}}
federation:
  edge: {operatorGroups: [kubeswift-operators]}
  role: edge
gateway: {image: {tag: v0.14.1}}
gpuDiscovery: {image: {tag: v0.14.1}}
migrationStunnel: {image: {tag: v0.14.1}}
sandboxMaterialize: {image: {tag: v0.14.1}}
snapshotORAS: {image: {tag: v0.14.1}}
snapshotS3: {image: {tag: v0.14.1}}
swiftletd: {image: {tag: v0.14.1}}
```

- **Controller-manager:** `controller-manager:v0.14.1`, 1/1 Ready, 0 restarts, age 75 m.
  Args: `--leader-elect --webhook-enabled=false`. Env images are all `:v0.14.1`.
  It is the only kubeswift workload.

| Component | Installed? | Evidence |
|---|---|---|
| webhook | **no** | no kubeswift webhook configurations; `--webhook-enabled=false` |
| gateway | **no** | no Deployment (the tag is pinned but `gateway.enabled` is not set) |
| UI | **no** | — |
| cert-manager | **yes**, but not a standalone install | cert-manager CRDs + a cert-manager bundled by the CAPI management stack in another namespace; no kubeswift Issuer/Certificate |
| Prometheus Operator | **no** | no `monitoring.coreos.com` CRDs |
| migration mTLS issuer | **no** | — |

- **federation.role:** `edge` (member RBAC applied by default; operatorGroups `kubeswift-operators`).
- **VAPs:** `kubeswift-launcher-sa-gate`, `kubeswift-launcher-sa-token-secret-gate` (`[Deny]`).
- **Storage classes:** `longhorn` (**default**, 3 replicas) and `longhorn-static`.
  No migratable class. **No VolumeSnapshot CRDs, so no snapshot class.**
- **CRDs:** 15 core kubeswift CRDs, `v1alpha1`. `swiftsnapshots` `status.guestSpec`
  is present with the same keys as dev. **No `primaryIP`.**

### ntx

- **Kubernetes** `v1.34.9` (kubeadm), OVN-Kubernetes primary CNI (helm
  `ovn-kubernetes-1.2.0`; `k8s.ovn.org` CRDs). 3 nodes (cp-1, worker-1, worker-2),
  all Ready, **all KVM-capable** (nested). No taints.

| Node | kubeswift labels | /dev/kvm |
|---|---|---|
| cp-1 | — | yes |
| worker-1 | `kernel-node=true` | yes |
| worker-2 | `kernel-node=true` | yes |

- **Helm release:** `kubeswift` in `kubeswift-system`, chart `kubeswift-0.14.1`,
  app `0.14.1`, revision 25, deployed 2026-09-24 21:41:45 UTC.
- **User-supplied values** (no secrets present):

```yaml
controllerManager: {image: {tag: v0.14.1}}
dra: {image: {tag: v0.14.1}}
gateway: {image: {tag: v0.14.1}}
gpuDiscovery: {image: {tag: v0.14.1}}
migrationStunnel: {image: {tag: v0.14.1}}
monitoring: {enabled: true}
sandboxMaterialize: {image: {tag: v0.14.1}}
snapshotORAS: {image: {tag: v0.14.1}}
snapshotS3: {image: {tag: v0.14.1}}
swiftletd: {image: {tag: v0.14.1}}
```

- **Controller-manager:** `controller-manager:v0.14.1`, 1/1 Ready, 0 restarts, age 75 m,
  on worker-1. Args: `--leader-elect --webhook-enabled=false`. Env images are all `:v0.14.1`.

| Component | Installed? | Evidence |
|---|---|---|
| webhook | **no** | no kubeswift webhook configurations; `--webhook-enabled=false` |
| gateway | **no** | — |
| UI | **no** | — |
| cert-manager | **yes** | `cert-manager` namespace, 3/3 deployments Ready |
| Prometheus Operator | **yes** | helm `kube-prometheus-stack-87.15.1`; ServiceMonitor `kubeswift-controller-manager` present |
| migration mTLS issuer | **no** | — |

- **federation.role:** unset, so the chart default `standalone`.
- **VAPs:** `kubeswift-launcher-sa-gate`, `kubeswift-launcher-sa-token-secret-gate` (`[Deny]`).
- **Storage classes:**

| Class | Default | Migratable | Replicas |
|---|---|---|---|
| `longhorn-r1` | **yes** | no | 1 |
| `longhorn` | no | no | 3 |
| `longhorn-migratable` | no | **yes** | 2 |
| `longhorn-static` | no | no | – |

  **No VolumeSnapshot CRDs, so no Tier A CSI snapshots on ntx.**
- **CRDs:** 15 core kubeswift CRDs, `v1alpha1`. `status.guestSpec` is present, same
  keys as dev. **No `primaryIP`.**

---

## 2. Activity in progress

Now = 2026-09-24 ~22:55 UTC. "Last 6 h" = created after 16:55 UTC.

| Cluster | SwiftGuests | SwiftSnapshots | SwiftRestores | SwiftMigrations | Other | Namespaces < 6 h |
|---|---|---|---|---|---|---|
| dev | `gpu-cells/innercp` Running on worker-2, 8 d (downstream GPUCellPool lab guest; launcher `swiftletd:v0.13.15`, uid `10bd28b8-…`, 0 restarts) | `field-testing/ft-golden-snap` Ready, 20 d | none | `default/ceph-mig-b-migrate-g962j` live, Completed, 39 d | SwiftSandboxPool `field-testing/ft-gpu-pool` Degraded, 20 d (known #659/#660) | **none** (newest: `gpu-cells`, 2026-09-16) |
| sov | none | none | none | none | — | **none** (newest: 2026-07-03) |
| ntx | `capi-udn/ks-udn-cp-54klw` Running on cp-1, 68 d; `capi-udn/ks-udn-md0-sz2ql-bz9zs` **Scheduling** on worker-1 (see heads-up 4) | none | none | `field-testing/ft-live-vm-migrate-{prtpd,thkdp}` live, Completed, 78–79 d | — | **none** (newest: `capi-udn`, 2026-07-18) |

**No other validation appears to be running.** The only recent activity on dev
was the finished `val141` check (heads-up 1). On ntx, the worker guest's launcher
churn has been going on since before the v0.14.1 upgrade, and it comes from
Longhorn, not from a test.

---

## 3. dev: live migration and snapshot-suite storage

- **Live migration: possible.** All three nodes have `/dev/kvm`. worker-1 and
  worker-2 are the natural pair; cp-1 is untainted and KVM-capable too. There are
  two RWX Block options:
  - `longhorn-migratable`, via the existing SwiftGuestClass `small-migratable`
    (2 vCPU, 2 Gi, 10 Gi RWX Block).
  - `ceph-block`, via `ceph-migratable` (2 vCPU, 2 Gi, 6 Gi RWX Block). This is
    what the last dev live migration used, 39 d ago, and Ceph is currently
    `HEALTH_WARN` (mon disk low).

  **Recommendation: `longhorn-migratable`** for D7–D9. D8 and D9 need a bigger
  guest. worker-1 has about 49 Gi allocatable headroom and worker-2 about 50 Gi
  (requests are 7 % and 18 % of memory), so a 16–24 Gi guest fits on either.
  migration mTLS is off, so migrations use the plain-TCP path.
- **Snapshot suites:**
  - Tier A (D5, `snapshot-test` / `clonestrategy-test`): **`longhorn` (default
    SC) with `longhorn-snapshot-vsc`**, which must be passed as `--vsclass`
    (heads-up 6). `ceph-block` + `ceph-block-vsc` is the alternative, but Ceph is
    in `HEALTH_WARN`.
  - Tier B (D2, D3, D4, local memory snapshots): these use node hostPath under
    `/var/lib/kubeswift/snapshots/`. The source guest's root disk uses the sample
    class `snapshot-local-class` (no `storage` block, so the default SC
    `longhorn`). They do not depend on a snapshot class. Any KVM node works.

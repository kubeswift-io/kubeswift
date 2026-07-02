# Edge registry profile: Zot (`zot sync`)

KubeSwift's OCI features — golden images (`SwiftImage.spec.source.oci`),
snapshots (`SwiftSnapshot.spec.backend.type: oci`), cold / suspended-state
migration, and cosign provenance — treat the registry as a **declared external
dependency**: KubeSwift is a registry *client*, never a registry. This profile
is the recommended way to satisfy that dependency **at the edge**: a small,
self-contained [Zot](https://zotregistry.dev/) instance per edge site that
**mirrors VM artifacts from a hub registry** with Zot's `sync` extension.

Use this profile when:

- Edge clusters must pull golden images / resume full-state snapshots **without
  reaching the hub** on the hot path (slow, metered, or intermittent links).
- You want hub→edge distribution to be **pull-through and cached** (on-demand)
  and/or **pre-seeded** (periodic mirroring) rather than hand-copied.
- Air-gapped sites need a local registry that can be fed by an out-of-band
  `oras cp` / disk transfer instead of a network sync.

The hub can be anything OCI-conformant (ghcr.io, Harbor, another Zot). The
edge instance below is Zot because it is a single static binary/image, is
OCI-native (Referrers API → cosign signatures travel with the artifacts), and
has first-party mirroring.

## How it composes with KubeSwift

```
        hub registry (ghcr.io / Harbor / Zot)
          vm-images/…       vm-snapshots/…      (+ cosign referrers)
                 │  zot sync (onDemand and/or pollInterval)
                 ▼
        edge Zot  (zot-edge.<ns>.svc:5000, one per site)
                 ▲
   ┌─────────────┼──────────────────────────────┐
   │ SwiftImage  │ SwiftSnapshot                │ cold-migration import
   │ source.oci  │ backend.oci (capture local,  │ (cloneFromSnapshot pulls
   │ (golden     │ push to edge → sync back is  │ memory + disk artifacts
   │  image pull)│ NOT automatic — see below)   │ by digest via the edge)
   └─────────────┴──────────────────────────────┘
```

Every KubeSwift transfer Job pulls **by digest**, so a mirrored artifact is
byte-identical wherever it is fetched from — the digest either matches or the
pull fails loudly.

> **Direction:** `zot sync` replicates **hub → edge** (the edge *pulls*). An
> edge-side capture (`SwiftSnapshot backend.oci` pointed at the edge Zot) lands
> only on that edge; promoting it to the hub is a push from the edge side
> (`oras cp edge/repo:tag hub/repo:tag`) or a hub-side sync that lists the edge
> as an upstream. Design your repo topology accordingly.

## Install (edge cluster)

Apply the sample (adjust namespace/storage):

```bash
kubectl apply -f config/samples/edge-zot/zot-edge.yaml
```

The load-bearing part is the sync extension in the Zot config:

```json
{
  "distSpecVersion": "1.1.0",
  "storage": {"rootDirectory": "/var/lib/registry"},
  "http": {"address": "0.0.0.0", "port": "5000"},
  "extensions": {
    "sync": {
      "enable": true,
      "registries": [
        {
          "urls": ["https://hub.example.com"],
          "onDemand": true,
          "tlsVerify": true,
          "pollInterval": "6h",
          "content": [
            {"prefix": "vm-images"},
            {"prefix": "vm-snapshots"}
          ]
        }
      ]
    }
  }
}
```

- **`onDemand: true`** — a pull the edge doesn't have is fetched from the hub,
  cached, and served; subsequent pulls are local. This is the pull-through mode
  and needs no scheduling.
- **`content` + `pollInterval`** — the listed repo prefixes are additionally
  mirrored periodically (pre-seeding), so artifacts are already local before
  the first consumer asks. Use both together for edge sites with windows of
  connectivity.
- **`credentialsFile`** — set when the hub needs auth
  (`{"hub.example.com": {"username": "...", "password": "..."}}`), mounted from
  a Secret.
- Use the **full** Zot image (`ghcr.io/project-zot/zot-linux-amd64`) — the
  `zot-minimal` flavor has no extensions, so no sync.
- Give the edge instance a real PVC in production (the sample uses `emptyDir`
  for brevity); size it for your artifact set (a full-state snapshot is
  ~guest-RAM + the deduped disk chunks).
- TLS: front the edge Zot with your usual ingress/cert. A plaintext in-cluster
  edge works for KubeSwift pulls (`insecure: true` on the specs) but **cosign
  verification requires TLS** (see
  [`../design/oras-provenance-signing.md`](../design/oras-provenance-signing.md)).

## Pointing KubeSwift at the edge

Consumers just use the edge reference; nothing else changes:

```yaml
# Golden image, pulled from the edge mirror
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata: {name: noble-golden}
spec:
  format: raw
  rootDisk: {size: "10Gi"}
  source:
    oci:
      repository: zot-edge.registry.svc:5000/vm-images
      tag: noble-v1
      insecure: true   # in-cluster plaintext edge; drop with TLS
```

For a **cross-cluster cold-migration import** at the edge, recreate the
snapshot object with the **edge** repository and the same digests (the transfer
recipe is in [`../snapshots/cold-migration.md`](../snapshots/cold-migration.md));
the import's download Jobs pull by digest through the edge, which on-demand
syncs from the hub if the artifacts aren't local yet.

## Air-gapped sites

When there is no network path at all, feed the edge Zot out-of-band; the
KubeSwift side is unchanged:

```bash
# On a connected bastion: copy hub → an OCI layout on disk
oras cp --recursive hub.example.com/vm-images:noble-v1 --to-oci-layout ./airgap/vm-images:noble-v1
# Transfer ./airgap by disk, then on the edge side:
oras cp --recursive --from-oci-layout ./airgap/vm-images:noble-v1 zot-edge.registry.svc:5000/vm-images:noble-v1
```

(`--recursive` carries cosign referrers along, so signatures verify at the
edge.)

## Validated

On the dev cluster (2026-07-02, Zot v2.1.2, the exact config above with an
in-cluster hub): the edge instance started empty; the first **tag** pull of a
real full-state snapshot artifact through the edge returned it with a
**digest identical** to the hub's; a **digest-pinned** manifest pull (what
KubeSwift's `snapshot-oras` download Jobs perform) and a **64 MiB chunk blob**
fetch both served through the edge — i.e. the entire KubeSwift consumer
surface works against a sync-enabled edge Zot. Periodic (`pollInterval`)
mirroring uses the same config block per the
[Zot sync docs](https://zotregistry.dev/).

## See also

- [`../design/oras-vm-disk-artifacts.md`](../design/oras-vm-disk-artifacts.md) — the ADR (registry as a declared dependency)
- [`../snapshots/cold-migration.md`](../snapshots/cold-migration.md) — cross-cluster full-state moves
- [`../snapshots/s3-snapshots.md`](../snapshots/s3-snapshots.md) — the S3 alternative backend

# Cold / suspended-state migration (via an OCI registry)

Move a running VM's **full state — memory + disk — between nodes or clusters**
through an OCI registry, and **resume it where it left off** (not a fresh boot).
The source is suspended and its exact runtime state (RAM + disk) is pushed to a
registry; a new guest elsewhere CH-restores that memory against a disk
materialized from the registry.

Use cold migration when:

- You need to move a VM that **cannot live-migrate** (e.g. its state must cross a
  cluster boundary, or there is no shared L2 / migration network between source
  and target).
- Seconds-to-minutes of downtime is acceptable (the VM is paused for the capture,
  then resumes on the target).
- You want the move to be **asynchronous and durable** — the registry is the seam,
  so the source and target never talk directly and the artifact outlives either.

For same-cluster, near-zero-downtime moves that preserve the IP, use **live
migration** instead ([../migration/overview.md](../migration/overview.md)). For
fanning out many *fresh* clones from one snapshot, use
[clone-from-snapshot.md](clone-from-snapshot.md).

> **Source-independence:** a full-state export also captures a
> **launcher-sufficient spec surface** into the snapshot
> (`status.guestSpec`), so import works even when the source SwiftGuest —
> and its SwiftImage and SwiftSeedProfile — **no longer exist**. When the
> source is still present its live spec is used (unchanged behaviour); when
> it is gone the clone resolves from the captured surface + the registry
> artifacts alone. See "Cross-cluster (source-independent) import" below
> for the recipe and limits (same-name namespace).
>
> **Data disks (v1.1):** a full-state export also carries the source's
> **secondary VM data disks** (`blank` and `pvcRef` + `attachAsDisk`) as
> their own registry artifacts, and import materializes + attaches them
> under the same names — so a clone gets the data-disk state its resumed
> RAM references, not fresh blank disks. A source with an **image-backed**
> data disk (the legacy `dataDiskRef`, or `dataDiskRefs[].imageRef`) is
> rejected for full-state export (that disk is the SwiftImage's shared PVC,
> owned by the image, not the guest).

## How it works

Cold migration composes two shipped mechanisms — it adds no new runtime path:

1. **Export** = a full-state `SwiftSnapshot` (`backend.type: oci`,
   `includeMemory: true`, `includeDisk: true`). This is **capture-then-terminate**:

   ```
   Pending ──▶ Capturing ──▶ Uploading ──────────────────▶ Ready
              (pause +        (STOP source → terminate       (status.oci: memory
               snapshot        launcher → chunk the           + status.oci.disk)
               memory,         frozen disk to oci → push
               stay paused)    memory to oci)
   ```

   The guest is paused, its memory is snapshotted, the **source is stopped**
   (`runPolicy: Stopped`) at the snapshot instant so the disk is coherent and no
   split-brain is possible, and a memory + disk artifact pair is pushed to the
   registry. The source stays down — this is a migration, not a backup.

2. **Import** = a `SwiftGuest` with `cloneFromSnapshot` pointing at that snapshot.
   The controller materializes the root disk from the snapshot's **OCI disk
   artifact** (pinned by digest, into a fresh per-guest PVC) and the
   restore-receive launcher CH-`--restore`s the captured memory against it, then
   resumes. The guest continues from the exact point it was captured (same
   `boot_id`, same in-RAM state).

## Prerequisites

- An OCI registry reachable from the cluster (ghcr.io, Harbor, a TLS-fronted or
  in-cluster [Zot](https://zotregistry.dev/)). For a private registry, a
  `kubernetes.io/dockerconfigjson` pull Secret in the guest's namespace.
- The controller's `KUBESWIFT_SNAPSHOT_ORAS_IMAGE` set (the Helm chart sets it;
  `make deploy` uses the pinned default).
- The source guest running with a memory-snapshottable hypervisor (Cloud
  Hypervisor). VFIO/GPU guests cannot be memory-snapshotted (webhook-rejected).

## Quick start (swiftctl)

```bash
# Export a running guest's full state to the registry (source is stopped).
swiftctl guest export db --to ghcr.io/acme/vm-snapshots --credentials-secret regcreds --wait
# ... Ready. Artifacts:
#   memory: ghcr.io/acme/vm-snapshots:default-db-export (sha256:...)
#   disk:   ghcr.io/acme/vm-snapshots:default-db-export-disk (sha256:...)

# Resume it as a new guest on another node.
swiftctl guest import db2 --from-snapshot db-export --target-node boba --guest-class ft-small --wait
# ... Running on boba (IP 192.168.99.x)
```

`--sign-key <secret>` on export cosign-signs the artifacts (supply-chain
provenance — see [s3-snapshots.md](s3-snapshots.md) and the signing design). Use
`--insecure` only for a plaintext in-cluster/test registry on a trusted network.

## Equivalent YAML

`swiftctl guest export db --to <repo>` creates:

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshot
metadata:
  name: db-export
spec:
  guestRef: {name: db}
  includeMemory: true
  includeDisk: true          # full-state: pairs the disk with the memory
  backend:
    type: oci
    oci:
      repository: ghcr.io/acme/vm-snapshots
      credentialsSecretRef: {name: regcreds}   # omit for anonymous
      # insecure: true                         # plaintext registry only
      # signingKeySecretRef: {name: cosign-key}
```

`swiftctl guest import db2 --from-snapshot db-export --target-node boba
--guest-class ft-small` creates:

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: db2
spec:
  cloneFromSnapshot:
    snapshotRef: {name: db-export}
    targetNode: boba           # required for an oci snapshot
  guestClassRef: {name: ft-small}  # required even for a clone (resources come
  runPolicy: Running               # from the snapshot, but a class ref is mandatory)
```

## Cross-cluster (source-independent) import

A full-state export freezes a **launcher-sufficient surface** into
`status.guestSpec` (resources, storage shape, root-disk size, networking,
interface names, OS type, seed/agent flags) while the source is live. Import
then needs only **three things in the target**: the snapshot object, the
registry artifacts, and a SwiftGuestClass for the clone's `guestClassRef` —
no source SwiftGuest, no SwiftImage, no SwiftSeedProfile.

Within one cluster this is automatic: delete the source (and even its image
and seed profile) after export, and `swiftctl guest import` still works.

**To move to a different cluster**, recreate the SwiftSnapshot object there
(the artifacts are already in the shared registry; only the small object
moves). Its oci refs + captured surface live in *status*, which `kubectl
apply` cannot set — patch the status subresource after creating the spec:

```bash
# In cluster A: dump the snapshot's spec and status.
kubectl -n team-a get swiftsnapshot db-export -o json > snap.json

# In cluster B: recreate the spec, then transplant the status.
kubectl -n team-a apply -f <(jq 'del(.status, .metadata.uid, .metadata.resourceVersion,
  .metadata.creationTimestamp, .metadata.generation, .metadata.finalizers,
  .metadata.managedFields)' snap.json)
kubectl -n team-a patch swiftsnapshot db-export --subresource=status \
  --type=merge -p "$(jq '{status: .status}' snap.json)"

# Then import as usual:
swiftctl guest import db2 --from-snapshot db-export --target-node <node> --guest-class <class>
```

(A first-party `swiftctl snapshot export-manifest`-style helper is a tracked
follow-up.)

### v1 requirements and limits

- **Same-name namespace.** The captured memory's `config.json` records paths
  under the source's `<namespace>-<name>` runtime directory; the import
  rewrites them using the snapshot's namespace. Recreate the snapshot in a
  namespace **with the same name** as the source's original namespace.
- **Data disks (v1.1).** A source's `blank` / `attachAsDisk` data disks are
  captured + materialized on import (byte-round-tripped through the registry,
  attached under the same names). A source with an **image-backed** data disk
  (legacy `dataDiskRef` / `dataDiskRefs[].imageRef`) is rejected for full-state
  export — that disk is the SwiftImage's shared PVC; re-export without it, or
  convert it to a `blank`/`attachAsDisk` disk.
- **Placeholder seed.** When the source had a seed profile, the clone gets a
  minimal placeholder seed.iso purely so CH `--restore` can re-open the seed
  disk — the resumed guest never reads it. A later *reboot* of such a clone
  has no real seed (the identity-regen bootcmd won't fire); use the in-guest
  vsock agent for identity ([identity-regeneration.md](identity-regeneration.md)).
- **Default storage-class fall-through.** A source that didn't pin a
  storageClassName clones onto the *target cluster's default* class.
- **Registry reachability + credentials** in the target cluster (same
  `credentialsSecretRef` semantics as capture).
- **Pre-existing snapshots** captured by older controllers don't carry the
  surface — those still require the live source guest.

## What resumes vs. what regenerates

Because import **resumes** captured memory (it is not a fresh boot), the guest
keeps the source's in-guest identity by default: `machine-id`, SSH host keys,
hostname, and the guest-visible IP are inherited. Each imported guest gets a
distinct **hypervisor-side** MAC and its own pod network namespace, so two
imports of the same snapshot do not collide at L2.

- To regenerate identity in place with no reboot, enable the in-guest vsock agent
  on the source before export — see
  [identity-regeneration.md](identity-regeneration.md).
- The `cloneFromSnapshot.regenerate` list (hostname/machineId/sshHostKeys) fires
  on the guest's first *reboot* via the seed bootcmd; note the CH v52 clone-reboot
  caveat in [clone-from-snapshot.md](clone-from-snapshot.md).

## Verifying a resume (not a reboot)

```bash
# On the source, before export — record the boot id:
cat /proc/sys/kernel/random/boot_id
# After import, on the resumed guest — it MATCHES (a reboot would change it):
cat /proc/sys/kernel/random/boot_id
journalctl --list-boots     # a single boot spanning source-capture → resume
```

## Cleanup

The source is left `Stopped` after export (the workload has moved). Delete it once
the import is healthy:

```bash
kubectl delete swiftguest db          # the stopped source
kubectl delete swiftsnapshot db-export  # optional; oci objects are purged if
                                        # deletionPolicy: Delete (the default)
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Export stuck in `Capturing`/`Uploading` | Check the snapshot's conditions (`swiftctl snapshot describe db-export`) and the `<snap>-oci-*` Job logs. A private registry needs `--credentials-secret`; a plaintext registry needs `--insecure`. |
| Export `Failed: OCI disk chunk Job failed` | Registry auth/TLS, or the node ran out of space chunking the disk. Inspect the `<snap>-oci-disk` Job. |
| Import guest stuck in `Scheduling` | The disk-from-oci download Job (`swiftguest-root-<guest>-oci-disk-dl`) is still pulling, or `--target-node` has no capacity. Check the Job + `kubectl describe swiftguest <guest>`. |
| Import `Failed: source SwiftGuest ... no longer exists` | The snapshot is memory-only or pre-dates the captured spec surface — those still need the live source. Full-state exports from current releases import source-independently (see the cross-cluster section). |
| Export rejected: image-backed data disk | Full-state export carries `blank`/`attachAsDisk` data disks (v1.1) but rejects an **image-backed** data disk (legacy `dataDiskRef` / `dataDiskRefs[].imageRef`) — that PVC belongs to the SwiftImage. Re-export without it. |
| Import `Failed: ... pre-v1.1 root-disk-only snapshot` | The snapshot was captured by a controller predating v1.1 and recorded data disks without registry artifacts. Re-export from a current release, or keep the live source guest. |
| Two copies running | Pre-fix behaviour; on current releases the source is stopped during export. If you manually restarted the source, stop it again. |

## See also

- [../design/oras-cold-migration.md](../design/oras-cold-migration.md) — design + decisions
- [clone-from-snapshot.md](clone-from-snapshot.md) — fan out fresh clones from a snapshot
- [s3-snapshots.md](s3-snapshots.md) — the S3 equivalent of the OCI backend
- [../migration/overview.md](../migration/overview.md) — live migration (same-cluster, IP-preserving)

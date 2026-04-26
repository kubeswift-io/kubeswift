# Local Snapshots (Tier B)

Tier B captures full VM state — memory + disk references — to a
node-local hostPath directory. This is the right backend when:

- You want a memory snapshot (Tier B is the only Phase 2 backend that
  pauses the VM and serializes RAM).
- The source VM lives on a single node and won't migrate.
- You're OK with restores running only on the same node where the
  capture happened (snapshots are inherently node-local; we don't
  copy them between nodes).

For backup/restore where you want cross-node mobility and disk-only
state, use `backend: csi-volume-snapshot` instead. See
[csi-snapshots.md](csi-snapshots.md).

## Anatomy

A Tier B SwiftSnapshot captures three things on the source node:

| File                | What                                  |
|---------------------|---------------------------------------|
| `config.json`       | CH's view of the VM (devices, paths)  |
| `state.json`        | CPU/device/regs (~62KB)               |
| `memory-ranges`     | Serialized RAM (≈ VM memory size)     |

Total on-disk size ≈ VM RAM + a few KB of metadata. KubeSwift only
ever reads/writes these files via Cloud Hypervisor itself plus a
narrow set of in-place edits to `config.json` (clone identity-
regen marker, MAC rewrites). All other files are opaque.

## Quick start

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshot
metadata:
  name: db-mem-2026-04-26
spec:
  guestRef:
    name: db
  backend:
    type: local
    local:
      hostPath: /var/lib/kubeswift/snapshots/default-db-mem-2026-04-26
  includeMemory: true       # default; explicit here for clarity
  resumeAfterSnapshot: true # default; resume the VM once capture done
```

The `hostPath` must be under `/var/lib/kubeswift/snapshots/` — the
admission webhook rejects other prefixes. The directory is created
on the node where the source VM is running; KubeSwift schedules a
cleanup pod on that node when the SwiftSnapshot is deleted.

Equivalent CLI:

```bash
swiftctl snapshot create db-mem-2026-04-26 --guest db --backend local \
  --hostpath /var/lib/kubeswift/snapshots/default-db-mem-2026-04-26
```

## Restoring

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftRestore
metadata:
  name: db-restore-1
spec:
  snapshotRef:
    name: db-mem-2026-04-26
  targetGuest:
    name: db                # in-place restore (same name as source)
  resumeAfterRestore: true
```

For an **in-place** restore (same name as source), no identity
regeneration is needed — the restored VM is the same VM. For a
**clone** (different name), set `spec.identity.regenerate` to include
at least `macAddresses`:

```yaml
spec:
  snapshotRef:
    name: db-mem-2026-04-26
  targetGuest:
    name: db-clone           # clone, not in-place
  identity:
    regenerate:
      - macAddresses         # required by webhook for memory clones
      - hostname
      - machineId
      - sshHostKeys
```

The macAddresses regen is enforced by the webhook — without it, two
VMs would share a MAC on the same L2 segment and the conflict is
unrecoverable from inside the guest. The other three (hostname,
machineId, sshHostKeys) are in-guest concerns; see
[identity-regeneration.md](identity-regeneration.md) for the
mechanism.

## Constraints (hard, enforced upfront)

The admission webhook **rejects** SwiftSnapshots with:

- `gpuProfileRef` set on the source guest. VFIO + memory snapshot
  succeeds at capture but fails restore with `bar 0 already used`
  (Phase 0 spike Constraint #1). No safe path; rejection is
  permanent.
- An SR-IOV interface on the source guest. Same VFIO failure mode.
- `kubeswift.io/hypervisor-override=qemu` on the source guest.
  Phase 2 ships memory snapshots on Cloud Hypervisor only.

## Pause window

Capturing memory pauses the source VM until the entire RAM image is
written to disk. Phase 0 measured ~2.8 s/GiB on Longhorn-backed
hostPath; faster storage scales linearly. See
[pause-window.md](pause-window.md) for sizing guidance and what to
do about evictions during capture.

## Hypervisor version compatibility

The SwiftRestore controller checks the snapshot's recorded
`status.hypervisorVersion` against the cluster's current CH version.
Default policy:

- Exact match: proceed.
- Same major.minor, different patch: proceed (routine upgrades).
- Different minor: **block** (default).
- Different major: **block** (default).

For disaster recovery after a major upgrade, set the
`kubeswift.io/skip-hypervisor-version-check=true` annotation on the
SwiftRestore (or pass `--skip-hypervisor-version-check` to
`swiftctl restore create`). Restore may fail with `bar 0 already
used`-style errors; the override exists because in DR scenarios
"try the restore" is preferable to "give up".

## Restore staging — disk cost & the in-place fast path

When SwiftRestore brings a Tier B snapshot back up, the launcher pod
needs the snapshot directory in its mount namespace. There are two
shapes the controller can produce:

### In-place fast path (no staging copy)

When `targetGuest.name` equals the source SwiftGuest name AND
`spec.identity.regenerate` is empty (or omitted), the launcher pod
mounts the snapshot directory **read-only** from its on-node hostPath
directly. No staging emptyDir, no init container, no `cp -r`. The
restored VM resumes against the original snapshot bytes verbatim.

This is the right path for **disaster recovery**: the original VM
crashed or was deleted and you want the same VM back, byte-for-byte,
including its machine-id, SSH host keys, and MAC.

```yaml
spec:
  snapshotRef:
    name: db-mem-2026-04-26
  targetGuest:
    name: db                       # same as source — fast path
    overwriteExisting: true        # required when source SwiftGuest still exists
  resumeAfterRestore: true
  # no spec.identity → in-place fast path
```

Disk cost: 0 bytes beyond the snapshot itself.

### Clone path (stage + patch — disk cost ≈ snapshot size per clone)

When the target name differs from the source OR any identity
attribute is regenerated, the launcher pod gets:

- A **read-only** hostPath mount of the snapshot directory at
  `/var/lib/kubeswift/restore/source` (init container only).
- A **writable** pod-local `emptyDir` at
  `/var/lib/kubeswift/restore/staging`.
- A `snapshot-stager` init container that copies the snapshot into
  the emptyDir and applies the requested config.json patches
  (`kubeswift.clone=true` cmdline marker for the in-guest bootcmd
  and per-clone MAC rewrites). Sentinel-guarded for restart safety.

The launcher boots from the staged copy, never the read-only source.
This keeps the on-disk snapshot pristine across multiple concurrent
clones — important because two clones cannot in-place-patch the same
config.json without the second clone reading the first clone's MAC.

**Disk cost: roughly the snapshot size, per concurrent clone, on the
source node, for the lifetime of the cloned launcher pod.** A 16 GiB
VM clone allocates ~16 GiB of pod-local emptyDir on the source node.
Two simultaneous clones of a 64 GiB VM consume ~128 GiB of additional
disk. The staging emptyDir is freed when the clone's launcher pod is
deleted (Kubernetes `emptyDir` lifecycle — node-local scratch space
that lives with the pod).

If disk pressure on the source node is a concern, **prefer the CSI
backend** (`backend: csi-volume-snapshot`) for cloning workflows.
That backend captures only disk state and uses CSI VolumeSnapshots
that the storage system handles efficiently (often via copy-on-write
references rather than full copies). The trade-off: CSI does not
capture in-memory state, so cloned VMs reboot from disk rather than
resuming from a snapshot's RAM.

Phase 3 (S3 backend) is intended to lift the node-local constraint
entirely — clones materialize from S3 directly into the launcher pod
without consuming source-node disk — but that's not in Phase 2 scope.

### Why the staging copy

The ostensibly simpler design — patch the snapshot's `config.json`
**in place** on the read-only source — would corrupt multi-clone
workflows: clone B would read clone A's MAC (the patched value)
rather than its own. The staging copy gives each clone its own
config.json to mutate, leaving the on-disk snapshot strictly
read-only.

### Disk caveat — clone-mode root disk

A Tier B memory snapshot does **not** capture the disk. The clone's
root disk is a fresh copy from the source SwiftImage (KubeSwift's
existing per-guest clone path), not a snapshot of the source VM's
disk at capture time. The clone resumes a memory state that may
expect filesystem writes the fresh disk doesn't have.

For workloads where this matters (mostly anything that depends on
on-disk writes between cloud-init and snapshot), use the CSI backend
or take a fresh CSI snapshot of the source VM's per-guest PVC and
restore both backends together. The identity-regeneration test
suite intentionally targets workloads where the in-guest cloud-init
bootcmd is the only post-resume disk write of consequence
(machine-id, SSH host keys, hostname).

## Cleanup

Deleting a SwiftSnapshot triggers a one-shot cleanup pod on the
source node that runs `rm -rf` on the hostPath subdirectory. The
finalizer `kubeswift.io/snapshot-hostpath-cleanup` blocks deletion
until cleanup completes.

If the cleanup pod fails (e.g. node is unreachable, hostPath was
already removed manually), the finalizer is retained and the
operator can `kubectl delete pod swift-snap-cleanup-<snap-name>` to
trigger a re-create on the next reconcile pass.

If the source node is permanently lost, you can manually remove
the finalizer to allow GC:

```bash
kubectl patch swiftsnapshot/<name> -p '{"metadata":{"finalizers":[]}}' --type=merge
```

The hostPath remains on the lost node's storage (or doesn't, if the
node is truly gone). Orphan cleanup of pre-existing directories is
not in Phase 2 scope.

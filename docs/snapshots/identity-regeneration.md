# Clone Identity Regeneration

When a memory snapshot is restored to a **different** SwiftGuest name
(a clone), the new VM resumes the captured guest's state byte-for-byte
— including its `/etc/machine-id`, `/etc/ssh/ssh_host_*_key*`, kernel-
captured network MAC, and whatever the guest had set as its hostname.
Without intervention every clone shares network and identity state
with the source: same MACs (L2 collisions), same machine-id (dbus,
journald, systemd, license-anchored apps confused), same SSH host
keys (man-in-the-middle warnings on connect).

KubeSwift handles regeneration in two halves: a **hypervisor-level**
half done by the controller before CH boots from the snapshot, and an
**in-guest** half done by a cloud-init module on first wake.

## Hypervisor-level: MAC rewriting

The SwiftRestore controller patches `config.json` inside the snapshot
directory before kicking off the restore-receive launcher. For each
network device, it computes a deterministic new MAC:

```
new_mac = first 24 bits of sha256(target-namespace/target-name/iface-name)
          OR'd with locally-administered bit, multicast bit cleared
```

(`internal/runtimeintent/GenerateMAC` + `InterfaceMACSeed`). Same
inputs always produce the same MAC, so re-running a restore against
the same target name doesn't churn MACs across pod recreations.

The patch only writes the `mac` field on `config.net[]` entries; all
other fields (queue counts, MTU, tap names) pass through. CH on
restore reads the patched config and brings up the virtio-net devices
with the new MAC values.

## In-guest: cloud-init `bootcmd` module

The MAC change is a hypervisor-level fait accompli. The other three
(machine-id, SSH host keys, hostname) need to happen inside the
guest — the controller can't safely modify a paused guest's
filesystem. Mechanism:

1. The controller appends `kubeswift.clone=true` to the kernel
   cmdline in the snapshot's `config.json` (idempotent — re-running
   doesn't double-append).
2. CH boots the restored VM. `/proc/cmdline` carries the marker.
3. cloud-init runs the `bootcmd` from the SwiftSeedProfile shipped
   in `config/samples/seed-profiles/clone-identity-regen.yaml`. The
   bootcmd:
   - Checks `/proc/cmdline` for `kubeswift.clone=true`.
   - Checks `/var/lib/kubeswift/.clone-regenerated` for absence.
   - If both: regenerates `/etc/machine-id` (via
     `systemd-machine-id-setup` or `uuidgen`), removes and
     re-creates SSH host keys (`ssh-keygen -A`), sets the hostname
     (`hostnamectl` with `/etc/hostname` fallback), and touches
     the sentinel.
   - If either condition fails: exit 0 (idempotent no-op).

### Why `bootcmd`, not `runcmd`?

cloud-init's `runcmd` runs once per *instance-id*. Snapshot/restore
preserves the original instance-id (it's part of the captured cloud-
init state), so `runcmd` would be a no-op on every clone after the
first. `bootcmd` runs every boot, gated by the sentinel file, which
gives "exactly once per clone" without depending on instance-id
detection.

### Bind your guests to the seed profile

Apply this seed profile to your *source* SwiftGuest (the one you'll
take the snapshot of) via `spec.seedProfileRef.name`:

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: db
spec:
  seedProfileRef:
    name: clone-identity-regen
  # ... rest of spec
```

The seed contents are baked into the snapshot's memory state at
capture time — clones inherit the seed automatically. You don't need
to apply the seed profile separately to each clone.

If your existing source guests use a different seed profile, you can
merge the bootcmd module into your existing profile's `userData`
instead of switching profiles wholesale.

## What to verify after a clone

After cloning and waiting for the restored guest to reach Running:

```bash
# Compare /etc/machine-id between source and clone
swiftctl ssh db --       cat /etc/machine-id
swiftctl ssh db-clone -- cat /etc/machine-id
# Must differ.

# Compare SSH host fingerprints
swiftctl ssh db --       ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
swiftctl ssh db-clone -- ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
# Must differ.

# Compare hostname
swiftctl ssh db --       hostname
swiftctl ssh db-clone -- hostname
# Must differ.

# Compare MAC
swiftctl ssh db --       cat /sys/class/net/eth0/address
swiftctl ssh db-clone -- cat /sys/class/net/eth0/address
# Must differ.
```

If any of these match, the clone identity regeneration didn't fire.
Common causes:

- Guest doesn't have the seed profile applied to its source —
  bootcmd never ran.
- Sentinel file was created in a snapshot taken AFTER first
  regeneration: bootcmd correctly skips on restore. Take a fresh
  snapshot from the un-regenerated source.
- Image is missing `ssh-keygen` or `systemd-machine-id-setup` —
  bootcmd silently no-ops on missing tools. Check
  `/var/log/cloud-init.log` in the guest for errors.

## Future work

- Per-clone hostname template (operator-set) is plumbed through
  cloud-init metadata but the seed profile only consumes it when
  present. A controller-driven hostname-from-targetGuest-name
  override would tighten this in a future commit.
- IPv6 SLAAC addresses derive from MAC under EUI-64; the MAC change
  fixes those automatically. IPv6 ULAs configured via DHCPv6 are
  unaffected (DHCPv6 issues a fresh prefix from the new MAC).

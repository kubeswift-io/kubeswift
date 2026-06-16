# Clone Identity Regeneration

When a memory snapshot is restored to a **different** SwiftGuest name
(a clone), the new VM resumes the captured guest's state byte-for-byte
— including its `/etc/machine-id`, `/etc/ssh/ssh_host_*_key*`, the
guest-visible MAC on the virtio-net device, and whatever the guest
had set as its hostname. Without intervention every clone shares
identity state with the source: same machine-id (dbus, journald,
systemd, license-anchored apps confused), same SSH host keys
(man-in-the-middle warnings on connect), same hostname.

This document describes what KubeSwift does at the hypervisor layer,
what does NOT happen automatically inside the guest, and the
workarounds operators have today.

## The real fix: the in-guest identity agent (vsock)

> The reboot-based remedy further down (`clone-identity-regen.yaml` +
> a cloud-init `bootcmd`) is **broken on Cloud Hypervisor v52**: a
> `--restore`d guest hangs in EDK2 firmware on reboot
> ([`../design/known-issues-clone-reboot-firmware-hang.md`](../design/known-issues-clone-reboot-firmware-hang.md)).
> The replacement is the **in-guest identity agent** — it regenerates
> machine-id / SSH host keys / hostname / MAC and re-DHCPs **in place,
> with no reboot**, over a host-only vsock channel. Design + validated
> spike:
> [`../design/clone-identity-vsock-agent.md`](../design/clone-identity-vsock-agent.md),
> [`../design/clone-identity-vsock-agent-spike.md`](../design/clone-identity-vsock-agent-spike.md).

KubeSwift ships `kubeswift-guest-agent` (a tiny static binary) plus a
`kubeswift-guest-agent.service` systemd unit inside the `swiftletd`
image at `/usr/local/share/kubeswift/guest-agent/`. The agent must be
**running in the SOURCE guest at snapshot-capture time** — a clone
resumes captured RAM and never boots, so the agent cannot be started
on the clone; it has to already be there.

**Golden image (recommended for fleets).** Bake the binary + unit into
your `SwiftImage` at build time:

```sh
install -m0755 kubeswift-guest-agent /usr/local/bin/kubeswift-guest-agent
install -m0644 kubeswift-guest-agent.service /etc/systemd/system/
systemctl enable kubeswift-guest-agent
```

**Seed profile.** Attach the `guest-agent` SwiftSeedProfile
([`config/samples/seed-profiles/guest-agent.yaml`](../../config/samples/seed-profiles/guest-agent.yaml))
to the source SwiftGuest via `spec.seedProfileRef`; it installs the
agent from a controller-attached virtiofs share on first boot. (The
controller-side share attachment and the auto-drive on clone resume
ship in later PRs of the arc; once they land, a clone's identity is
regenerated automatically and `status.network.primaryIP` populates
with no operator action.)

**Verify it is running on the source before snapshotting:**

```sh
systemctl is-active kubeswift-guest-agent   # -> active
```

If a clone is created from a snapshot whose source did **not** have
the agent running, the clone still works as a warm replica with the
source's identity; KubeSwift surfaces the gap as a
`CloneIdentityRegenerated=False` / `GuestAgentUnreachable` condition
(later PR) rather than failing silently.

The remainder of this document describes the hypervisor-layer MAC
rewrite (which the agent complements by setting the *guest-visible*
MAC to match) and the legacy reboot-based workaround (kept for
agent-absent guests on pre-v52 hypervisors).

## The fundamental constraint: resume is not a boot

CH `--restore` resumes the captured guest state byte-for-byte. The
kernel does not re-init, systemd does not re-init, and cloud-init
does not re-run. Anything cached in RAM at snapshot time is restored
into the resumed VM exactly as it was — including the contents of
`/etc/machine-id`, the SSH host key files (already loaded into the
sshd process and cached on the filesystem), the hostname (cached in
the kernel and exposed via `gethostname(2)`), and the virtio-net
driver's view of its MAC address.

Mechanisms that depend on a fresh boot — cloud-init `bootcmd`,
systemd unit ordering, kernel cmdline parsing — do not fire on
resume. There is no boot.

## Hypervisor-level: MAC rewriting in `config.json` (visible only to the bridge)

The SwiftRestore controller patches `config.json` inside the snapshot
directory before kicking off the restore-receive launcher. For each
network device, it computes a deterministic new MAC:

```
new_mac = first 24 bits of sha256(target-namespace/target-name/iface-name)
          OR'd with locally-administered bit, multicast bit cleared
```

(`internal/runtimeintent.GenerateMAC` + `InterfaceMACSeed`). Same
inputs always produce the same MAC, so re-running a restore against
the same target name doesn't churn MACs across pod recreations.

The patch only writes the `mac` field on `config.net[]` entries; all
other fields (queue counts, MTU, tap names) pass through.

**What this rewrite actually affects.** The new MAC is visible in
CH's `vm.info` output, in the bridge fdb of the launcher pod, and on
the host-side tap device. It is **not** visible to the guest's
`ip link show` output: the guest's virtio-net driver state — including
the MAC the guest believes its NIC has — is captured in the snapshot's
RAM image and restored verbatim. The guest keeps using the source's
MAC at the kernel network stack layer.

In practice, L2 collisions between clones running on the same node
are avoided not by the MAC rewrite but by **Kubernetes pod network
namespace isolation**: each launcher pod has its own bridge in its
own network namespace, so cross-clone bridge fdbs never overlap even
when the guest-visible MACs are identical.

The `host_mac` field in `config.json` is nulled by the same patcher
pass so CH's `open_tap` auto-discovers the clone tap's MAC at restore
time instead of forcing the source value (which would conflict if
two clones somehow ended up bridged together).

## In-guest identity (machine-id, SSH host keys, hostname): inherited from source

These three fields are filesystem and kernel state, captured and
restored along with the rest of the guest's RAM. There is no
hypervisor-layer mechanism that can rewrite them safely without
knowing the guest OS layout.

Empirical evidence captured during Phase 2 e2e (April 2026,
commit `a40c17f`): source, clone-a, and clone-b all showed identical
`machine-id`, SSH host fingerprint, hostname, and guest-side MAC,
despite the snapshot-stager patcher correctly rewriting the
hypervisor-side `config.net[0].mac` per clone.

### The cmdline-marker / bootcmd design (and why it doesn't work for memory snapshots)

The `snapshot-stager` patcher appends `kubeswift.clone=true` to
`config.payload.cmdline` for clones. The original design intent was
that an in-guest cloud-init `bootcmd` module would `grep` for this
marker on each boot and run the regen sequence.

This does not work for memory-snapshot resume:

1. **The marker may not propagate to `/proc/cmdline` at all on disk
   boot.** CH 51.1 disk-boot snapshots use CLOUDHV.fd as the kernel
   payload. CH writes the cmdline into guest memory at
   `arch::layout::CMDLINE_START` and PVH boot info points at it,
   but whether CLOUDHV.fd then forwards it through GRUB to the
   kernel is firmware-dependent. On Ubuntu Noble (cloud-images.ubuntu.com)
   the source guest's `/proc/cmdline` is the GRUB-supplied cmdline
   (`BOOT_IMAGE=/vmlinuz-… root=LABEL=cloudimg-rootfs ro …`) with no
   trace of any value CH supplied.
2. **Even if the marker did reach `/proc/cmdline`, the bootcmd
   never fires on resume.** cloud-init `bootcmd` runs on every
   *boot*, gated by systemd's cloud-init service ordering. A memory-
   snapshot resume is not a boot — systemd is already in `Completed`
   state, cloud-init has already exited, and nothing re-invokes it.

The patcher still installs the marker (and the configjson package
exports `CloneCmdlineMarker = "kubeswift.clone=true"`) because:

- It is needed for the future vsock-based regen path (the in-guest
  agent may consult `config.payload.cmdline` to confirm "this is a
  clone resume" if the cmdline does propagate via that boot path).
- Writing it costs nothing and is idempotent.
- For workflows that **do** reboot the clone after first resume,
  the marker is then visible in the post-reboot `/proc/cmdline`
  and the existing bootcmd works as originally designed.

But the marker alone, without a reboot, does not regenerate identity.

## What operators have today

### Workaround 1: reboot the clone after first resume

If your workflow tolerates a fast reboot post-clone-restore, this
is the simplest path. Issue `reboot` inside the clone via SSH or
serial console. The kernel cold-starts, cloud-init re-runs, and
the bootcmd in the SwiftSeedProfile fires normally — regenerating
machine-id, SSH host keys, and hostname on the cold-start kernel.

Sample bootcmd at
`config/samples/local-snapshots/01-seed-profile.yaml`. Fires
correctly on every fresh boot of a clone that carries the
`kubeswift.clone=true` cmdline marker.

### Workaround 2: manual regen inside the clone

If you can't reboot (e.g. the clone is mid-task and you want to
preserve the in-RAM state), regenerate identity manually:

```sh
# machine-id: dbus, systemd, journald all key on this
: > /etc/machine-id
systemd-machine-id-setup

# SSH host keys: regenerate from scratch
rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub
ssh-keygen -A
systemctl reload-or-restart ssh.service 2>/dev/null \
  || systemctl reload-or-restart sshd.service 2>/dev/null

# Hostname
hostnamectl set-hostname <unique-name>

# Optionally, force the guest to re-DHCP so the clone gets a
# fresh lease from the clone's pod-local dnsmasq instead of using
# the source's cached lease
dhclient -r eth0 2>/dev/null || ip addr flush dev ens3
dhclient eth0 2>/dev/null || dhclient ens3
```

The MAC at the guest level cannot be changed without unbinding
and rebinding the virtio-net driver (or rebooting). For most
workloads this isn't necessary because pod network isolation
prevents the cross-clone L2 collision the rewrite was meant to
solve.

## Future work: in-guest agent over vsock

The "fast clone, unique identity, no reboot" combination requires
an in-guest mechanism that fires on resume without needing
cloud-init to re-run. The targeted design is:

- A small one-shot service inside the guest image
  (`kubeswift-clone-regen.service`) listens on a vsock port at
  boot and remains idle.
- When swiftletd resumes a clone, it sends a "clone activated"
  message to the in-guest service via vsock (`vhost-vsock` is
  already supported by CH).
- The service runs the regen sequence (machine-id, SSH host
  keys, hostname, optionally a DHCP refresh, optionally a
  `ip link set` MAC change), reports completion, and exits.
- The service is idempotent: a sentinel file
  (`/var/lib/kubeswift/.clone-regenerated`) guards against
  repeated execution if the message is delivered twice.

This requires modifying the guest image build (the image needs
the vsock service shipped) and adding vsock plumbing on the
hypervisor side (CH config.json, swiftletd post-resume logic).
Tracked for a future phase; not in Phase 2 scope.

## What to verify after a clone today

```bash
# Compare /etc/machine-id between source and clone
swiftctl ssh source-name -- cat /etc/machine-id
swiftctl ssh clone-name  -- cat /etc/machine-id
# Phase 2: these MATCH (the documented limitation)

# Compare SSH host fingerprints
swiftctl ssh source-name -- ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
swiftctl ssh clone-name  -- ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
# Phase 2: these MATCH

# Compare hostname
swiftctl ssh source-name -- hostname
swiftctl ssh clone-name  -- hostname
# Phase 2: these MATCH

# Compare guest-visible eth0 MAC
swiftctl ssh source-name -- ip -br link show ens3
swiftctl ssh clone-name  -- ip -br link show ens3
# Phase 2: these MATCH (despite hypervisor config.net[0].mac being unique)
```

If you need them to differ, apply one of the workarounds above
or wait for the vsock-agent phase.

//! Runtime intent types and parsing.
//! Must match internal/runtimeintent output format.

use serde::Deserialize;

/// Canonical path for runtime intent. Must match controller mount.
pub const INTENT_PATH: &str = "/var/lib/kubeswift/intent/runtime-intent.json";

/// Canonical path for seed ConfigMap. Must match controller mount.
pub const SEED_PATH: &str = "/var/lib/kubeswift/seed";

/// Runtime intent - node-local VM specification.
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeIntent {
    pub root_disk: RootDisk,
    pub seed_path: String,
    pub cpu: u32,
    pub memory: u32,
    pub lifecycle: String,
    pub guest_id: String,
    /// When true, guest has network (TAP, DHCP). Defaults to true when seed present.
    #[serde(default)]
    pub network: Option<bool>,
    /// When set, boot via --kernel + --initramfs instead of --disk for root.
    #[serde(default)]
    pub kernel_boot: Option<KernelBoot>,
    /// Set alongside `kernel_boot` for a mode-3 sandbox: the OCI image as the
    /// VM's READ-ONLY root block device. swiftletd emits it as the first --disk
    /// (readonly, buffered) so it is /dev/vda; the bridge-initramfs overlays a
    /// tmpfs upper. Block path only. None for a normal SwiftGuest kernel boot.
    #[serde(default)]
    pub sandbox_rootfs: Option<SandboxRootfs>,
    /// Set alongside `sandbox_rootfs` for a mode-3 sandbox: the workload exec spec
    /// (argv + env + cwd). swiftletd serializes it to a per-sandbox read-only config
    /// disk (emitted right after the rootfs, so /dev/vdb) that the bridge-initramfs
    /// reads to exec the workload. Rides a DISK, never the cmdline — env stays off
    /// /proc/cmdline + the host's ps/logs. None for a SwiftGuest.
    #[serde(default)]
    pub sandbox_exec: Option<SandboxExec>,
    /// Hypervisor to use: "cloud-hypervisor" (default) or "qemu".
    /// Empty or absent means Cloud Hypervisor.
    #[serde(default)]
    pub hypervisor: Option<String>,
    /// Guest OS family: "windows" or "linux"/absent (default). When "windows",
    /// the CH disk-boot path adds `kvm_hyperv=on` to `--cpus` (the one runtime
    /// setting the spike proved Windows needs — without it the kernel hangs in
    /// early MP/HAL init).
    #[serde(default)]
    pub os_type: Option<String>,
    /// vCPU core-scheduling policy ("vm"/"vcpu"); absent/empty = off. When set,
    /// swiftletd appends core_scheduling=<v> to the CH --cpus arg.
    #[serde(default)]
    pub core_scheduling: Option<String>,
    /// Optional secondary data disk (appears as /dev/vdb in guest).
    /// LEGACY singular field — superseded by `data_disks`. Retained so a
    /// new swiftletd still honors intent JSON written by an older
    /// controller (version-skew tolerance); `data_disk_paths()` merges
    /// the two, preferring the slice when present.
    #[serde(default)]
    pub data_disk: Option<RootDisk>,
    /// Secondary data disks (each becomes an additional virtio-blk device
    /// in the guest, in order, after the root disk). A current controller
    /// emits this slice; an older one emits the singular `data_disk`.
    #[serde(default)]
    pub data_disks: Option<Vec<DataDiskSpec>>,
    /// Network interface list for multi-NIC support.
    /// If empty/absent and network=true, a single default NIC is used (backward compat).
    #[serde(default)]
    pub nics: Option<Vec<NICIntent>>,
    /// OVN-Kubernetes primary UDN interface (ovn-udn1) when the guest rides its
    /// namespace's primary UserDefinedNetwork (Model A). None otherwise. A TOP-LEVEL
    /// attribute (not per-NIC) because the primary is singular and the common case is
    /// a default guest with no nics. When set, network-init bridges this interface to
    /// br0/tap0 (setup_primary_udn_nic) and the guest adopts OVN's IP-derived MAC + IP
    /// (OVN port_security pins them); swiftletd uses that captured MAC for the primary
    /// NIC. eth0 stays on the cluster default for the control path.
    ///
    /// Explicit rename: the struct's rename_all=camelCase would expect
    /// `primaryUdnInterface`, but the Go controller emits `primaryUDNInterface`
    /// (the UDN acronym is kept upper-case in the json tag). Without the rename the
    /// field silently deserializes to None and the MAC override never fires.
    #[serde(default, rename = "primaryUDNInterface")]
    pub primary_udn_interface: Option<String>,
    /// virtiofs shares. For each, swiftletd spawns a virtiofsd backend
    /// (shared-dir = source_path, listening on socket_path) before Cloud
    /// Hypervisor, then passes CH `--fs tag=<tag>,socket=<socket_path>`.
    #[serde(default)]
    pub filesystems: Option<Vec<FilesystemIntent>>,
    /// Operator-backed vhost-user devices: vhost-user-blk disks and generic
    /// vhost-user devices. swiftletd hands each to CH (--disk vhost_user=on
    /// for blk; --generic-vhost-user for generic). CH path only.
    #[serde(default)]
    pub vhost_user_devices: Option<Vec<VhostUserDeviceIntent>>,
    /// GPU passthrough configuration. Populated when gpuProfileRef is set.
    #[serde(default)]
    pub gpu: Option<GPUIntent>,
    /// Restore-receive configuration. Populated by the SwiftRestore
    /// controller when this launcher pod is meant to bring up a VM
    /// from a Tier B local snapshot rather than from a fresh boot.
    /// When set, swiftletd skips seed.iso creation, skips the normal
    /// CH spawn path, and instead invokes
    /// `cloud-hypervisor --api-socket=... --restore source_url=file://<path>`.
    /// The VM comes up Paused; the SwiftRestore controller drives the
    /// resume through the snapshot-action annotation surface, the
    /// same path used for every other hypervisor action.
    #[serde(default)]
    pub restore: Option<RestoreIntent>,
    /// vsock device for the in-guest identity agent. Populated by the controller
    /// ONLY for a SOURCE guest that opted into the agent; carries just the CID
    /// (swiftletd computes the socket path from the runtime dir). On `--restore`
    /// the clone reopens the captured vsock device from config.json, so this is
    /// absent on a clone's intent.
    #[serde(default)]
    pub vsock: Option<VsockIntent>,
    /// Live-migration role (Phase 2). Constructed at startup in
    /// `main.rs` from the `KUBESWIFT_MIGRATION_ROLE` env var, NOT
    /// deserialized from the intent JSON file — env-var-driven keeps
    /// the existing intent JSON shape unchanged and isolates the
    /// receiver branch from the Phase 1 pod-builder logic.
    #[serde(skip)]
    pub migration: Option<MigrationIntent>,
}

/// vsock device configuration for swiftletd.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct VsockIntent {
    /// Guest context id (>= 3). Deterministic per guest, derived controller-side
    /// from (namespace, name); rides the snapshot on restore.
    pub cid: u32,
}

/// Restore-receive configuration for swiftletd.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RestoreIntent {
    /// Absolute path to the snapshot directory inside the launcher
    /// container — the controller mounts the on-node hostPath dir
    /// here as readOnly. CH reads `config.json`, `state.json`, and
    /// `memory-ranges` from this directory.
    pub snapshot_path: String,
    /// When true, swiftletd passes `resume=true` on the CH `--restore` (CH v52)
    /// so the guest comes up RUNNING instead of paused. Set only for
    /// cloneFromSnapshot (replaces the resumeCloneIfNeeded round-trip, Bug #73);
    /// SwiftRestore leaves it false and drives resume via its Resuming phase.
    #[serde(default)]
    pub auto_resume: bool,
    /// Cloud Hypervisor `memory_restore_mode` (CH v52): "ondemand"
    /// registers guest memory with userfaultfd so the VM resumes
    /// immediately and pages fault in lazily (cuts restore-to-resume
    /// latency for large guests); "copy" is the eager default. None
    /// omits the field. Set by the controller — cloneFromSnapshot
    /// defaults to "ondemand"; SwiftRestore from spec.memoryRestoreMode.
    #[serde(default)]
    pub memory_restore_mode: Option<String>,
}

/// Live-migration role (Phase 2). When `role == "receiver"`, swiftletd
/// spawns CH with `--api-socket` only (via
/// `swift_ch_client::spawn_ch_receive`) and waits for the action loop
/// to dispatch `vm.receive-migration`. Source-side (`role == "source"`
/// or `migration` absent) uses the normal CH spawn path; the action
/// loop dispatches `vm.send-migration` against the already-running CH.
///
/// Phase 2's manual demo path uses operator-applied launcher-pod YAML
/// to set `KUBESWIFT_MIGRATION_ROLE=receiver` on the destination pod's
/// swiftletd container. Phase 3's controller will set the same env var
/// programmatically when building the destination launcher pod.
#[derive(Debug, Clone)]
pub struct MigrationIntent {
    /// `"receiver"` for the destination pod; any other value (or
    /// absence) means source / not in migration mode. Phase 2 only
    /// has the receiver role as a distinct startup mode — source
    /// pods boot normally, and the migration is initiated via
    /// annotation after the VM is already running.
    pub role: String,
}

impl MigrationIntent {
    /// Returns true if this launcher pod is meant to start in
    /// receiver mode (CH spawned with `--api-socket` only, awaiting
    /// `vm.receive-migration`).
    pub fn is_receiver(&self) -> bool {
        self.role == "receiver"
    }
}

/// Deserialize an explicitly-null field to the type's default. serde's
/// #[serde(default)] only applies when the field is ABSENT; Go marshals nil
/// slices as `null`, which otherwise fails with "invalid type: null".
fn null_to_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Default + serde::Deserialize<'de>,
{
    let opt = Option::<T>::deserialize(d)?;
    Ok(opt.unwrap_or_default())
}

/// GPU passthrough configuration from the controller.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GPUIntent {
    /// VFIO GPU devices to pass through. null-tolerant: a Go writer can emit
    /// `"devices": null` for a nil slice, and plain #[serde(default)] only
    /// covers a MISSING field, not an explicit null (cluster-e2e finding,
    /// 2026-06-12).
    #[serde(default, deserialize_with = "null_to_default")]
    pub devices: Vec<GPUDeviceIntent>,
    /// Where the device list comes from:
    ///   ""    — `devices` above (native backend; controller-time allocation).
    ///   "env" — synthesize from the GPU_PCI_ADDRESSES env var, injected by the
    ///           DRA reference driver's CDI containerEdits at container create
    ///           (the controller cannot know the devices when it writes this
    ///           intent). Clique -1, flat topology in v1. Explicit marker — no
    ///           silent empty-devices magic. (DRA Workstream A, design §A2.)
    #[serde(default)]
    pub device_source: String,
    /// Firmware type: "cloudhv" or "ovmf".
    #[serde(default)]
    pub firmware: String,
    /// Fabric Manager partition ID (-1 = none).
    #[serde(default)]
    pub fabric_manager_partition_id: i32,
}

impl GPUIntent {
    /// Resolve the effective device list: the intent's `devices` (native), or —
    /// when `device_source == "env"` — devices synthesized from the
    /// GPU_PCI_ADDRESSES env var (comma-separated BDFs) that the DRA driver's
    /// CDI containerEdits injected. Fails loudly when the env source is
    /// selected but the variable is missing/empty: a DRA pod without CDI
    /// injection is a node/claim misconfiguration (enable_cdi off, or the
    /// container does not reference the claim), and booting GPU-less would be
    /// a silent failure.
    pub fn resolved_devices(&self) -> Result<Vec<GPUDeviceIntent>, String> {
        if self.device_source != "env" {
            return Ok(self.devices.clone());
        }
        let raw = std::env::var("GPU_PCI_ADDRESSES").unwrap_or_default();
        let bdfs: Vec<&str> = raw
            .split(',')
            .map(|s| s.trim())
            .filter(|s| !s.is_empty())
            .collect();
        if bdfs.is_empty() {
            return Err(
                "gpu.deviceSource=env but GPU_PCI_ADDRESSES is empty — CDI injection \
                 from the DRA driver did not happen (is enable_cdi on, and does the \
                 container reference the claim?)"
                    .to_string(),
            );
        }
        Ok(bdfs
            .into_iter()
            .map(|bdf| GPUDeviceIntent {
                host_path: format!("/sys/bus/pci/devices/{}/", bdf),
                pci_address: bdf.to_string(),
                pcie_root_port: false,
                gpu_direct_clique: -1,
                no_mmap: false,
            })
            .collect())
    }
}

/// A single GPU VFIO device.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GPUDeviceIntent {
    /// Sysfs path (e.g., "/sys/bus/pci/devices/0000:01:00.0/").
    pub host_path: String,
    /// PCI BDF address (e.g., "0000:01:00.0").
    pub pci_address: String,
    /// Place behind pcie-root-port (QEMU Tier 2/3 only).
    #[serde(default)]
    pub pcie_root_port: bool,
    /// x_nv_gpudirect_clique value (CH Tier 1 only).
    #[serde(default)]
    pub gpu_direct_clique: i32,
    /// Add x-no-mmap=true (QEMU, large BARs).
    #[serde(default)]
    pub no_mmap: bool,
}

/// Describes a single virtiofs share (vhost-user-fs). swiftletd runs a
/// virtiofsd backend on `source_path` listening at `socket_path`, then
/// hands Cloud Hypervisor `--fs tag=<tag>,socket=<socket_path>`.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FilesystemIntent {
    /// Per-guest identifier.
    pub name: String,
    /// virtiofs mount tag the guest uses.
    pub tag: String,
    /// In-pod directory virtiofsd shares (--shared-dir). Set by the controller
    /// (the source volume mount path). The unix socket is derived by swiftletd
    /// from the runtime dir (`<run>/<name>.fs.sock`), like the serial/api
    /// sockets — the controller doesn't need to know the run-dir layout.
    pub source_path: String,
    /// Informational: the source volume is mounted read-only by the pod builder
    /// when set (that is the enforcement); swiftletd just logs it.
    #[serde(default)]
    pub read_only: bool,
}

/// An operator-backed vhost-user device (blk or generic). swiftletd hands the
/// socket opaquely to Cloud Hypervisor; the operator runs the backend.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct VhostUserDeviceIntent {
    /// Per-guest identifier.
    pub name: String,
    /// "blk" (vhost-user-blk disk) or "generic" (any vhost-user device).
    pub r#type: String,
    /// Operator backend socket path (mounted into the launcher).
    pub socket: String,
    /// virtio device-type id for a generic device (number or symbolic name).
    #[serde(default)]
    pub virtio_id: Option<String>,
    /// Optional per-queue sizes for a generic device.
    #[serde(default)]
    pub queue_sizes: Option<Vec<u32>>,
}

impl VhostUserDeviceIntent {
    /// True if this is a vhost-user-blk disk.
    pub fn is_blk(&self) -> bool {
        self.r#type == "blk"
    }
}

/// Describes a single network interface for the VM.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct NICIntent {
    /// Interface identifier (matches spec.interfaces[].name).
    pub name: String,
    /// Interface type: "bridge" (tap+bridge+virtio-net) or "sriov" (VFIO passthrough).
    /// Defaults to "bridge" if absent.
    #[serde(default = "default_bridge_type")]
    pub r#type: String,
    /// Tap device name inside the pod namespace (tap0, tap1, etc.)
    /// Empty for SR-IOV interfaces.
    #[serde(default)]
    pub tap_device: String,
    /// MAC address for this interface (bridge type only).
    #[serde(default)]
    pub mac: String,
    /// True if this is the primary NIC with DHCP/dnsmasq.
    #[serde(default)]
    pub primary: bool,
    /// Multus-created interface name (net1, net2, etc.). Empty for primary.
    #[serde(default)]
    pub multus_interface: Option<String>,
    /// Bridge device name (br0, br1, etc.). Empty for SR-IOV.
    #[serde(default)]
    pub bridge: String,
    /// SR-IOV VF device info for VFIO passthrough. Only set when type=sriov.
    #[serde(default)]
    pub sriov_device: Option<SRIOVDeviceIntent>,
    /// In-pod path of the operator's vhost-user-net backend listener socket.
    /// Only set when type=vhost-user; passed to CH as `--net vhost_user=on,socket=`.
    #[serde(default)]
    pub vhost_user_socket: Option<String>,
}

/// SR-IOV VF device info for VFIO passthrough.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SRIOVDeviceIntent {
    /// SR-IOV device plugin resource name (e.g., "intel.com/sriov_netdevice").
    /// Used to find the PCIDEVICE_* env var at runtime.
    pub resource_name: String,
}

fn default_bridge_type() -> String {
    "bridge".to_string()
}

impl NICIntent {
    /// Returns true if this is a bridge-type NIC (tap+bridge+virtio-net).
    pub fn is_bridge(&self) -> bool {
        self.r#type.is_empty() || self.r#type == "bridge"
    }

    /// Returns true if this is an SR-IOV NIC (VFIO passthrough).
    pub fn is_sriov(&self) -> bool {
        self.r#type == "sriov"
    }

    /// Returns true if this is a vhost-user-net NIC (operator backend).
    pub fn is_vhost_user(&self) -> bool {
        self.r#type == "vhost-user"
    }
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RootDisk {
    /// Disk path. Opaque to swiftletd — the value is forwarded
    /// unchanged to Cloud Hypervisor's `--disk path=<value>` argument
    /// (via `swift_ch_client::config::VmConfig::disk_path`). CH opens
    /// regular files and raw block devices identically through this
    /// arg, so the controller-side resolver decides which form to
    /// emit based on the SwiftGuest's resolved
    /// `spec.storage.volumeMode` (W9 — see
    /// `internal/runtimeintent/build.go::Build` for the producer side).
    ///
    /// No path-suffix or extension-based branching exists in this
    /// crate. The W9 PR description records the verbatim grep audit.
    pub path: String,
    /// Disk format. Today always "raw" in practice (the SwiftImage
    /// import pipeline converts qcow2→raw before guest boot). The
    /// field is informational only — swiftletd does not branch on it,
    /// and CH treats every `--disk path=` target as a raw stream.
    pub format: String,
}

/// A secondary data disk entry. Matches the controller-side
/// `internal/runtimeintent.DataDiskSpec`.
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DataDiskSpec {
    /// Stable per-disk name (DNS label). Informational on the runtime
    /// side — used by the controller to name the PVC/volume.
    #[serde(default)]
    pub name: String,
    /// Disk path. Opaque to swiftletd — forwarded unchanged to Cloud
    /// Hypervisor's `--disk path=<value>` argument, exactly like
    /// `RootDisk.path` (a regular raw file on Filesystem PVCs, or a raw
    /// block device on Block PVCs; the controller-side resolver decides).
    pub path: String,
    /// Disk format — informational only (always "raw" in practice).
    #[serde(default)]
    pub format: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct KernelBoot {
    pub kernel_path: String,
    pub initramfs_path: String,
    pub cmdline: String,
}

/// The OCI image as a read-only root block device for a mode-3 sandbox boot.
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SandboxRootfs {
    /// Node-local RO OCI rootfs — opaque (a cached ext4 file or a block device).
    pub path: String,
}

/// The mode-3 workload exec: full argv, merged env ("KEY=VAL"), and working dir.
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SandboxExec {
    #[serde(default)]
    pub argv: Vec<String>,
    #[serde(default)]
    pub env: Vec<String>,
    #[serde(default)]
    pub cwd: String,
}

const B64_ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

// Standard base64 (with '=' padding). Values are base64-encoded on the config disk
// so argv/env may contain spaces, tabs, or newlines (e.g. a multi-line `sh -c`
// script) without breaking the line-based blob; the bridge decodes with `base64 -d`.
fn base64_encode(data: &[u8]) -> String {
    let mut out = String::with_capacity(data.len().div_ceil(3) * 4);
    for chunk in data.chunks(3) {
        let b0 = chunk[0];
        let b1 = *chunk.get(1).unwrap_or(&0);
        let b2 = *chunk.get(2).unwrap_or(&0);
        let n = ((b0 as u32) << 16) | ((b1 as u32) << 8) | (b2 as u32);
        out.push(B64_ALPHABET[((n >> 18) & 63) as usize] as char);
        out.push(B64_ALPHABET[((n >> 12) & 63) as usize] as char);
        out.push(if chunk.len() > 1 {
            B64_ALPHABET[((n >> 6) & 63) as usize] as char
        } else {
            '='
        });
        out.push(if chunk.len() > 2 {
            B64_ALPHABET[(n & 63) as usize] as char
        } else {
            '='
        });
    }
    out
}

impl SandboxExec {
    /// Serializes to the bridge's config-disk blob: the `KUBESWIFT-EXEC-V1` magic,
    /// TAB-separated `CWD`/`ARGV`/`ENV` lines (values base64-encoded), and the
    /// `KUBESWIFT-EXEC-END` sentinel — padded to a 512-byte sector (virtio-blk).
    pub fn to_config_blob(&self) -> Vec<u8> {
        let mut s = String::from("KUBESWIFT-EXEC-V1\n");
        if !self.cwd.is_empty() {
            s.push_str(&format!("CWD\t{}\n", base64_encode(self.cwd.as_bytes())));
        }
        for a in &self.argv {
            s.push_str(&format!("ARGV\t{}\n", base64_encode(a.as_bytes())));
        }
        for e in &self.env {
            s.push_str(&format!("ENV\t{}\n", base64_encode(e.as_bytes())));
        }
        s.push_str("KUBESWIFT-EXEC-END\n");
        let mut b = s.into_bytes();
        // Pad to at least 1 MiB (a multiple of 512, the virtio-blk logical block size).
        // The exec blob is small (a few hundred bytes); 1 MiB just gives the config disk
        // a conventional, non-degenerate size (avoids a 1-sector disk) and is negligible
        // — sparse on the host, and the guest bridge reads only the first 64 KiB.
        const MIN_CONFIG_DISK: usize = 1024 * 1024;
        let rounded = b.len().div_ceil(512) * 512;
        b.resize(rounded.max(MIN_CONFIG_DISK), 0u8);
        b
    }
}

impl RuntimeIntent {
    /// Returns the disk path from intent (no hardcoded path).
    pub fn disk_path(&self) -> &str {
        &self.root_disk.path
    }

    /// Returns the seed path; empty if no seed.
    pub fn seed_path(&self) -> &str {
        &self.seed_path
    }

    /// Returns true if seed is present.
    pub fn has_seed(&self) -> bool {
        !self.seed_path.is_empty()
    }

    /// Returns true if guest has network (TAP, DHCP). Defaults to true when seed present.
    pub fn has_network(&self) -> bool {
        self.network.unwrap_or(self.has_seed())
    }

    /// Returns true if guest boots via direct kernel (not disk).
    pub fn has_kernel(&self) -> bool {
        self.kernel_boot.is_some()
    }

    /// Returns the mode-3 sandbox RO rootfs disk path when set (kernel boot +
    /// an OCI rootfs), else None.
    pub fn sandbox_rootfs_path(&self) -> Option<&str> {
        self.sandbox_rootfs.as_ref().map(|s| s.path.as_str())
    }

    /// Returns the mode-3 sandbox workload exec spec when set.
    pub fn sandbox_exec(&self) -> Option<&SandboxExec> {
        self.sandbox_exec.as_ref()
    }

    /// Returns the ordered list of secondary data-disk paths.
    ///
    /// Version-skew tolerant: a current controller emits `dataDisks[]`,
    /// an older one emits the singular `dataDisk`. When the slice is
    /// present and yields at least one non-empty path it wins; otherwise
    /// the legacy single field is used. Empty paths are skipped.
    pub fn data_disk_paths(&self) -> Vec<String> {
        if let Some(disks) = &self.data_disks {
            let paths: Vec<String> = disks
                .iter()
                .filter(|d| !d.path.is_empty())
                .map(|d| d.path.clone())
                .collect();
            if !paths.is_empty() {
                return paths;
            }
        }
        match &self.data_disk {
            Some(d) if !d.path.is_empty() => vec![d.path.clone()],
            _ => Vec::new(),
        }
    }

    /// Returns true if at least one data disk is attached.
    pub fn has_data_disk(&self) -> bool {
        !self.data_disk_paths().is_empty()
    }

    /// Returns true when this launcher pod is meant to restore a VM
    /// from a local snapshot rather than perform a fresh boot.
    pub fn is_restore(&self) -> bool {
        self.restore.is_some()
    }

    /// Returns true when this launcher pod is meant to start in
    /// migration-receiver mode (Phase 2). swiftletd spawns CH with `--api-socket` only via
    /// `spawn_ch_receive`; the action loop dispatches
    /// `vm.receive-migration` over the API socket once the
    /// destination launcher pod's `migration-action: receive`
    /// annotation arrives.
    ///
    /// Source-side migration is NOT a distinct startup mode — source
    /// pods boot normally and the migration is initiated via
    /// annotation after the VM is already running.
    pub fn is_migration_receiver(&self) -> bool {
        self.migration.as_ref().is_some_and(|m| m.is_receiver())
    }

    /// Returns the snapshot path for restore-receive mode, or empty
    /// string when not restoring.
    pub fn restore_snapshot_path(&self) -> &str {
        match &self.restore {
            Some(r) if !r.snapshot_path.is_empty() => &r.snapshot_path,
            _ => "",
        }
    }

    /// Returns true when the restore should pass `resume=true` to CH (CH v52)
    /// so the guest comes up running (cloneFromSnapshot; replaces Bug #73's
    /// resumeCloneIfNeeded). False for SwiftRestore-driven restores.
    pub fn restore_auto_resume(&self) -> bool {
        self.restore
            .as_ref()
            .map(|r| r.auto_resume)
            .unwrap_or(false)
    }

    /// Returns the CH `memory_restore_mode` ("ondemand"/"copy") when set,
    /// else None (CH uses its native default). cloneFromSnapshot defaults
    /// to "ondemand" (userfaultfd lazy paging); SwiftRestore is driven by
    /// spec.memoryRestoreMode.
    pub fn restore_memory_mode(&self) -> Option<&str> {
        self.restore
            .as_ref()
            .and_then(|r| r.memory_restore_mode.as_deref())
    }

    /// Returns the snapshot URL CH expects on `--restore source_url=`.
    /// Empty when not restoring.
    pub fn restore_source_url(&self) -> String {
        let p = self.restore_snapshot_path();
        if p.is_empty() {
            return String::new();
        }
        // CH wants `file:///abs/path/`. Trailing slash is required by
        // CH's URL parser; tolerate either form in the intent.
        if p.ends_with('/') {
            format!("file://{}", p)
        } else {
            format!("file://{}/", p)
        }
    }

    /// Returns the hypervisor to use. Defaults to "cloud-hypervisor".
    pub fn hypervisor(&self) -> &str {
        match self.hypervisor.as_deref() {
            Some(h) if !h.is_empty() => h,
            _ => "cloud-hypervisor",
        }
    }

    /// Returns true when the guest OS is Windows (osType=windows). Drives
    /// `kvm_hyperv=on` on the Cloud Hypervisor `--cpus` arg.
    pub fn is_windows(&self) -> bool {
        self.os_type.as_deref() == Some("windows")
    }

    /// Returns the NIC list if present and non-empty, or None for legacy single-NIC mode.
    pub fn nics(&self) -> Option<&[NICIntent]> {
        match &self.nics {
            Some(nics) if !nics.is_empty() => Some(nics),
            _ => None,
        }
    }
}

/// Discover VF PCI address from the SR-IOV device plugin environment variable.
/// The device plugin sets PCIDEVICE_<RESOURCE>=<BDF> where the resource name
/// is uppercased and . / are replaced with _.
/// If multiple VFs are allocated, the value is comma-separated; this returns
/// the nth address (0-indexed).
pub fn discover_sriov_vf_address(resource_name: &str, index: usize) -> Option<String> {
    let env_key = format!(
        "PCIDEVICE_{}",
        resource_name
            .to_uppercase()
            .replace('.', "_")
            .replace('/', "_")
    );
    if let Ok(val) = std::env::var(&env_key) {
        let addrs: Vec<&str> = val.split(',').collect();
        addrs.get(index).map(|s| s.to_string())
    } else {
        log::warn!("SR-IOV env var {} not found", env_key);
        None
    }
}

/// Load runtime intent from the canonical path.
pub fn load_intent(path: &str) -> Result<RuntimeIntent, String> {
    let contents = std::fs::read_to_string(path)
        .map_err(|e| format!("failed to read intent from {}: {}", path, e))?;
    serde_json::from_str(&contents).map_err(|e| format!("invalid intent JSON: {}", e))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_base64_encode_known_vectors() {
        assert_eq!(base64_encode(b""), "");
        assert_eq!(base64_encode(b"f"), "Zg==");
        assert_eq!(base64_encode(b"fo"), "Zm8=");
        assert_eq!(base64_encode(b"foo"), "Zm9v");
        assert_eq!(base64_encode(b"test123"), "dGVzdDEyMw==");
    }

    #[test]
    fn test_sandbox_exec_config_blob() {
        let e = SandboxExec {
            // A multi-line arg (a `sh -c` script) must survive — that's why values
            // are base64-encoded rather than written raw into the line format.
            argv: vec!["/bin/sh".into(), "-c".into(), "echo hi\nline2".into()],
            env: vec!["PATH=/usr/bin:/bin".into()],
            cwd: "/work".into(),
        };
        let blob = e.to_config_blob();
        assert_eq!(blob.len() % 512, 0, "blob must be sector-padded");
        assert!(
            blob.len() >= 1024 * 1024,
            "blob must be padded to >= 1 MiB so it enumerates as a virtio-blk device \
             (a 1-sector disk breaks guest virtio-blk enumeration); got {}",
            blob.len()
        );
        let text = String::from_utf8_lossy(&blob);
        assert!(text.starts_with("KUBESWIFT-EXEC-V1\n"));
        assert!(text.contains("KUBESWIFT-EXEC-END\n"));
        assert!(text.contains(&format!("CWD\t{}\n", base64_encode(b"/work"))));
        assert!(text.contains(&format!("ARGV\t{}\n", base64_encode(b"echo hi\nline2"))));
        assert!(text.contains(&format!("ENV\t{}\n", base64_encode(b"PATH=/usr/bin:/bin"))));
        // No raw newline leaked from the multi-line arg into the line structure:
        // exactly the magic + CWD + 3 ARGV + 1 ENV + END = 7 content lines.
        let content_lines = text.trim_end_matches('\u{0}').lines().count();
        assert_eq!(content_lines, 7, "unexpected line count: {text:?}");
    }

    #[test]
    fn test_intent_os_type_windows() {
        let win: RuntimeIntent = serde_json::from_str(
            r#"{"rootDisk":{"path":"/d/i.raw","format":"raw"},"seedPath":"","cpu":2,"memory":4096,"lifecycle":"start","guestId":"default/w","osType":"windows"}"#,
        )
        .unwrap();
        assert!(win.is_windows(), "osType=windows should be is_windows()");

        // Absent osType (legacy) -> not Windows.
        let lin: RuntimeIntent = serde_json::from_str(
            r#"{"rootDisk":{"path":"/d/i.raw","format":"raw"},"seedPath":"","cpu":2,"memory":2048,"lifecycle":"start","guestId":"default/l"}"#,
        )
        .unwrap();
        assert!(
            !lin.is_windows(),
            "absent osType should not be is_windows()"
        );
    }

    // Model A: the Go controller emits the top-level key `primaryUDNInterface`
    // (UDN acronym upper-case). The struct's rename_all=camelCase would otherwise
    // expect `primaryUdnInterface` and silently drop it to None — which on a real
    // cluster skips the CH --net mac override and the guest never gets a UDN IP.
    // This round-trips the EXACT Go-emitted key (the gap a cross-side test closes).
    #[test]
    fn test_intent_primary_udn_interface_go_key() {
        let intent: RuntimeIntent = serde_json::from_str(
            r#"{"rootDisk":{"path":"/d/i.raw","format":"raw"},"seedPath":"","cpu":2,"memory":2048,"lifecycle":"start","guestId":"model-a/g","network":true,"primaryUDNInterface":"ovn-udn1"}"#,
        )
        .unwrap();
        assert_eq!(
            intent.primary_udn_interface.as_deref(),
            Some("ovn-udn1"),
            "the Go-emitted `primaryUDNInterface` key must deserialize into primary_udn_interface"
        );

        // Absent (every non-Model-A guest) -> None.
        let plain: RuntimeIntent = serde_json::from_str(
            r#"{"rootDisk":{"path":"/d/i.raw","format":"raw"},"seedPath":"","cpu":2,"memory":2048,"lifecycle":"start","guestId":"default/g","network":true}"#,
        )
        .unwrap();
        assert_eq!(plain.primary_udn_interface, None);
    }

    #[test]
    fn test_data_disk_paths_new_slice() {
        // New controller: dataDisks[] is honored in order.
        let intent: RuntimeIntent = serde_json::from_str(
            r#"{"rootDisk":{"path":"/d/i.raw","format":"raw"},"seedPath":"","cpu":2,"memory":2048,"lifecycle":"start","guestId":"default/g",
                "dataDisks":[{"name":"data","path":"/disks/data/image.raw","format":"raw"},{"name":"blank0","path":"/dev/kubeswift-data-blank0","format":"raw"}]}"#,
        )
        .unwrap();
        assert_eq!(
            intent.data_disk_paths(),
            vec![
                "/disks/data/image.raw".to_string(),
                "/dev/kubeswift-data-blank0".to_string()
            ]
        );
        assert!(intent.has_data_disk());
    }

    #[test]
    fn test_data_disk_paths_legacy_single() {
        // Old controller: only the singular dataDisk is present.
        let intent: RuntimeIntent = serde_json::from_str(
            r#"{"rootDisk":{"path":"/d/i.raw","format":"raw"},"seedPath":"","cpu":2,"memory":2048,"lifecycle":"start","guestId":"default/g",
                "dataDisk":{"path":"/disks/data/image.raw","format":"raw"}}"#,
        )
        .unwrap();
        assert_eq!(
            intent.data_disk_paths(),
            vec!["/disks/data/image.raw".to_string()]
        );
        assert!(intent.has_data_disk());
    }

    #[test]
    fn test_data_disk_paths_none() {
        let intent: RuntimeIntent = serde_json::from_str(
            r#"{"rootDisk":{"path":"/d/i.raw","format":"raw"},"seedPath":"","cpu":2,"memory":2048,"lifecycle":"start","guestId":"default/g"}"#,
        )
        .unwrap();
        assert!(intent.data_disk_paths().is_empty());
        assert!(!intent.has_data_disk());
    }

    #[test]
    fn test_intent_no_nics() {
        let json = r#"{
            "rootDisk": {"path": "/data/image.raw", "format": "raw"},
            "seedPath": "/data/seed",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/test",
            "network": true
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        assert!(
            intent.nics().is_none(),
            "nics should be None for legacy format"
        );
        assert!(intent.has_network());
    }

    #[test]
    fn test_intent_with_nics() {
        let json = r#"{
            "rootDisk": {"path": "/data/image.raw", "format": "raw"},
            "seedPath": "/data/seed",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/test",
            "network": true,
            "nics": [
                {"name": "mgmt", "tapDevice": "tap0", "mac": "52:54:00:aa:bb:01", "primary": true, "bridge": "br0"},
                {"name": "data", "tapDevice": "tap1", "mac": "52:54:00:aa:bb:02", "primary": false, "multusInterface": "net1", "bridge": "br1"}
            ]
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        let nics = intent.nics().expect("should have nics");
        assert_eq!(nics.len(), 2);
        assert_eq!(nics[0].name, "mgmt");
        assert!(nics[0].primary);
        assert_eq!(nics[0].tap_device, "tap0");
        assert_eq!(nics[1].name, "data");
        assert!(!nics[1].primary);
        assert_eq!(nics[1].multus_interface.as_deref(), Some("net1"));
    }

    #[test]
    fn test_intent_empty_nics() {
        let json = r#"{
            "rootDisk": {"path": "/data/image.raw", "format": "raw"},
            "seedPath": "/data/seed",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/test",
            "network": true,
            "nics": []
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        assert!(intent.nics().is_none(), "empty nics should return None");
    }

    #[test]
    fn test_intent_sriov_type() {
        let json = r#"{
            "rootDisk": {"path": "/data/image.raw", "format": "raw"},
            "seedPath": "/data/seed",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/test",
            "network": true,
            "nics": [
                {"name": "mgmt", "type": "bridge", "tapDevice": "tap0", "mac": "52:54:00:aa:bb:01", "primary": true, "bridge": "br0"},
                {"name": "rdma", "type": "sriov", "sriovDevice": {"resourceName": "intel.com/sriov_netdevice"}}
            ]
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        let nics = intent.nics().expect("should have nics");
        assert_eq!(nics.len(), 2);
        assert!(nics[0].is_bridge());
        assert!(!nics[0].is_sriov());
        assert!(nics[1].is_sriov());
        assert!(!nics[1].is_bridge());
        assert_eq!(
            nics[1].sriov_device.as_ref().unwrap().resource_name,
            "intel.com/sriov_netdevice"
        );
    }

    #[test]
    fn test_intent_default_type() {
        // NICIntent without explicit type defaults to bridge
        let json = r#"{
            "rootDisk": {"path": "/data/image.raw", "format": "raw"},
            "seedPath": "/data/seed",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/test",
            "network": true,
            "nics": [
                {"name": "mgmt", "tapDevice": "tap0", "mac": "52:54:00:aa:bb:01", "primary": true, "bridge": "br0"}
            ]
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        let nics = intent.nics().unwrap();
        assert!(nics[0].is_bridge(), "default type should be bridge");
    }

    #[test]
    fn test_intent_no_restore_by_default() {
        let json = r#"{
            "rootDisk": {"path": "/data/image.raw", "format": "raw"},
            "seedPath": "/data/seed",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/test"
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        assert!(!intent.is_restore());
        assert!(intent.restore_snapshot_path().is_empty());
        assert!(intent.restore_source_url().is_empty());
    }

    #[test]
    fn test_intent_sandbox_rootfs_go_wire_contract() {
        // The exact JSON the Go SwiftSandbox controller emits for a mode-3 boot:
        // kernelBoot + sandboxRootfs, no root disk, no seed. Pins the wire
        // contract (camelCase keys; #[serde(default)] absence -> None) — the
        // class of bug that bit primaryUDNInterface and the DRA null-vs-missing.
        let json = r#"{
            "rootDisk": {"path": "", "format": ""},
            "seedPath": "",
            "cpu": 2, "memory": 512,
            "lifecycle": "start",
            "guestId": "default/agent-123",
            "kernelBoot": {
                "kernelPath": "/var/lib/kubeswift/kernels/default-sandbox/bzImage",
                "initramfsPath": "/var/lib/kubeswift/kernels/default-sandbox/rootfs.cpio.gz",
                "cmdline": "console=ttyS0 kubeswift.rootfs=block kubeswift.entrypoint=/bin/sh"
            },
            "sandboxRootfs": {"path": "/var/lib/kubeswift/sandbox-rootfs/sha256-abc.ext4"}
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        assert!(intent.has_kernel());
        assert_eq!(
            intent.sandbox_rootfs_path(),
            Some("/var/lib/kubeswift/sandbox-rootfs/sha256-abc.ext4")
        );
        // Absent sandboxRootfs -> None (a plain faas kernel boot).
        let faas = r#"{
            "rootDisk": {"path": "", "format": ""},
            "seedPath": "",
            "cpu": 1, "memory": 256,
            "lifecycle": "start",
            "guestId": "default/faas",
            "kernelBoot": {"kernelPath": "/k/bzImage", "initramfsPath": "/k/rootfs.cpio.gz", "cmdline": "console=ttyS0"}
        }"#;
        let f: RuntimeIntent = serde_json::from_str(faas).unwrap();
        assert!(f.has_kernel());
        assert_eq!(f.sandbox_rootfs_path(), None);
    }

    #[test]
    fn test_intent_restore_with_trailing_slash() {
        let json = r#"{
            "rootDisk": {"path": "", "format": ""},
            "seedPath": "",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/restored",
            "restore": {"snapshotPath": "/var/lib/kubeswift/snapshots/default-snap1/"}
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        assert!(intent.is_restore());
        assert_eq!(
            intent.restore_snapshot_path(),
            "/var/lib/kubeswift/snapshots/default-snap1/"
        );
        assert_eq!(
            intent.restore_source_url(),
            "file:///var/lib/kubeswift/snapshots/default-snap1/"
        );
        // autoResume absent -> false (SwiftRestore path).
        assert!(!intent.restore_auto_resume());
    }

    #[test]
    fn test_intent_restore_auto_resume() {
        // cloneFromSnapshot sets autoResume -> swiftletd passes resume=true.
        let intent: RuntimeIntent = serde_json::from_str(
            r#"{"rootDisk":{"path":"","format":""},"seedPath":"","cpu":2,"memory":2048,"lifecycle":"start","guestId":"default/clone","restore":{"snapshotPath":"/snap/","autoResume":true}}"#,
        )
        .unwrap();
        assert!(intent.restore_auto_resume());
    }

    #[test]
    fn test_intent_restore_without_trailing_slash_normalizes() {
        // Tolerate intent that omits the trailing slash; restore URL
        // always carries one (CH's URL parser requires it).
        let json = r#"{
            "rootDisk": {"path": "", "format": ""},
            "seedPath": "",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/restored",
            "restore": {"snapshotPath": "/var/lib/kubeswift/snapshots/default-snap1"}
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        assert_eq!(
            intent.restore_source_url(),
            "file:///var/lib/kubeswift/snapshots/default-snap1/"
        );
    }

    #[test]
    fn test_intent_empty_restore_snapshot_path_treated_as_no_restore() {
        let json = r#"{
            "rootDisk": {"path": "", "format": ""},
            "seedPath": "",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/restored",
            "restore": {"snapshotPath": ""}
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        // is_restore() is purely structural — the field is set, even
        // if empty. The launcher's run_ch_restore rejects empty source
        // URLs at runtime, so we don't second-guess it here.
        assert!(intent.is_restore());
        // ...but the URL is empty, which run_ch_restore checks.
        assert!(intent.restore_source_url().is_empty());
    }

    // W9 Component 3 — a Block-mode SwiftGuest produces a RuntimeIntent
    // whose RootDisk.path is the in-pod block device path
    // (`/dev/kubeswift-root`). The controller emits the path that way
    // (see internal/runtimeintent/build.go::Build); swiftletd
    // deserializes it transparently and forwards it unchanged to CH.
    // This test pins the deserialization contract and the disk_path()
    // accessor's pass-through behaviour.

    /// W9 — Block-mode root disk path deserializes correctly and
    /// `disk_path()` returns the device path verbatim. No suffix
    /// detection, no path-shape interpretation; the string is
    /// opaque all the way to CH's --disk path= argument.
    #[test]
    fn test_intent_block_mode_root_disk_path() {
        let json = r#"{
            "rootDisk": {"path": "/dev/kubeswift-root", "format": "raw"},
            "seedPath": "/data/seed",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/block-guest",
            "network": true
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        assert_eq!(
            intent.disk_path(),
            "/dev/kubeswift-root",
            "Block-mode disk_path should pass through verbatim"
        );
        assert_eq!(intent.root_disk.format, "raw");
    }

    /// W9 — Filesystem-mode disk path is unchanged (regression gate).
    /// The default and overwhelming-majority case must continue to
    /// produce the legacy filesystem path.
    #[test]
    fn test_intent_filesystem_mode_root_disk_path_unchanged() {
        let json = r#"{
            "rootDisk": {"path": "/var/lib/kubeswift/disks/root/image.raw", "format": "raw"},
            "seedPath": "/data/seed",
            "cpu": 2, "memory": 2048,
            "lifecycle": "start",
            "guestId": "default/fs-guest",
            "network": true
        }"#;
        let intent: RuntimeIntent = serde_json::from_str(json).unwrap();
        assert_eq!(
            intent.disk_path(),
            "/var/lib/kubeswift/disks/root/image.raw"
        );
    }

    /// DRA Workstream A: resolved_devices() — native passthrough, env synthesis,
    /// and the fail-loud missing-CDI-injection case. One test (not three): the
    /// GPU_PCI_ADDRESSES env var is process-global and cargo runs tests in
    /// parallel threads, so all phases run sequentially here.
    #[test]
    fn gpu_intent_resolved_devices() {
        // Native (deviceSource empty): pass the intent's list through.
        let native: GPUIntent = serde_json::from_str(
            r#"{"devices":[{"hostPath":"/sys/bus/pci/devices/0000:41:00.0/",
                "pciAddress":"0000:41:00.0","gpuDirectClique":0}],
                "firmware":"cloudhv","fabricManagerPartitionId":-1}"#,
        )
        .unwrap();
        let devs = native.resolved_devices().unwrap();
        assert_eq!(devs.len(), 1);
        assert_eq!(devs[0].pci_address, "0000:41:00.0");
        assert_eq!(devs[0].gpu_direct_clique, 0);

        // DRA (deviceSource=env) with no env: fail loudly, never boot GPU-less.
        let dra: GPUIntent = serde_json::from_str(
            r#"{"devices":[],"deviceSource":"env","firmware":"cloudhv",
                "fabricManagerPartitionId":-1}"#,
        )
        .unwrap();
        std::env::remove_var("GPU_PCI_ADDRESSES");
        let err = dra.resolved_devices().unwrap_err();
        assert!(err.contains("GPU_PCI_ADDRESSES is empty"), "got: {err}");

        // DRA with the CDI-injected env: synthesize devices (clique -1).
        std::env::set_var("GPU_PCI_ADDRESSES", "0000:01:00.0, 0000:02:00.0");
        let devs = dra.resolved_devices().unwrap();
        std::env::remove_var("GPU_PCI_ADDRESSES");
        assert_eq!(devs.len(), 2);
        assert_eq!(devs[0].pci_address, "0000:01:00.0");
        assert_eq!(devs[0].host_path, "/sys/bus/pci/devices/0000:01:00.0/");
        assert_eq!(devs[0].gpu_direct_clique, -1);
        assert!(!devs[0].pcie_root_port);
        assert_eq!(devs[1].pci_address, "0000:02:00.0");
    }

    /// Regression (cluster-e2e 2026-06-12): the Go controller can emit
    /// `"devices": null` (nil slice); deserialization must treat it as empty,
    /// not fail with "invalid type: null".
    #[test]
    fn gpu_intent_null_devices() {
        let dra: GPUIntent = serde_json::from_str(
            r#"{"devices":null,"deviceSource":"env","firmware":"cloudhv",
                "fabricManagerPartitionId":-1}"#,
        )
        .unwrap();
        assert!(dra.devices.is_empty());
        assert_eq!(dra.device_source, "env");
    }
}

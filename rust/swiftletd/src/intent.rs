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
    /// Hypervisor to use: "cloud-hypervisor" (default) or "qemu".
    /// Empty or absent means Cloud Hypervisor.
    #[serde(default)]
    pub hypervisor: Option<String>,
    /// Guest OS family: "windows" or "linux"/absent (default). When "windows",
    /// the CH disk-boot path adds `kvm_hyperv=on` to `--cpus` (the one runtime
    /// setting the spike proved Windows needs — without it the kernel hangs in
    /// early MP/HAL init). See docs/design/windows-guest-support-spike.md.
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
    /// Live-migration role (Phase 2). Constructed at startup in
    /// `main.rs` from the `KUBESWIFT_MIGRATION_ROLE` env var, NOT
    /// deserialized from the intent JSON file — env-var-driven keeps
    /// the existing intent JSON shape unchanged and isolates the
    /// receiver branch from the Phase 1 pod-builder logic. See
    /// `docs/design/live-migration-phase-2.md` §4.3.2 for the
    /// rationale.
    #[serde(skip)]
    pub migration: Option<MigrationIntent>,
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
    /// migration-receiver mode (Phase 2 — `docs/design/live-migration-phase-2.md`
    /// §4.3.2). swiftletd spawns CH with `--api-socket` only via
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

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
    /// Optional secondary data disk (appears as /dev/vdb in guest).
    #[serde(default)]
    pub data_disk: Option<RootDisk>,
    /// Network interface list for multi-NIC support.
    /// If empty/absent and network=true, a single default NIC is used (backward compat).
    #[serde(default)]
    pub nics: Option<Vec<NICIntent>>,
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

/// GPU passthrough configuration from the controller.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GPUIntent {
    /// VFIO GPU devices to pass through.
    pub devices: Vec<GPUDeviceIntent>,
    /// Firmware type: "cloudhv" or "ovmf".
    #[serde(default)]
    pub firmware: String,
    /// Fabric Manager partition ID (-1 = none).
    #[serde(default)]
    pub fabric_manager_partition_id: i32,
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
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RootDisk {
    pub path: String,
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

    /// Returns the data disk path, or empty string if no data disk.
    pub fn data_disk_path(&self) -> &str {
        match &self.data_disk {
            Some(d) if !d.path.is_empty() => &d.path,
            _ => "",
        }
    }

    /// Returns true if a data disk is attached.
    pub fn has_data_disk(&self) -> bool {
        !self.data_disk_path().is_empty()
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
}

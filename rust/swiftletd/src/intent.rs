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
}

/// Describes a single network interface for the VM.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct NICIntent {
    /// Interface identifier (matches spec.interfaces[].name).
    pub name: String,
    /// Tap device name inside the pod namespace (tap0, tap1, etc.)
    pub tap_device: String,
    /// MAC address for this interface.
    pub mac: String,
    /// True if this is the primary NIC with DHCP/dnsmasq.
    pub primary: bool,
    /// Multus-created interface name (net1, net2, etc.). Empty for primary.
    #[serde(default)]
    pub multus_interface: Option<String>,
    /// Bridge device name (br0, br1, etc.)
    pub bridge: String,
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
}

//! Refuse a memory snapshot of a VM that has a passed-through (VFIO) device.
//!
//! Cloud Hypervisor v54 snapshots a VFIO device without refusing. It saves
//! the device's internal state only when the device supports VFIO migration
//! v2; for the generic `vfio-pci` driver it saves PCI config and MSI state
//! and nothing else. A guest restored from such a snapshot sees a device
//! whose state does not match what its driver last wrote: with a GPU whose
//! driver held state, CUDA hung after restore. Live migration is refused by
//! Cloud Hypervisor itself; snapshot is not, so swiftletd refuses it here.
//!
//! The check reads the devices from Cloud Hypervisor's own `vm.info`, so it
//! covers every way a device gets there: a SwiftGPUProfile, a DRA claim, an
//! SR-IOV interface. It does not depend on the (default-off) admission
//! webhook.
//!
//! Every VFIO device is refused, including ones whose variant driver
//! implements migration v2: none has been validated with KubeSwift yet.

use std::path::Path;

use serde_json::Value;

/// Returns why a memory snapshot of this VM must be refused, or `None` when
/// it has no VFIO device. `vm_info` is the JSON from `GET /api/v1/vm.info`.
pub fn snapshot_refusal(
    vm_info: &Value,
    driver_of: impl Fn(&str) -> Option<String>,
) -> Option<String> {
    let config = vm_info.get("config")?;
    let mut found: Vec<String> = Vec::new();
    for dev in config
        .get("devices")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let path = dev.get("path").and_then(Value::as_str).unwrap_or("");
        let bdf = Path::new(path.trim_end_matches('/'))
            .file_name()
            .and_then(|n| n.to_str())
            .unwrap_or(path)
            .to_string();
        let driver = driver_of(path).unwrap_or_else(|| "unknown driver".to_string());
        found.push(format!("{} ({})", bdf, driver));
    }
    for dev in config
        .get("user_devices")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let socket = dev.get("socket").and_then(Value::as_str).unwrap_or("?");
        found.push(format!("vfio-user {}", socket));
    }
    if found.is_empty() {
        return None;
    }
    Some(format!(
        "memory snapshot refused: the VM has passed-through device(s) {}; Cloud Hypervisor would \
         save them without their device state and a restore can hang the guest's driver",
        found.join(", ")
    ))
}

/// The kernel driver bound to a PCI device, from its sysfs `driver` link.
pub fn sysfs_driver(device_path: &str) -> Option<String> {
    let link =
        std::fs::read_link(Path::new(device_path.trim_end_matches('/')).join("driver")).ok()?;
    link.file_name()?.to_str().map(str::to_string)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn vfio_pci(_: &str) -> Option<String> {
        Some("vfio-pci".to_string())
    }

    #[test]
    fn no_devices_is_allowed() {
        let info = json!({"state": "Running", "config": {"cpus": {}, "devices": null}});
        assert_eq!(snapshot_refusal(&info, vfio_pci), None);
        let info = json!({"state": "Running", "config": {"devices": []}});
        assert_eq!(snapshot_refusal(&info, vfio_pci), None);
    }

    #[test]
    fn gpu_is_refused_with_address_and_driver() {
        // Shape from Cloud Hypervisor v54 vm.info with a GPU attached.
        let info = json!({"config": {"devices": [{
            "id": "_vfio4", "pci_segment": 0,
            "path": "/sys/bus/pci/devices/0000:01:00.0/",
            "x_nv_gpudirect_clique": 0
        }]}});
        let msg = snapshot_refusal(&info, vfio_pci).expect("refused");
        assert!(msg.contains("0000:01:00.0 (vfio-pci)"), "{msg}");
    }

    #[test]
    fn every_device_is_listed_and_unknown_driver_still_refused() {
        let info = json!({"config": {
            "devices": [
                {"path": "/sys/bus/pci/devices/0000:01:00.0/"},
                {"path": "/sys/bus/pci/devices/0000:01:00.1"}
            ],
            "user_devices": [{"socket": "/run/vfu.sock"}]
        }});
        let msg = snapshot_refusal(&info, |_| None).expect("refused");
        assert!(msg.contains("0000:01:00.0 (unknown driver), 0000:01:00.1 (unknown driver), vfio-user /run/vfu.sock"), "{msg}");
    }

    #[test]
    fn sysfs_driver_reads_link_name() {
        let dir = std::env::temp_dir().join(format!("vfio-guard-{}", std::process::id()));
        let dev = dir.join("0000:01:00.0");
        std::fs::create_dir_all(&dev).unwrap();
        std::os::unix::fs::symlink("../../../bus/pci/drivers/vfio-pci", dev.join("driver"))
            .unwrap();
        let p = format!("{}/", dev.display());
        assert_eq!(sysfs_driver(&p).as_deref(), Some("vfio-pci"));
        assert_eq!(sysfs_driver(dir.join("missing").to_str().unwrap()), None);
        std::fs::remove_dir_all(&dir).unwrap();
    }
}

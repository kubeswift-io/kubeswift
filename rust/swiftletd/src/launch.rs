//! Hypervisor launch dispatch — Cloud Hypervisor or QEMU based on RuntimeIntent.hypervisor.

use std::path::PathBuf;
use std::time::Duration;

use swift_ch_client::{spawn_ch, wait_for_socket, NICConfig, VFIODeviceConfig, VmConfig};
use swift_qemu_client::{QemuConfig, QemuNICConfig, QemuProcess, QemuVFIODevice};
use swift_runtime::RuntimeDir;

use crate::intent::RuntimeIntent;

/// Remove stale socket files before hypervisor bind.
/// Neither CH nor QEMU clean up on crash; restart fails with "Address in use".
fn remove_stale_sockets(runtime_dir: &RuntimeDir) {
    let _ = std::fs::remove_file(runtime_dir.api_socket()); // ch.sock
    let _ = std::fs::remove_file(runtime_dir.root().join("serial.sock"));
    let _ = std::fs::remove_file(runtime_dir.root().join("qmp.sock"));
}

/// Dispatch VM launch to Cloud Hypervisor or QEMU based on `intent.hypervisor`.
///
/// Calls `on_socket_ready(pid, serial_socket_path)` once the hypervisor is ready
/// (API/QMP socket appeared). Returns `(exit_status, pid, serial_socket_path)`.
pub fn run<F>(
    intent: &RuntimeIntent,
    runtime_dir: &RuntimeDir,
    on_socket_ready: Option<F>,
) -> Result<(std::process::ExitStatus, u32, String), String>
where
    F: FnOnce(u32, String, String),
{
    remove_stale_sockets(runtime_dir);
    match intent.hypervisor() {
        "qemu" => run_qemu(intent, runtime_dir, on_socket_ready),
        _ => run_ch(intent, runtime_dir, on_socket_ready),
    }
}

// ─── Cloud Hypervisor path ───────────────────────────────────────────────────

fn run_ch<F>(
    intent: &RuntimeIntent,
    runtime_dir: &RuntimeDir,
    on_socket_ready: Option<F>,
) -> Result<(std::process::ExitStatus, u32, String), String>
where
    F: FnOnce(u32, String, String),
{
    let seed_path = if intent.has_seed() {
        runtime_dir
            .root()
            .join("seed.iso")
            .to_string_lossy()
            .to_string()
    } else {
        String::new()
    };

    let serial_socket_path = runtime_dir
        .root()
        .join("serial.sock")
        .to_string_lossy()
        .to_string();

    let data_disk_path = intent.data_disk_path().to_string();

    // Build NIC config from intent.
    let (tap_name, ch_nics, vfio_devices) = build_ch_nics(intent);

    let config = if intent.has_kernel() {
        let kb = intent.kernel_boot.as_ref().unwrap();
        VmConfig {
            disk_path: String::new(),
            memory_mib: intent.memory.max(128),
            cpus: intent.cpu.max(1),
            api_socket: runtime_dir.api_socket().to_string_lossy().to_string(),
            seed_path: String::new(),
            serial_socket_path: Some(serial_socket_path.clone()),
            firmware_path: None,
            tap_name,
            nics: ch_nics,
            kernel_path: Some(kb.kernel_path.clone()),
            initramfs_path: Some(kb.initramfs_path.clone()),
            kernel_cmdline: Some(kb.cmdline.clone()),
            data_disk_path: data_disk_path.clone(),
            vfio_devices,
        }
    } else {
        VmConfig {
            disk_path: intent.disk_path().to_string(),
            memory_mib: intent.memory.max(128),
            cpus: intent.cpu.max(1),
            api_socket: runtime_dir.api_socket().to_string_lossy().to_string(),
            seed_path,
            serial_socket_path: Some(serial_socket_path.clone()),
            firmware_path: Some("/usr/share/kubeswift-firmware/CLOUDHV.fd".to_string()),
            tap_name,
            nics: ch_nics,
            kernel_path: None,
            initramfs_path: None,
            kernel_cmdline: None,
            data_disk_path: data_disk_path.clone(),
            vfio_devices,
        }
    };

    let args = config.to_args();
    log::info!("spawning cloud-hypervisor args={:?}", args);

    let mut child =
        spawn_ch(&config).map_err(|e| format!("failed to spawn cloud-hypervisor: {}", e))?;
    let pid = child.id();

    // Wait for CH to create the API socket (signals VM is initialising)
    wait_for_socket(&runtime_dir.api_socket(), Duration::from_secs(30))?;

    if let Some(cb) = on_socket_ready {
        cb(
            pid,
            serial_socket_path.clone(),
            "cloud-hypervisor".to_string(),
        );
    }

    let status = child.wait().map_err(|e| format!("wait failed: {}", e))?;
    Ok((status, pid, serial_socket_path))
}

// ─── QEMU path ───────────────────────────────────────────────────────────────

fn run_qemu<F>(
    intent: &RuntimeIntent,
    runtime_dir: &RuntimeDir,
    on_socket_ready: Option<F>,
) -> Result<(std::process::ExitStatus, u32, String), String>
where
    F: FnOnce(u32, String, String),
{
    let serial_socket = runtime_dir.root().join("serial.sock");
    let qmp_socket = runtime_dir.root().join("qmp.sock");
    let ovmf_vars = runtime_dir.root().join("OVMF_VARS.fd");

    let seed_path = if intent.has_seed() {
        runtime_dir
            .root()
            .join("seed.iso")
            .to_string_lossy()
            .to_string()
    } else {
        String::new()
    };

    // Build NIC config from intent.
    let (tap_name, mac, qemu_nics, vfio_devices) = build_qemu_nics(intent);

    let config = QemuConfig {
        guest_id: intent.guest_id.clone(),
        cpus: intent.cpu.max(1),
        memory_mib: intent.memory.max(128),
        ovmf_code: PathBuf::from("/usr/share/OVMF/OVMF_CODE.fd"),
        ovmf_vars,
        disk_path: intent.disk_path().to_string(),
        seed_path,
        tap_name,
        mac,
        nics: qemu_nics,
        vfio_devices,
        serial_socket: serial_socket.clone(),
        qmp_socket: qmp_socket.clone(),
        data_disk_path: intent.data_disk_path().to_string(),
    };

    let mut process = QemuProcess::spawn(&config)?;
    let pid = process.pid();
    let serial_socket_path = serial_socket.to_string_lossy().to_string();

    // Wait for QEMU to create the QMP socket (signals QEMU is ready to accept connections)
    wait_for_socket(&qmp_socket, Duration::from_secs(30))?;

    if let Some(cb) = on_socket_ready {
        cb(pid, serial_socket_path.clone(), "qemu".to_string());
    }

    let status = process.wait()?;
    Ok((status, pid, serial_socket_path))
}

// ─── NIC helpers ────────────────────────────────────────────────────────────

/// Build Cloud Hypervisor NIC config from intent.
/// Returns (legacy_tap_name, bridge_nics, vfio_devices).
/// Bridge NICs get --net tap=<tap>,mac=<mac>; SR-IOV NICs get --device path=<sysfs>.
fn build_ch_nics(
    intent: &RuntimeIntent,
) -> (Option<String>, Vec<NICConfig>, Vec<VFIODeviceConfig>) {
    if let Some(nics) = intent.nics() {
        let mut ch_nics = vec![];
        let mut vfio_devs = vec![];
        let mut sriov_idx = 0usize;
        for n in nics {
            if n.is_sriov() {
                if let Some(dev) = &n.sriov_device {
                    if let Some(addr) =
                        crate::intent::discover_sriov_vf_address(&dev.resource_name, sriov_idx)
                    {
                        vfio_devs.push(VFIODeviceConfig {
                            sysfs_path: format!("/sys/bus/pci/devices/{}/", addr),
                        });
                        sriov_idx += 1;
                    } else {
                        log::error!(
                            "SR-IOV VF address not found for resource {}",
                            dev.resource_name
                        );
                    }
                }
            } else {
                ch_nics.push(NICConfig {
                    tap_name: n.tap_device.clone(),
                    mac: n.mac.clone(),
                });
            }
        }
        (None, ch_nics, vfio_devs)
    } else if intent.has_network() {
        (Some("tap0".to_string()), vec![], vec![])
    } else {
        (None, vec![], vec![])
    }
}

/// Build QEMU NIC config from intent.
/// Returns (legacy_tap, legacy_mac, bridge_nics, vfio_devices).
fn build_qemu_nics(
    intent: &RuntimeIntent,
) -> (
    Option<String>,
    String,
    Vec<QemuNICConfig>,
    Vec<QemuVFIODevice>,
) {
    if let Some(nics) = intent.nics() {
        let mut qemu_nics = vec![];
        let mut vfio_devs = vec![];
        let mut net_idx = 0usize;
        let mut sriov_idx = 0usize;
        for n in nics {
            if n.is_sriov() {
                if let Some(dev) = &n.sriov_device {
                    if let Some(addr) =
                        crate::intent::discover_sriov_vf_address(&dev.resource_name, sriov_idx)
                    {
                        vfio_devs.push(QemuVFIODevice { host_address: addr });
                        sriov_idx += 1;
                    } else {
                        log::error!(
                            "SR-IOV VF address not found for resource {}",
                            dev.resource_name
                        );
                    }
                }
            } else {
                qemu_nics.push(QemuNICConfig {
                    tap_name: n.tap_device.clone(),
                    mac: n.mac.clone(),
                    netdev_id: format!("net{}", net_idx),
                });
                net_idx += 1;
            }
        }
        (None, String::new(), qemu_nics, vfio_devs)
    } else if intent.has_network() {
        (
            Some("tap0".to_string()),
            "52:54:00:12:34:56".to_string(),
            vec![],
            vec![],
        )
    } else {
        (None, String::new(), vec![], vec![])
    }
}

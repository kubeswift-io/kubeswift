//! Hypervisor launch dispatch — Cloud Hypervisor or QEMU based on RuntimeIntent.hypervisor.

use std::path::PathBuf;
use std::time::Duration;

use swift_ch_client::{spawn_ch, wait_for_socket, VmConfig};
use swift_qemu_client::{QemuConfig, QemuProcess};
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
    F: FnOnce(u32, String),
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
    F: FnOnce(u32, String),
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

    let config = if intent.has_kernel() {
        let kb = intent.kernel_boot.as_ref().unwrap();
        let tap_name = if intent.has_network() {
            Some("tap0".to_string())
        } else {
            None
        };
        VmConfig {
            disk_path: String::new(),
            memory_mib: intent.memory.max(128),
            cpus: intent.cpu.max(1),
            api_socket: runtime_dir.api_socket().to_string_lossy().to_string(),
            seed_path: String::new(),
            serial_socket_path: Some(serial_socket_path.clone()),
            firmware_path: None,
            tap_name,
            kernel_path: Some(kb.kernel_path.clone()),
            initramfs_path: Some(kb.initramfs_path.clone()),
            kernel_cmdline: Some(kb.cmdline.clone()),
        }
    } else {
        let tap_name = if intent.has_network() {
            Some("tap0".to_string())
        } else {
            None
        };
        VmConfig {
            disk_path: intent.disk_path().to_string(),
            memory_mib: intent.memory.max(128),
            cpus: intent.cpu.max(1),
            api_socket: runtime_dir.api_socket().to_string_lossy().to_string(),
            seed_path,
            serial_socket_path: Some(serial_socket_path.clone()),
            firmware_path: Some("/usr/share/kubeswift-firmware/hypervisor-fw".to_string()),
            tap_name,
            kernel_path: None,
            initramfs_path: None,
            kernel_cmdline: None,
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
        cb(pid, serial_socket_path.clone());
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
    F: FnOnce(u32, String),
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

    let config = QemuConfig {
        guest_id: intent.guest_id.clone(),
        cpus: intent.cpu.max(1),
        memory_mib: intent.memory.max(128),
        ovmf_code: PathBuf::from("/usr/share/OVMF/OVMF_CODE.fd"),
        ovmf_vars,
        disk_path: intent.disk_path().to_string(),
        seed_path,
        tap_name: if intent.has_network() {
            Some("tap0".to_string())
        } else {
            None
        },
        // Fixed locally-administered MAC (sufficient for Phase 1 single-VM testing).
        mac: "52:54:00:12:34:56".to_string(),
        serial_socket: serial_socket.clone(),
        qmp_socket: qmp_socket.clone(),
    };

    let mut process = QemuProcess::spawn(&config)?;
    let pid = process.pid();
    let serial_socket_path = serial_socket.to_string_lossy().to_string();

    // Wait for QEMU to create the QMP socket (signals QEMU is ready to accept connections)
    wait_for_socket(&qmp_socket, Duration::from_secs(30))?;

    if let Some(cb) = on_socket_ready {
        cb(pid, serial_socket_path.clone());
    }

    let status = process.wait()?;
    Ok((status, pid, serial_socket_path))
}

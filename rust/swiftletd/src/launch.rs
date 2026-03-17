//! Cloud Hypervisor launch and process monitoring.

use std::time::Duration;

use swift_ch_client::{spawn_ch, wait_for_socket, VmConfig};
use swift_runtime::RuntimeDir;

use crate::intent::RuntimeIntent;

/// Remove stale socket files before CH bind. CH does not clean up on crash; restart fails with "Address in use".
fn remove_stale_sockets(runtime_dir: &RuntimeDir) {
    let api_sock = runtime_dir.api_socket();
    let serial_sock = runtime_dir.root().join("serial.sock");
    let _ = std::fs::remove_file(&api_sock);
    let _ = std::fs::remove_file(&serial_sock);
}

/// Runs the VM: spawns CH, waits for socket, monitors process until exit.
/// Calls `on_socket_ready` when the API socket is available (VM running).
/// Returns (exit_status, pid, serial_socket_path) on success.
pub fn run<F>(
    intent: &RuntimeIntent,
    runtime_dir: &RuntimeDir,
    on_socket_ready: Option<F>,
) -> Result<(std::process::ExitStatus, u32, String), String>
where
    F: FnOnce(u32, String),
{
    // CH expects disk image (ISO), not directory. main.rs creates seed.iso from NoCloud dir.
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

    let tap_name = if intent.has_network() {
        Some("tap0".to_string())
    } else {
        None
    };

    let config = VmConfig {
        disk_path: intent.disk_path().to_string(),
        memory_mib: intent.memory.max(128),
        cpus: intent.cpu.max(1),
        api_socket: runtime_dir.api_socket().to_string_lossy().to_string(),
        seed_path,
        serial_socket_path: Some(serial_socket_path.clone()),
        firmware_path: Some("/usr/share/kubeswift-firmware/hypervisor-fw".to_string()),
        tap_name,
    };

    remove_stale_sockets(runtime_dir);

    let args = config.to_args();
    eprintln!("swiftletd: spawning cloud-hypervisor with args: {:?}", args);

    let mut child =
        spawn_ch(&config).map_err(|e| format!("failed to spawn cloud-hypervisor: {}", e))?;

    let pid = child.id();

    // Wait for CH to create the API socket
    let socket_path = runtime_dir.api_socket();
    wait_for_socket(&socket_path, Duration::from_secs(30))?;

    // VM is running; notify caller (e.g. for status reporting)
    if let Some(cb) = on_socket_ready {
        cb(pid, serial_socket_path.clone());
    }

    // Monitor process until exit
    let status = child.wait().map_err(|e| format!("wait failed: {}", e))?;

    Ok((status, pid, serial_socket_path))
}

//! Cloud Hypervisor launch and process monitoring.

use std::time::Duration;

use swift_ch_client::{spawn_ch, wait_for_socket, VmConfig};
use swift_runtime::RuntimeDir;

use crate::intent::RuntimeIntent;

/// Runs the VM: spawns CH, waits for socket, monitors process until exit.
/// Calls `on_socket_ready` when the API socket is available (VM running).
pub fn run<F>(
    intent: &RuntimeIntent,
    runtime_dir: &RuntimeDir,
    on_socket_ready: Option<F>,
) -> Result<std::process::ExitStatus, String>
where
    F: FnOnce(),
{
    let seed_path = if intent.has_seed() {
        runtime_dir.seed_dir().to_string_lossy().to_string()
    } else {
        String::new()
    };

    let console_path = runtime_dir
        .root()
        .join("console.log")
        .to_string_lossy()
        .to_string();

    let config = VmConfig {
        disk_path: intent.disk_path().to_string(),
        memory_mib: intent.memory.max(128),
        cpus: intent.cpu.max(1),
        api_socket: runtime_dir.api_socket().to_string_lossy().to_string(),
        seed_path,
        console_path: Some(console_path),
    };

    let mut child =
        spawn_ch(&config).map_err(|e| format!("failed to spawn cloud-hypervisor: {}", e))?;

    // Wait for CH to create the API socket
    let socket_path = runtime_dir.api_socket();
    wait_for_socket(&socket_path, Duration::from_secs(30))?;

    // VM is running; notify caller (e.g. for status reporting)
    if let Some(cb) = on_socket_ready {
        cb();
    }

    // Monitor process until exit
    let status = child.wait().map_err(|e| format!("wait failed: {}", e))?;

    Ok(status)
}

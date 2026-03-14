//! Spawn Cloud Hypervisor and connect to socket.

use std::path::Path;
use std::process::Child;
use std::time::Duration;

use crate::config::DEFAULT_CH_BINARY;
use crate::VmConfig;

/// Spawns the Cloud Hypervisor process with the given config.
/// Returns the child process. The API socket will be created by CH when ready.
pub fn spawn_ch(config: &VmConfig) -> Result<Child, std::io::Error> {
    let binary = std::env::var("KUBESWIFT_CH_BINARY")
        .unwrap_or_else(|_| DEFAULT_CH_BINARY.to_string());

    let args = config.to_args();

    std::process::Command::new(&binary)
        .args(&args)
        .spawn()
}

/// Polls until the socket file exists, then returns.
/// Returns Err if timeout is exceeded.
pub fn wait_for_socket(socket_path: &Path, timeout: Duration) -> Result<(), String> {
    let start = std::time::Instant::now();
    while start.elapsed() < timeout {
        if socket_path.exists() {
            return Ok(());
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    Err(format!(
        "timeout waiting for socket at {}",
        socket_path.display()
    ))
}

/// Connects to the Cloud Hypervisor API socket.
/// For MVP: returns a placeholder; full HTTP client over Unix socket can be added later.
/// Used for shutdown_vm and other API calls.
pub fn connect_socket(_socket_path: &Path) -> Result<(), String> {
    // TODO: Implement HTTP client over Unix stream for CH REST API.
    // CH API: GET/POST to socket. Shutdown: POST /vm.shutdown
    Ok(())
}

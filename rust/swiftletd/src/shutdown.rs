//! Graceful guest shutdown on SIGTERM.
//!
//! swiftletd is PID 1 in the launcher container (the entrypoint `exec`s it),
//! and the kernel drops a signal sent to a PID-namespace init that has no
//! handler for it. With no handler installed, every pod deletion — a guest stop,
//! delete, node drain, or the offline-migration source teardown — sent a SIGTERM
//! that was silently ignored; the kubelet waited out the grace period and then
//! SIGKILLed Cloud Hypervisor/QEMU. The guest never saw an ACPI power-off, lost
//! its dirty page cache, and came back needing journal replay or with a damaged
//! filesystem.
//!
//! This installs a SIGTERM handler that presses the guest's ACPI power button.
//! The guest OS then shuts down cleanly, the hypervisor exits, `launch::run`
//! returns on the main thread, and swiftletd reports `VmStopped` and exits
//! normally. If the guest ignores ACPI (a minimal initramfs, a hung OS) nothing
//! changes from before: the kubelet SIGKILLs at the end of the grace period.

use std::path::PathBuf;
use std::time::Duration;

/// How to reach the hypervisor to request a power-off.
pub enum PowerTarget {
    CloudHypervisor { api_socket: PathBuf },
    Qemu { qmp_socket: PathBuf },
}

impl PowerTarget {
    /// Pick the target for the hypervisor named in the intent.
    pub fn for_hypervisor(hypervisor: &str, api_socket: PathBuf, qmp_socket: PathBuf) -> Self {
        if hypervisor == "qemu" {
            PowerTarget::Qemu { qmp_socket }
        } else {
            PowerTarget::CloudHypervisor { api_socket }
        }
    }

    fn request_poweroff(&self) -> Result<(), String> {
        match self {
            PowerTarget::CloudHypervisor { api_socket } => {
                swift_ch_client::ApiClient::new(api_socket.clone())
                    .with_timeout(Duration::from_secs(5))
                    .power_button()
                    .map_err(|e| e.to_string())
            }
            PowerTarget::Qemu { qmp_socket } => swift_qemu_client::request_powerdown(qmp_socket),
        }
    }
}

/// Install the SIGTERM handler on its own thread. Must be called before the
/// main thread blocks on the hypervisor. A repeated SIGTERM presses the button
/// again (harmless; a guest that missed the first press gets another chance).
pub fn spawn_sigterm_handler(target: PowerTarget) {
    let spawned = std::thread::Builder::new()
        .name("sigterm".into())
        .spawn(move || {
            let rt = match tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
            {
                Ok(rt) => rt,
                Err(e) => {
                    log::error!("sigterm handler: tokio runtime: {}", e);
                    return;
                }
            };
            rt.block_on(async move {
                use tokio::signal::unix::{signal, SignalKind};
                let mut term = match signal(SignalKind::terminate()) {
                    Ok(s) => s,
                    Err(e) => {
                        log::error!("sigterm handler: install failed: {}", e);
                        return;
                    }
                };
                while term.recv().await.is_some() {
                    log::info!("sigterm_received; requesting guest ACPI power-off");
                    match target.request_poweroff() {
                        Ok(()) => log::info!("guest_poweroff_requested"),
                        Err(e) => log::warn!(
                            "guest_poweroff_request_failed: {} (the kubelet will SIGKILL at the end of the grace period)",
                            e
                        ),
                    }
                }
            });
        });
    if let Err(e) = spawned {
        log::error!("sigterm handler: thread spawn failed: {}", e);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::os::unix::net::UnixListener;

    #[test]
    fn target_follows_the_intent_hypervisor() {
        let api = PathBuf::from("/run/ch.sock");
        let qmp = PathBuf::from("/run/qmp.sock");
        assert!(matches!(
            PowerTarget::for_hypervisor("cloud-hypervisor", api.clone(), qmp.clone()),
            PowerTarget::CloudHypervisor { .. }
        ));
        assert!(matches!(
            PowerTarget::for_hypervisor("qemu", api, qmp),
            PowerTarget::Qemu { .. }
        ));
    }

    // SIGTERM on a Cloud Hypervisor guest must press the ACPI power button over
    // the API socket (a clean guest shutdown), not vm.shutdown.
    #[test]
    fn cloud_hypervisor_poweroff_presses_the_power_button() {
        let dir = tempfile::tempdir().unwrap();
        let sock = dir.path().join("ch.sock");
        let listener = UnixListener::bind(&sock).unwrap();
        let server = std::thread::spawn(move || {
            let (mut conn, _) = listener.accept().unwrap();
            let mut buf = [0u8; 1024];
            let n = conn.read(&mut buf).unwrap();
            conn.write_all(b"HTTP/1.1 204 No Content\r\n\r\n").unwrap();
            String::from_utf8_lossy(&buf[..n]).into_owned()
        });
        let target = PowerTarget::CloudHypervisor { api_socket: sock };
        target.request_poweroff().unwrap();
        let req = server.join().unwrap();
        assert!(
            req.starts_with("PUT /api/v1/vm.power-button "),
            "unexpected request: {req}"
        );
    }

    // No hypervisor listening (e.g. a migration receiver with no VM yet): the
    // request fails and is only logged; it must not panic.
    #[test]
    fn poweroff_without_a_hypervisor_is_an_error_not_a_panic() {
        let dir = tempfile::tempdir().unwrap();
        let target = PowerTarget::CloudHypervisor {
            api_socket: dir.path().join("absent.sock"),
        };
        assert!(target.request_poweroff().is_err());
    }
}

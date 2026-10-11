//! Guest shutdown on SIGTERM.
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
//! A SwiftGuest ([`Mode::Guest`]): SIGTERM presses the guest's ACPI power
//! button. The guest OS then shuts down cleanly, the hypervisor exits,
//! `launch::run` returns on the main thread, and swiftletd reports `VmStopped`
//! and exits normally. If the guest ignores ACPI (a hung OS) the kubelet
//! SIGKILLs at the end of the grace period.
//!
//! A sandbox ([`Mode::Sandbox`]): the bridge init has no power-button handler,
//! so ACPI would change nothing and the workload kept serving until the
//! kubelet's SIGKILL. Instead SIGTERM marks the sandbox terminating (the probe
//! runner reports the workload not ready and stops), keeps the workload
//! running for its drain (spec.termination.drainPeriod; less if it exits
//! first), then kills the hypervisor. The pod's grace period is the drain plus
//! a stop allowance, so the kubelet's SIGKILL is only the backstop.

use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::time::{Duration, Instant};

/// What SIGTERM does to the guest.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    /// Press the ACPI power button; the guest OS shuts itself down.
    Guest,
    /// Withdraw readiness, drain, then stop the microVM.
    Sandbox,
}

static TERMINATING: AtomicBool = AtomicBool::new(false);
static WORKLOAD_ENDED: AtomicBool = AtomicBool::new(false);
static DRAIN_MS: AtomicU64 = AtomicU64::new(0);
static VMM_PID: AtomicU32 = AtomicU32::new(0);

/// How often the drain checks whether the workload has ended.
const DRAIN_POLL: Duration = Duration::from_millis(50);

/// Whether the sandbox is terminating: SIGTERM has arrived. Probe runners stop
/// and report the workload not ready.
pub fn terminating() -> bool {
    TERMINATING.load(Ordering::SeqCst)
}

/// Sets the drain of the workload that runs now (a cold sandbox's from its
/// intent, a warm slot's from its checkout).
pub fn set_drain(drain: Duration) {
    DRAIN_MS.store(drain.as_millis() as u64, Ordering::SeqCst);
}

/// Records that the workload has exited: a drain in progress ends now.
pub fn workload_ended() {
    WORKLOAD_ENDED.store(true, Ordering::SeqCst);
}

/// Records the hypervisor's process, the one a sandbox termination stops.
pub fn set_vmm_pid(pid: u32) {
    VMM_PID.store(pid, Ordering::SeqCst);
}

/// Waits up to `drain`, or until `ended` is set. Returns whether the workload
/// ended first.
fn wait_drain(drain: Duration, ended: &AtomicBool) -> bool {
    let until = Instant::now() + drain;
    loop {
        if ended.load(Ordering::SeqCst) {
            return true;
        }
        let now = Instant::now();
        if now >= until {
            return false;
        }
        std::thread::sleep(DRAIN_POLL.min(until - now));
    }
}

/// The sandbox termination sequence after SIGTERM: drain, then `stop`.
fn terminate_sandbox(
    drain: Duration,
    ended: &AtomicBool,
    stop: impl FnOnce() -> Result<(), String>,
) {
    log::info!(
        "termination_started drain_ms={}; the workload is out of Service endpoints",
        drain.as_millis()
    );
    let started = Instant::now();
    let ended_early = wait_drain(drain, ended);
    log::info!(
        "termination_drain_complete elapsed_ms={} workload_ended={}",
        started.elapsed().as_millis(),
        ended_early
    );
    match stop() {
        Ok(()) => log::info!("termination_vm_stop_requested"),
        Err(e) => log::warn!(
            "termination_vm_stop_failed: {} (the kubelet will SIGKILL at the end of the grace period)",
            e
        ),
    }
}

/// Kills the hypervisor process. A sandbox's microVM has nothing to flush: its
/// root is read-only and its writes are in guest memory.
fn kill_vmm(pid: u32) -> Result<(), String> {
    if pid == 0 {
        return Err("no hypervisor process".to_string());
    }
    // SAFETY: kill(2) with a plain pid and signal has no memory effects.
    if unsafe { libc::kill(pid as libc::pid_t, libc::SIGKILL) } != 0 {
        return Err(format!("kill {}: {}", pid, std::io::Error::last_os_error()));
    }
    Ok(())
}

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
/// main thread blocks on the hypervisor. For a guest, a repeated SIGTERM
/// presses the button again (harmless; a guest that missed the first press gets
/// another chance). For a sandbox, the first SIGTERM starts the termination and
/// later ones change nothing.
pub fn spawn_sigterm_handler(target: PowerTarget, mode: Mode) {
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
                    if mode == Mode::Sandbox {
                        if TERMINATING.swap(true, Ordering::SeqCst) {
                            continue;
                        }
                        log::info!("sigterm_received; terminating the sandbox");
                        terminate_sandbox(
                            Duration::from_millis(DRAIN_MS.load(Ordering::SeqCst)),
                            &WORKLOAD_ENDED,
                            || kill_vmm(VMM_PID.load(Ordering::SeqCst)),
                        );
                        continue;
                    }
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

    // The drain lasts its full length while the workload runs, then the stop
    // follows.
    #[test]
    fn sandbox_termination_drains_then_stops() {
        let ended = AtomicBool::new(false);
        let started = Instant::now();
        let stopped_at = std::cell::Cell::new(None);
        terminate_sandbox(Duration::from_millis(300), &ended, || {
            stopped_at.set(Some(started.elapsed()));
            Ok(())
        });
        let at = stopped_at.get().expect("stop was not requested");
        assert!(at >= Duration::from_millis(300), "stopped after {at:?}");
        assert!(at < Duration::from_millis(1000), "stopped after {at:?}");
    }

    // A workload that exits during the drain ends it.
    #[test]
    fn sandbox_termination_ends_the_drain_when_the_workload_exits() {
        let ended = std::sync::Arc::new(AtomicBool::new(false));
        let flag = ended.clone();
        let setter = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(100));
            flag.store(true, Ordering::SeqCst);
        });
        let started = Instant::now();
        let mut stopped = false;
        terminate_sandbox(Duration::from_secs(30), &ended, || {
            stopped = true;
            Ok(())
        });
        setter.join().unwrap();
        assert!(stopped);
        assert!(started.elapsed() < Duration::from_secs(2));
    }

    // No drain: the microVM stops at once.
    #[test]
    fn sandbox_termination_without_a_drain_stops_at_once() {
        let ended = AtomicBool::new(false);
        let started = Instant::now();
        let mut stopped = false;
        terminate_sandbox(Duration::ZERO, &ended, || {
            stopped = true;
            Ok(())
        });
        assert!(stopped);
        assert!(started.elapsed() < Duration::from_millis(100));
    }

    // A failed stop is logged, not a panic (the kubelet's SIGKILL follows).
    #[test]
    fn sandbox_termination_survives_a_failed_stop() {
        terminate_sandbox(Duration::ZERO, &AtomicBool::new(false), || {
            Err("no hypervisor process".to_string())
        });
    }

    // The stop kills the hypervisor process, however it handles signals.
    #[test]
    fn kill_vmm_kills_the_process() {
        use std::os::unix::process::ExitStatusExt;
        let mut child = std::process::Command::new("sh")
            .args(["-c", "trap '' TERM INT; sleep 30"])
            .spawn()
            .unwrap();
        kill_vmm(child.id()).unwrap();
        let status = child.wait().unwrap();
        assert_eq!(status.signal(), Some(libc::SIGKILL));
        assert!(kill_vmm(0).is_err());
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

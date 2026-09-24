//! QEMU process management for KubeSwift swiftletd.
//!
//! Provides QemuProcess (spawn, monitor, shutdown) and QemuConfig (arg
//! builder). Covers disk boot plus the Tier-2/3 HGX GPU topology:
//! pcie-root-port per SXM device, x-no-mmap large-BAR handling, NUMA memory
//! backends, hugepages, and post-spawn vCPU pinning.

mod config;
mod pinning;
mod qmp;

pub use config::{
    QemuConfig, QemuNICConfig, QemuNUMANode, QemuVCPUPin, QemuVFIODevice, DEFAULT_QEMU_BINARY,
};

use std::path::{Path, PathBuf};
use std::process::{Child, ExitStatus};
use std::time::Duration;

use qmp::QmpClient;

/// Ask the guest behind a QMP socket to power itself off (ACPI
/// `system_powerdown`) without holding a [`QemuProcess`]. Used by swiftletd's
/// SIGTERM handler, which runs on its own thread while the main thread waits
/// on the child. Returns once QEMU acknowledges the command, not when the guest
/// has finished shutting down.
pub fn request_powerdown(qmp_socket: &Path) -> Result<(), String> {
    QmpClient::new(qmp_socket.to_path_buf()).powerdown()
}

/// Pin a running QEMU's vCPU threads once its QMP monitor answers, waiting up
/// to `wait` for it.
///
/// QEMU creates its QMP socket early but does not answer until machine init is
/// done, and that includes preallocating hugepages and mapping VFIO DMA: for a
/// large GPU guest, minutes. A single short-timeout query gave up long before,
/// and the guest ran unpinned with nothing retrying. Run this off the launch
/// path (it blocks for as long as QEMU takes).
///
/// `allowed` is the host CPUs this process may use (the pod's cpuset); pins
/// outside it are moved (see [`pinning::fit_to_cpuset`]). Returns how many
/// vCPUs were pinned and a note for every pin that had to move or be dropped.
pub fn pin_vcpus_when_ready(
    qmp_socket: &Path,
    pins: &[QemuVCPUPin],
    allowed: Option<&[u32]>,
    wait: Duration,
) -> Result<(usize, Vec<String>), String> {
    if pins.is_empty() {
        return Ok((0, Vec::new()));
    }
    let qmp = QmpClient::new(qmp_socket.to_path_buf());
    let deadline = std::time::Instant::now() + wait;
    let threads = loop {
        match qmp.query_cpus_fast() {
            Ok(t) => break t,
            Err(e) if std::time::Instant::now() >= deadline => {
                return Err(format!("QMP did not answer within {:?}: {}", wait, e));
            }
            Err(_) => std::thread::sleep(Duration::from_secs(2)),
        }
    };
    let (pins, notes) = match allowed {
        Some(cpus) => pinning::fit_to_cpuset(pins, cpus, &pinning::cpu_numa_node),
        None => (pins.to_vec(), Vec::new()),
    };
    let n = pinning::apply_pins(&pins, &threads)?;
    Ok((n, notes))
}

/// Managed QEMU process with lifecycle control via QMP.
pub struct QemuProcess {
    child: Child,
    pid: u32,
    serial_socket: PathBuf,
    qmp: QmpClient,
}

impl QemuProcess {
    /// Spawn QEMU from a QemuConfig.
    ///
    /// Copies the OVMF_VARS template to `config.ovmf_vars` before spawning so each
    /// VM gets its own mutable UEFI variable store.
    pub fn spawn(config: &QemuConfig) -> Result<Self, String> {
        // Copy OVMF_VARS template so each VM has an isolated writable UEFI vars store.
        let vars_template = Path::new("/usr/share/OVMF/OVMF_VARS.fd");
        if vars_template.exists() {
            std::fs::copy(vars_template, &config.ovmf_vars)
                .map_err(|e| format!("copy OVMF_VARS to {:?}: {}", config.ovmf_vars, e))?;
        } else {
            // In test or CI environments OVMF may not be present — log and continue.
            log::warn!(
                "OVMF_VARS template not found at {:?}; skipping copy",
                vars_template
            );
        }

        let binary = std::env::var("KUBESWIFT_QEMU_BINARY")
            .unwrap_or_else(|_| DEFAULT_QEMU_BINARY.to_string());

        let args = config.to_args();
        log::info!("spawning qemu binary={} args={:?}", binary, args);

        let child = std::process::Command::new(&binary)
            .args(&args)
            .spawn()
            .map_err(|e| format!("failed to spawn qemu ({}): {}", binary, e))?;

        let pid = child.id();
        let qmp = QmpClient::new(config.qmp_socket.clone());

        Ok(Self {
            child,
            pid,
            serial_socket: config.serial_socket.clone(),
            qmp,
        })
    }

    /// Returns the OS process ID of the QEMU process.
    pub fn pid(&self) -> u32 {
        self.pid
    }

    /// Apply vCPU→host-CPU pinning post-spawn (QEMU has no CLI for thread
    /// affinity — the libvirt-style flow): QMP query-cpus-fast maps each vCPU
    /// index to its host thread id, then sched_setaffinity pins it. Call once
    /// the QMP socket is up. Returns the number of vCPUs pinned. Callers
    /// treat failure as best-effort: a missed pin degrades performance, never
    /// correctness.
    pub fn apply_vcpu_pinning(&self, pins: &[QemuVCPUPin]) -> Result<usize, String> {
        if pins.is_empty() {
            return Ok(0);
        }
        let cpu_threads = self.qmp.query_cpus_fast()?;
        pinning::apply_pins(pins, &cpu_threads)
    }

    /// Returns the path to the serial console Unix socket.
    pub fn serial_socket(&self) -> &Path {
        &self.serial_socket
    }

    /// Block until the QEMU process exits and return its exit status.
    pub fn wait(&mut self) -> Result<ExitStatus, String> {
        self.child.wait().map_err(|e| format!("wait failed: {}", e))
    }

    /// Graceful shutdown sequence:
    ///   1. QMP system_powerdown → wait 30 s for guest to power off
    ///   2. QMP quit            → wait  5 s
    ///   3. SIGKILL             (last resort)
    pub fn shutdown(&mut self) -> Result<(), String> {
        if let Err(e) = self.qmp.powerdown() {
            log::warn!("qmp powerdown failed: {}; trying quit directly", e);
        } else {
            if self.wait_with_timeout(Duration::from_secs(30)) {
                return Ok(());
            }
            log::info!("qemu did not exit after graceful powerdown; sending quit");
        }

        let _ = self.qmp.quit();
        if self.wait_with_timeout(Duration::from_secs(5)) {
            return Ok(());
        }

        // Last resort: SIGKILL
        let _ = self.child.kill();
        Ok(())
    }

    /// Poll `try_wait` until the process exits or the timeout elapses.
    /// Returns true if the process exited within the timeout.
    fn wait_with_timeout(&mut self, timeout: Duration) -> bool {
        let start = std::time::Instant::now();
        while start.elapsed() < timeout {
            match self.child.try_wait() {
                Ok(Some(_)) => return true,
                Ok(None) => std::thread::sleep(Duration::from_millis(200)),
                Err(_) => return false,
            }
        }
        false
    }
}

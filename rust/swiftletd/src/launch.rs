//! Hypervisor launch dispatch — Cloud Hypervisor or QEMU based on RuntimeIntent.hypervisor.

use std::path::PathBuf;
use std::time::Duration;

use swift_ch_client::{
    spawn_ch, spawn_ch_receive, spawn_ch_restore, wait_for_socket, NICConfig, VFIODeviceConfig,
    VmConfig,
};
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
///
/// Three CH-specific modes branch off before the normal CH/QEMU dispatch:
///
///   - `intent.is_migration_receiver()` → [`run_ch_receive`] (Phase 2 PR-C):
///     CH spawned with `--api-socket` only; awaits action loop's
///     vm.receive-migration dispatch. Skipped if `intent.is_restore()`
///     would also match — receiver and restore are mutually exclusive
///     and the operator-applied launcher pod sets exactly one role.
///   - `intent.is_restore()` → [`run_ch_restore`] (snapshot Phase 2):
///     CH spawned with `--api-socket` + `--restore source_url=...`.
///   - Otherwise → fresh boot via [`run_ch`] or [`run_qemu`].
pub fn run<F>(
    intent: &RuntimeIntent,
    runtime_dir: &RuntimeDir,
    on_socket_ready: Option<F>,
) -> Result<(std::process::ExitStatus, u32, String), String>
where
    F: FnOnce(u32, String, String),
{
    remove_stale_sockets(runtime_dir);
    if intent.is_migration_receiver() {
        return run_ch_receive(intent, runtime_dir, on_socket_ready);
    }
    if intent.is_restore() {
        return run_ch_restore(intent, runtime_dir, on_socket_ready);
    }
    match intent.hypervisor() {
        "qemu" => run_qemu(intent, runtime_dir, on_socket_ready),
        _ => run_ch(intent, runtime_dir, on_socket_ready),
    }
}

// ─── Migration-receive path (Cloud Hypervisor, Phase 2 PR-C) ───────────────

/// Migration-receive mode: bring up CH as an empty VMM awaiting
/// `vm.receive-migration`. The action loop dispatches the
/// receive-migration action over the API socket once the destination
/// launcher pod's `migration-action: receive` annotation arrives.
///
/// Differences from [`run_ch`] and [`run_ch_restore`]:
///
///   - Spawns via [`spawn_ch_receive`] which emits `--api-socket=<path>`
///     ONLY. CH starts without any VM created; the entire VM
///     configuration arrives over the migration wire from the source
///     CH (Q1c finding from
///     `docs/design/live-migration-phase-2-spike.md`).
///   - The migrated VM's disk paths must exist on this pod's
///     filesystem at the SAME paths the source used (Constraint 4
///     from `docs/design/live-migration.md`). Phase 2 manual demo
///     handles this by mounting the same PVC at the same path in
///     the hand-rolled launcher pod YAML.
///   - on_socket_ready callback intentionally fires with the CH PID
///     and serial-socket path even though no guest exists yet; the
///     action loop's vm.receive-migration completion is the actual
///     "guest running" signal (W1 gate from PR-B).
fn run_ch_receive<F>(
    intent: &RuntimeIntent,
    runtime_dir: &RuntimeDir,
    on_socket_ready: Option<F>,
) -> Result<(std::process::ExitStatus, u32, String), String>
where
    F: FnOnce(u32, String, String),
{
    let _ = intent; // intent is currently only used as the discriminator
    let api_socket = runtime_dir.api_socket();
    log::info!(
        "spawning cloud-hypervisor (receive) api_socket={}",
        api_socket.display()
    );
    let mut child = spawn_ch_receive(&api_socket)
        .map_err(|e| format!("failed to spawn cloud-hypervisor (receive): {}", e))?;
    let pid = child.id();

    wait_for_socket(&api_socket, Duration::from_secs(30))?;

    let serial_socket_path = runtime_dir
        .root()
        .join("serial.sock")
        .to_string_lossy()
        .to_string();

    // on_socket_ready is None for receiver mode (set in main.rs) —
    // the migrated VM is not yet running so reporting "guest running"
    // here would be a lie. The action loop's
    // dispatch_migration_receive verifies vm_info.state=Running
    // post-receive (PR-B's W1 gate). Operators verify destination
    // success via that path.
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

// ─── Restore-receive path (Cloud Hypervisor) ────────────────────────────────

/// Restore-receive mode: bring up CH from a Tier B local snapshot
/// rather than from a fresh boot. Differences from [`run_ch`]:
///
///   - No seed.iso construction. The original VM's seed (if any) is
///     baked into the snapshot's memory state; cloud-init has already
///     run.
///   - No VmConfig assembly. CH reads disks, network, kernel, memory,
///     and CPU layout from `config.json` inside the snapshot directory.
///     The launcher only passes `--api-socket=...` and `--restore
///     source_url=file://<path>/`.
///   - The VM comes up Paused. The SwiftRestore controller (commit 10)
///     drives the resume through the snapshot-action annotation surface,
///     same as every other hypervisor action — there is intentionally
///     no inline resume here.
///   - Network-init and gpu-init still run as init containers (the new
///     pod has its own netns and host-side tap/bridge/dnsmasq must be
///     re-created); restore-receive is a launcher-process-mode change,
///     not a pod-shape change. The validation webhook rejects
///     gpuProfileRef + memory snapshot upfront so gpu-init never runs
///     for a real restore in practice (Phase 0 Constraint #1).
///
/// The serial socket path returned is the runtime-dir-local path. CH
/// during restore re-uses paths from the snapshot's config.json, so if
/// the snapshot was taken on a pod with a different runtime-dir,
/// `swiftctl console` may not bind. The controller's config.json
/// patching (commit 12) keeps paths in sync; this function does not
/// re-validate.
fn run_ch_restore<F>(
    intent: &RuntimeIntent,
    runtime_dir: &RuntimeDir,
    on_socket_ready: Option<F>,
) -> Result<(std::process::ExitStatus, u32, String), String>
where
    F: FnOnce(u32, String, String),
{
    let api_socket = runtime_dir.api_socket();
    let source_url = intent.restore_source_url();
    if source_url.is_empty() {
        return Err("restore intent has empty snapshot path".to_string());
    }
    log::info!(
        "spawning cloud-hypervisor (restore) api_socket={} source_url={}",
        api_socket.display(),
        source_url
    );
    let mut child = spawn_ch_restore(&api_socket, &source_url)
        .map_err(|e| format!("failed to spawn cloud-hypervisor (restore): {}", e))?;
    let pid = child.id();

    wait_for_socket(&api_socket, Duration::from_secs(30))?;

    let serial_socket_path = runtime_dir
        .root()
        .join("serial.sock")
        .to_string_lossy()
        .to_string();

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

    // Build NIC config from intent (SR-IOV NICs produce VFIO devices).
    let (tap_name, ch_nics, mut vfio_devices) = build_ch_nics(intent);

    // Add GPU VFIO devices from the GPU intent.
    if let Some(ref gpu) = intent.gpu {
        for dev in &gpu.devices {
            log::info!(
                "gpu_device host_path={} clique={}",
                dev.host_path,
                dev.gpu_direct_clique
            );
            vfio_devices.push(VFIODeviceConfig {
                sysfs_path: dev.host_path.clone(),
                gpu_direct_clique: dev.gpu_direct_clique,
            });
        }
    }

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

    // Build NIC config from intent (SR-IOV NICs produce VFIO devices).
    let (tap_name, mac, qemu_nics, mut vfio_devices) = build_qemu_nics(intent);

    // Add GPU VFIO devices from the GPU intent.
    if let Some(ref gpu) = intent.gpu {
        for dev in &gpu.devices {
            log::info!(
                "gpu_device host={} root_port={}",
                dev.pci_address,
                dev.pcie_root_port
            );
            vfio_devices.push(QemuVFIODevice {
                host_address: dev.pci_address.clone(),
            });
        }
    }

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
                            gpu_direct_clique: -1, // Not applicable for SR-IOV NICs
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

//! Hypervisor launch dispatch — Cloud Hypervisor or QEMU based on RuntimeIntent.hypervisor.

use std::path::{Path, PathBuf};
use std::time::Duration;

use swift_ch_client::{
    spawn_ch, spawn_ch_receive, spawn_ch_restore, wait_for_socket, FsMount, GenericVhostUser,
    NICConfig, VFIODeviceConfig, VmConfig, VsockConfig,
};
use swift_qemu_client::{QemuConfig, QemuNICConfig, QemuProcess, QemuVFIODevice};
use swift_runtime::RuntimeDir;

use crate::intent::{FilesystemIntent, RuntimeIntent};

/// Default virtiofsd binary path (Debian bookworm `virtiofsd` package).
/// Override with KUBESWIFT_VIRTIOFSD_BINARY.
const DEFAULT_VIRTIOFSD_BINARY: &str = "/usr/libexec/virtiofsd";

/// Spawn a virtiofsd backend for one virtiofs share. Uses `--sandbox none`:
/// the launcher pod IS the security boundary (the default `namespace` sandbox
/// needs CAP_SYS_ADMIN, which KubeSwift containers do not have), and the
/// `--shared-dir` mount bounds what the guest can reach. Read-only enforcement
/// is at the source volumeMount (the pod builder sets it readOnly), so no
/// virtiofsd readonly flag is needed.
fn spawn_virtiofsd(
    fs: &FilesystemIntent,
    socket_path: &str,
) -> Result<std::process::Child, String> {
    let binary = std::env::var("KUBESWIFT_VIRTIOFSD_BINARY")
        .unwrap_or_else(|_| DEFAULT_VIRTIOFSD_BINARY.to_string());
    // virtiofsd refuses to bind an existing socket file; remove any stale one
    // (a prior SIGKILL leaves it behind — same hazard as the CH api-socket).
    let _ = std::fs::remove_file(socket_path);
    std::process::Command::new(&binary)
        .arg(format!("--socket-path={}", socket_path))
        .arg(format!("--shared-dir={}", fs.source_path))
        .arg("--sandbox")
        .arg("none")
        .spawn()
        .map_err(|e| format!("spawn virtiofsd ({}) for {}: {}", binary, fs.name, e))
}

/// Kills the virtiofsd backends on drop so a backend never outlives its VM.
struct VirtiofsdGuard(Vec<std::process::Child>);

impl Drop for VirtiofsdGuard {
    fn drop(&mut self) {
        for c in &mut self.0 {
            let _ = c.kill();
            let _ = c.wait();
        }
    }
}

/// Remove stale socket files before hypervisor bind.
/// Neither CH nor QEMU clean up on crash; restart fails with "Address in use".
fn remove_stale_sockets(runtime_dir: &RuntimeDir) {
    let _ = std::fs::remove_file(runtime_dir.api_socket()); // ch.sock
    let _ = std::fs::remove_file(runtime_dir.root().join("serial.sock"));
    let _ = std::fs::remove_file(runtime_dir.root().join("qmp.sock"));
    let _ = std::fs::remove_file(runtime_dir.root().join("vsock.sock"));
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
///     CH.
///   - The migrated VM's disk paths must exist on this pod's
///     filesystem at the SAME paths the source used. Phase 2 manual demo
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

    // Phase 3a D1: persist the receiver CH PID alongside the api socket so
    // the action loop's cancel handler (`dispatch_migration_cancel`) can
    // SIGKILL this process when the controller writes
    // `migration-action: cancel`. CH v51.1 has no `vm.cancel-migration`
    // API (Phase 2 spike F4); SIGKILL on the dst CH is the cancel
    // primitive.
    //
    // Write best-effort: a missing PID file just means the cancel
    // handler returns "cancel kill failed: pid file not found" and the
    // operator falls back to `kubectl delete pod` (the pre-D1
    // behavior). The file is colocated with `ch.sock` so cleanup
    // happens via the same lifecycle (runtime dir removal).
    let pid_path = runtime_dir.root().join("ch.pid");
    if let Err(e) = std::fs::write(&pid_path, pid.to_string()) {
        log::warn!(
            "ch_pid_write_failed path={} err={} (cancel handler will not be able to SIGKILL CH)",
            pid_path.display(),
            e
        );
    }

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
    let mut child = spawn_ch_restore(
        &api_socket,
        &source_url,
        intent.restore_auto_resume(),
        intent.restore_memory_mode(),
    )
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

    let data_disk_paths = intent.data_disk_paths();

    // Build NIC config from intent (SR-IOV NICs produce VFIO devices).
    let (tap_name, ch_nics, mut vfio_devices) = build_ch_nics(intent);

    // Add GPU VFIO devices from the GPU intent. resolved_devices() returns the
    // intent's device list (native backend) or synthesizes it from the
    // CDI-injected GPU_PCI_ADDRESSES env (DRA backend, deviceSource=env).
    if let Some(ref gpu) = intent.gpu {
        for dev in &gpu.resolved_devices()? {
            log::info!(
                "gpu_device host_path={} clique={} source={}",
                dev.host_path,
                dev.gpu_direct_clique,
                if gpu.device_source.is_empty() {
                    "intent"
                } else {
                    &gpu.device_source
                }
            );
            vfio_devices.push(VFIODeviceConfig {
                sysfs_path: dev.host_path.clone(),
                gpu_direct_clique: dev.gpu_direct_clique,
            });
        }
    }

    // virtiofs: spawn a virtiofsd backend per share BEFORE Cloud Hypervisor
    // (CH connects to the sockets at boot). The guard kills the virtiofsd
    // children when this function returns — i.e. when CH exits at child.wait()
    // below — so a backend never outlives its VM.
    let mut fs_mounts: Vec<FsMount> = Vec::new();
    let _virtiofsd = if let Some(ref filesystems) = intent.filesystems {
        let mut children = Vec::new();
        for fs in filesystems {
            // Derive the socket in the runtime dir (shared with CH), like the
            // serial/api sockets — the controller doesn't carry it.
            let socket_path = runtime_dir
                .root()
                .join(format!("{}.fs.sock", fs.name))
                .to_string_lossy()
                .to_string();
            log::info!(
                "spawning virtiofsd name={} tag={} source={} socket={} read_only={}",
                fs.name,
                fs.tag,
                fs.source_path,
                socket_path,
                fs.read_only
            );
            children.push(spawn_virtiofsd(fs, &socket_path)?);
            wait_for_socket(Path::new(&socket_path), Duration::from_secs(15))
                .map_err(|e| format!("virtiofsd socket for {} not ready: {}", fs.name, e))?;
            fs_mounts.push(FsMount {
                tag: fs.tag.clone(),
                socket: socket_path,
            });
        }
        Some(VirtiofsdGuard(children))
    } else {
        None
    };

    // Operator-backed vhost-user devices (blk + generic). The sockets are
    // operator backends mounted into the launcher by the pod builder; CH only
    // connects. No swiftletd-spawned backend (unlike virtiofs).
    let mut vhost_user_blk_sockets: Vec<String> = Vec::new();
    let mut generic_vhost_user: Vec<GenericVhostUser> = Vec::new();
    if let Some(ref devices) = intent.vhost_user_devices {
        for d in devices {
            if d.is_blk() {
                log::info!("vhost-user-blk name={} socket={}", d.name, d.socket);
                vhost_user_blk_sockets.push(d.socket.clone());
            } else {
                log::info!(
                    "generic-vhost-user name={} virtio_id={:?} socket={}",
                    d.name,
                    d.virtio_id,
                    d.socket
                );
                generic_vhost_user.push(GenericVhostUser {
                    virtio_id: d.virtio_id.clone().unwrap_or_default(),
                    socket: d.socket.clone(),
                    queue_sizes: d.queue_sizes.clone().unwrap_or_default(),
                });
            }
        }
    }

    // vsock device for the in-guest identity agent. The CID comes from the
    // intent (deterministic per guest, set controller-side); swiftletd owns the
    // socket path under the runtime dir (mirrors serial.sock), so the configjson
    // patcher's runtime-dir-prefix rewrite relocates it on a clone restore.
    let vsock = intent.vsock.as_ref().map(|v| VsockConfig {
        cid: v.cid,
        socket: runtime_dir
            .root()
            .join("vsock.sock")
            .to_string_lossy()
            .to_string(),
    });

    let config = if intent.has_kernel() {
        let kb = intent.kernel_boot.as_ref().unwrap();
        VmConfig {
            disk_path: String::new(),
            memory_mib: intent.memory.max(128),
            cpus: intent.cpu.max(1),
            kvm_hyperv: intent.is_windows(),
            core_scheduling: intent
                .core_scheduling
                .clone()
                .filter(|s| !s.is_empty() && s != "off"),
            api_socket: runtime_dir.api_socket().to_string_lossy().to_string(),
            seed_path: String::new(),
            serial_socket_path: Some(serial_socket_path.clone()),
            firmware_path: None,
            tap_name,
            nics: ch_nics,
            kernel_path: Some(kb.kernel_path.clone()),
            initramfs_path: Some(kb.initramfs_path.clone()),
            kernel_cmdline: Some(kb.cmdline.clone()),
            sandbox_rootfs: intent.sandbox_rootfs_path().map(|s| s.to_string()),
            data_disk_paths: data_disk_paths.clone(),
            vfio_devices,
            fs_mounts,
            vhost_user_blk_sockets,
            generic_vhost_user,
            vsock: vsock.clone(),
        }
    } else {
        VmConfig {
            disk_path: intent.disk_path().to_string(),
            memory_mib: intent.memory.max(128),
            cpus: intent.cpu.max(1),
            kvm_hyperv: intent.is_windows(),
            core_scheduling: intent
                .core_scheduling
                .clone()
                .filter(|s| !s.is_empty() && s != "off"),
            api_socket: runtime_dir.api_socket().to_string_lossy().to_string(),
            seed_path,
            serial_socket_path: Some(serial_socket_path.clone()),
            firmware_path: Some("/usr/share/kubeswift-firmware/CLOUDHV.fd".to_string()),
            tap_name,
            nics: ch_nics,
            kernel_path: None,
            initramfs_path: None,
            kernel_cmdline: None,
            sandbox_rootfs: None,
            data_disk_paths: data_disk_paths.clone(),
            vfio_devices,
            fs_mounts,
            vhost_user_blk_sockets,
            generic_vhost_user,
            vsock: vsock.clone(),
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

    // Add GPU VFIO devices from the GPU intent (resolved_devices: intent list
    // for native, CDI-injected GPU_PCI_ADDRESSES env for DRA).
    if let Some(ref gpu) = intent.gpu {
        for dev in &gpu.resolved_devices()? {
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
        data_disk_paths: intent.data_disk_paths(),
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
    // Model A: the guest rides its namespace primary OVN-K UDN (ovn-udn1). network-init
    // captured OVN's IP-derived MAC (the LSP port_security pins MAC+IP) and the launcher
    // entrypoint exported it here. The PRIMARY NIC must source frames with this MAC or
    // OVN drops them. Gated on the intent's primary_udn_interface signal so a stray env
    // var never rewrites a non-Model-A guest's MAC.
    let primary_udn_mac = if intent.primary_udn_interface.is_some() {
        std::env::var("KUBESWIFT_PRIMARY_UDN_MAC")
            .ok()
            .filter(|m| !m.is_empty())
    } else {
        None
    };

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
            } else if n.is_vhost_user() {
                // vhost-user-net: no tap; CH connects to the operator backend
                // socket. Skip (with a loud log) if the socket is missing — the
                // controller/webhook should always populate it.
                match &n.vhost_user_socket {
                    Some(sock) => ch_nics.push(NICConfig {
                        tap_name: String::new(),
                        mac: n.mac.clone(),
                        vhost_user_socket: Some(sock.clone()),
                    }),
                    None => {
                        log::error!("vhost-user NIC {} has no backend socket; skipping", n.name)
                    }
                }
            } else {
                // Model A: the primary NIC adopts OVN's captured MAC; others keep
                // their intent MAC.
                let mac = if n.primary {
                    primary_udn_mac.clone().unwrap_or_else(|| n.mac.clone())
                } else {
                    n.mac.clone()
                };
                ch_nics.push(NICConfig {
                    tap_name: n.tap_device.clone(),
                    mac,
                    vhost_user_socket: None,
                });
            }
        }
        (None, ch_nics, vfio_devs)
    } else if intent.has_network() {
        // Legacy single default NIC. For a Model A default guest (no explicit
        // interfaces), emit an explicit NICConfig carrying OVN's captured MAC instead
        // of the auto-MAC tap0 path, so the guest passes the UDN's port_security.
        if let Some(mac) = primary_udn_mac {
            (
                None,
                vec![NICConfig {
                    tap_name: "tap0".to_string(),
                    mac,
                    vhost_user_socket: None,
                }],
                vec![],
            )
        } else {
            (Some("tap0".to_string()), vec![], vec![])
        }
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

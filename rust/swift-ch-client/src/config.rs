//! VM configuration for Cloud Hypervisor.

/// Cloud Hypervisor binary name. Override with KUBESWIFT_CH_BINARY env.
pub const DEFAULT_CH_BINARY: &str = "cloud-hypervisor";

/// Network interface configuration for multi-NIC mode.
#[derive(Debug, Clone)]
pub struct NICConfig {
    /// Tap device name (tap0, tap1, etc.). Empty for a vhost-user NIC.
    pub tap_name: String,
    /// MAC address for the interface.
    pub mac: String,
    /// vhost-user-net backend socket path. When Some, this NIC is emitted as
    /// `--net vhost_user=on,socket=<path>,mac=<mac>` instead of a tap NIC.
    pub vhost_user_socket: Option<String>,
}

/// VFIO device for passthrough (GPU or SR-IOV NIC).
#[derive(Debug, Clone)]
pub struct VFIODeviceConfig {
    /// Sysfs path (e.g., "/sys/bus/pci/devices/0000:3b:0a.0/")
    pub sysfs_path: String,
    /// x_nv_gpudirect_clique value for NVIDIA GPUs. -1 = omit (non-NVIDIA or SR-IOV).
    pub gpu_direct_clique: i32,
}

/// VM configuration derived from runtime intent.
#[derive(Debug, Clone)]
pub struct VmConfig {
    /// Path to root disk. Opaque string — Cloud Hypervisor's
    /// `--disk path=<value>` opens both filesystem files and raw block
    /// devices identically, so this can be either a regular file path
    /// (e.g. `/var/lib/kubeswift/disks/root/image.raw` for Filesystem-
    /// mode PVCs) or a device path (e.g. `/dev/kubeswift-root` for
    /// Block-mode PVCs surfaced via Kubernetes `volumeDevices`).
    ///
    /// W9 — the controller resolves which form to use based on the
    /// SwiftGuest's resolved `spec.storage.volumeMode`; swiftletd
    /// hands whichever string it received through to CH unchanged.
    /// No suffix detection or path-shape branching exists in this
    /// crate (verified by grep audit at W9 Component 3 start —
    /// see PR description).
    pub disk_path: String,
    /// Memory size in MiB.
    pub memory_mib: u32,
    /// Number of CPUs.
    pub cpus: u32,
    /// When true, append `,kvm_hyperv=on` to `--cpus`. Set for Windows guests:
    /// the spike proved Windows hangs in early MP/HAL init on CH without the
    /// KVM Hyper-V enlightenments (docs/design/windows-guest-support-spike.md).
    /// Harmless/unused for Linux guests (default false).
    pub kvm_hyperv: bool,
    /// vCPU core-scheduling policy ("vm"/"vcpu"), appended to --cpus as
    /// core_scheduling=<v>. None = off (param omitted). SMT side-channel mitigation.
    pub core_scheduling: Option<String>,
    /// Path for Cloud Hypervisor API socket.
    pub api_socket: String,
    /// Optional path to seed media (NoCloud dir or ISO). Empty = no seed.
    pub seed_path: String,
    /// Optional path for serial socket. When set, CH creates a Unix socket for interactive serial console.
    pub serial_socket_path: Option<String>,
    /// Optional path to UEFI firmware (CLOUDHV.fd). Required for disk boot; passed via --kernel flag.
    pub firmware_path: Option<String>,
    /// Optional TAP device name for VM networking (legacy single-NIC mode).
    /// When set, CH gets --net tap=<name>.
    pub tap_name: Option<String>,
    /// Network interfaces for multi-NIC mode. When non-empty, overrides tap_name.
    /// Each entry produces a --net tap=<tap>,mac=<mac> argument.
    pub nics: Vec<NICConfig>,
    /// When set, boot via --kernel + --initramfs instead of --disk for root.
    pub kernel_path: Option<String>,
    /// Path to initramfs image. Required when kernel_path is set.
    pub initramfs_path: Option<String>,
    /// Kernel command line. Only used when kernel_path is set.
    pub kernel_cmdline: Option<String>,
    /// Secondary data disk paths, in order. Empty = no data disks. Each
    /// produces a `--disk path=<p>,image_type=raw,direct=on` argument
    /// after the root (and seed) disk, appearing as the next virtio-blk
    /// device in the guest (/dev/vdb, /dev/vdc, ...). Paths are opaque
    /// (raw file or raw block device — decided controller-side).
    pub data_disk_paths: Vec<String>,
    /// VFIO devices to pass through (SR-IOV VFs, GPUs).
    /// Each produces a --device path=<sysfs_path> argument.
    pub vfio_devices: Vec<VFIODeviceConfig>,
    /// virtiofs shares. Each produces a
    /// `--fs tag=<tag>,socket=<sock>,num_queues=1,queue_size=1024` argument.
    /// The backend (virtiofsd) is spawned by swiftletd before CH; CH only
    /// connects to the socket.
    pub fs_mounts: Vec<FsMount>,
    /// vhost-user-blk disks (operator backend, e.g. SPDK). Each socket
    /// produces a `--disk vhost_user=on,socket=<sock>` argument. The disk
    /// appears as a virtio-blk device after the root/data disks.
    pub vhost_user_blk_sockets: Vec<String>,
    /// Generic vhost-user devices. Each produces a
    /// `--generic-vhost-user virtio_id=<id>,socket=<sock>[,queue_sizes=[...]]`.
    pub generic_vhost_user: Vec<GenericVhostUser>,
    /// Optional vsock device. When set, produces `--vsock cid=<N>,socket=<path>`
    /// — a host<->guest channel for the in-guest identity agent. Set ONLY on a
    /// SOURCE guest (a snapshot source that opted into the agent): CH captures
    /// the device state into the snapshot, so the restored clone gets a working
    /// vsock device WITHOUT --vsock on the restore command (the clone path uses
    /// `spawn_ch_restore`, which reopens the device from config.json; the Go
    /// controller's configjson patcher rewrites only the socket PATH to the
    /// clone runtime dir). See docs/design/clone-identity-vsock-agent.md.
    pub vsock: Option<VsockConfig>,
}

/// A vsock device for `--vsock cid=<N>,socket=<path>`.
#[derive(Debug, Clone)]
pub struct VsockConfig {
    /// Guest context id (>= 3; 0-2 are reserved). Deterministic per guest,
    /// derived controller-side from (namespace, name). Rides the snapshot on
    /// restore — never re-assigned on a clone.
    pub cid: u32,
    /// Host-side Unix socket CH bridges the guest's AF_VSOCK to (the launcher
    /// connects here). Mirrors the serial/api runtime-dir socket convention:
    /// `<runtime-dir>/vsock.sock`.
    pub socket: String,
}

/// A generic vhost-user device for `--generic-vhost-user`.
#[derive(Debug, Clone)]
pub struct GenericVhostUser {
    /// virtio device-type id: a number or symbolic name ("block", "fs", ...).
    pub virtio_id: String,
    /// Operator backend socket path (mounted into the launcher).
    pub socket: String,
    /// Optional per-queue sizes; empty omits the param (CH defaults apply).
    pub queue_sizes: Vec<u32>,
}

/// A virtiofs (vhost-user-fs) share for `--fs`.
#[derive(Debug, Clone)]
pub struct FsMount {
    /// Mount tag the guest uses (`mount -t virtiofs <tag> ...`).
    pub tag: String,
    /// Unix socket virtiofsd binds and CH connects to.
    pub socket: String,
}

impl VmConfig {
    /// Returns the API socket path. Used by spawn paths to clean up
    /// stale sockets before invoking CH (W2 walkthrough finding —
    /// `docs/design/live-migration-phase-2.md` §4.3.3).
    pub fn api_socket(&self) -> &str {
        &self.api_socket
    }

    /// Build CH process arguments. Unix socket only; no TCP.
    /// Each option and its value must be separate argv elements (e.g. ["--api-socket", "path=/foo"]).
    /// For --disk, multiple disks are passed as multiple values to a single --disk (not repeated --disk).
    pub fn to_args(&self) -> Vec<String> {
        let mut args = vec![
            "--api-socket".to_string(),
            format!("path={}", self.api_socket),
            "--memory".to_string(),
            // shared=on maps the guest-RAM memfd ("ch_ram") MAP_SHARED instead of
            // CH's default MAP_PRIVATE. Under the default (shared=off), CH still
            // backs guest RAM with a memfd but maps it copy-on-write, so the
            // cgroup holds the guest pages TWICE -- once in the memfd (shmem) and
            // once in CH's CoW-private pages (anon) -- ~2x the touched guest RAM,
            // all unreclaimable. That left no room for a memory-snapshot capture's
            // buffered write and OOMKilled the launcher (cluster-diagnosed
            // 2026-06-08 via /proc/<ch>/smaps: the ch_ram mapping was rw-p with
            // Private_Dirty == guest RAM). shared=on collapses the double to ~1x
            // (writes land in the memfd; no CoW copy), which is also the standard
            // backing for snapshot/live-migration-capable guests (mirrors QEMU's
            // memory-backend-memfd,share=on) and what the sparse-snapshot /
            // userfaultfd path (#163) wants.
            format!("size={}M,shared=on", self.memory_mib),
            "--cpus".to_string(),
            {
                let mut cpus = format!("boot={}", self.cpus.max(1));
                if self.kvm_hyperv {
                    // Windows: KVM Hyper-V enlightenments (required on CH).
                    cpus.push_str(",kvm_hyperv=on");
                }
                if let Some(ref cs) = self.core_scheduling {
                    // vCPU core-scheduling (SMT side-channel mitigation): "vm"
                    // or "vcpu". Omitted when off.
                    cpus.push_str(&format!(",core_scheduling={}", cs));
                }
                cpus
            },
        ];

        if let Some(ref kp) = self.kernel_path {
            args.push("--kernel".to_string());
            args.push(kp.clone());

            if let Some(ref ip) = self.initramfs_path {
                args.push("--initramfs".to_string());
                args.push(ip.clone());
            }

            if let Some(ref cl) = self.kernel_cmdline {
                if !cl.is_empty() {
                    args.push("--cmdline".to_string());
                    args.push(cl.clone());
                }
            }

            if !self.seed_path.is_empty() {
                // No direct=on: emptyDir/tmpfs-backed; tmpfs rejects O_DIRECT.
                args.push("--disk".to_string());
                args.push(format!("path={},image_type=raw", self.seed_path));
            }
            // direct=on: PVC-backed data disks, O_DIRECT-capable (see the
            // disk-boot branch comment for why root/data bypass the cache).
            // One --disk per data disk, in order.
            for p in &self.data_disk_paths {
                if p.is_empty() {
                    continue;
                }
                args.push("--disk".to_string());
                args.push(format!("path={},image_type=raw,direct=on", p));
            }
        } else {
            // --kernel (CLOUDHV.fd UEFI firmware) required for disk boot.
            if let Some(ref path) = self.firmware_path {
                args.push("--kernel".to_string());
                args.push(path.clone());
            }

            // --disk accepts multiple values: --disk path=/foo path=/bar.
            // image_type=raw is explicit: every KubeSwift runtime disk is raw
            // (Design Principle #3 — root/seed.iso/data are all raw), and CH v52
            // deprecates disk image-type autodetection (removed in a future
            // release). Being explicit avoids the deprecation warning and the
            // autodetection sector-0 probe (W10).
            //
            // direct=on (O_DIRECT, KubeVirt cache=none parity) on the ROOT and
            // DATA disks bypasses the host page cache for guest disk I/O. The
            // guest already caches its own blocks in guest RAM, so the host copy
            // is a wasteful double-cache; bypassing it makes the launcher's memory
            // footprint honest and predictable (no ~disk-working-set of reclaimable
            // page cache silently consuming the overhead). NOTE: this is hygiene,
            // NOT the memory-snapshot OOM fix -- that root cause was CH backing
            // guest RAM with a MAP_PRIVATE memfd (~2x footprint), fixed by
            // `--memory ...,shared=on` above. Root and data disks are always
            // PVC-backed (ext4 raw file on Filesystem, or a raw block device on
            // Block); both support O_DIRECT. The SEED disk is deliberately left
            // buffered: it
            // is emptyDir-backed (often tmpfs), and tmpfs rejects O_DIRECT with
            // EINVAL -- direct=on there would fail boot (it is a few hundred KB,
            // not a cache concern). This is per-disk-ROLE, not path-shape
            // inference -- the opacity contract holds (config.rs never parses the
            // path string to decide behavior).
            args.push("--disk".to_string());
            args.push(format!("path={},image_type=raw,direct=on", self.disk_path));
            if !self.seed_path.is_empty() {
                // Cloud Hypervisor: second disk for cloud-init NoCloud.
                // CH expects ISO or vfat; we pass directory path (see swift-ch-client README).
                // No direct=on: emptyDir/tmpfs-backed; tmpfs rejects O_DIRECT.
                args.push(format!("path={},image_type=raw", self.seed_path));
            }
            // Data disks continue the same multi-value --disk argument
            // (--disk path=root path=seed path=data0 path=data1 ...).
            for p in &self.data_disk_paths {
                if p.is_empty() {
                    continue;
                }
                args.push(format!("path={},image_type=raw,direct=on", p));
            }
        }

        if let Some(ref path) = self.serial_socket_path {
            // Serial socket: bidirectional; connect with socat for interactive console.
            // Kernel cmdline comes from disk GRUB (patched during SwiftImage import for console=ttyS0).
            args.push("--serial".to_string());
            args.push(format!("socket={}", path));
            // Disable virtio-console; serial is the interactive console.
            args.push("--console".to_string());
            args.push("off".to_string());
        }

        if !self.nics.is_empty() {
            // Multi-NIC mode: one --net per NIC with tap and mac, or a
            // vhost-user backend socket when set.
            for nic in &self.nics {
                args.push("--net".to_string());
                if let Some(ref socket) = nic.vhost_user_socket {
                    args.push(format!("vhost_user=on,socket={},mac={}", socket, nic.mac));
                } else {
                    args.push(format!("tap={},mac={}", nic.tap_name, nic.mac));
                }
            }
        } else if let Some(ref tap) = self.tap_name {
            // Legacy single-NIC mode.
            args.push("--net".to_string());
            args.push(format!("tap={}", tap));
        }

        // VFIO passthrough devices (SR-IOV VFs, GPUs).
        for dev in &self.vfio_devices {
            args.push("--device".to_string());
            if dev.gpu_direct_clique >= 0 {
                // NVIDIA-specific: x_nv_gpudirect_clique enables PCIe P2P DMA between GPUs.
                args.push(format!(
                    "path={},x_nv_gpudirect_clique={}",
                    dev.sysfs_path, dev.gpu_direct_clique
                ));
            } else {
                args.push(format!("path={}", dev.sysfs_path));
            }
        }

        // virtiofs shares (vhost-user-fs). The virtiofsd backend is already
        // running on the socket (swiftletd spawned it before CH); CH attaches.
        // num_queues=1,queue_size=1024 are sane defaults (tuned later if needed).
        for fs in &self.fs_mounts {
            args.push("--fs".to_string());
            args.push(format!(
                "tag={},socket={},num_queues=1,queue_size=1024",
                fs.tag, fs.socket
            ));
        }

        // vhost-user-blk disks: operator backend, no path — just the socket.
        for sock in &self.vhost_user_blk_sockets {
            args.push("--disk".to_string());
            args.push(format!("vhost_user=on,socket={}", sock));
        }

        // Generic vhost-user devices.
        for dev in &self.generic_vhost_user {
            args.push("--generic-vhost-user".to_string());
            let mut arg = format!("virtio_id={},socket={}", dev.virtio_id, dev.socket);
            if !dev.queue_sizes.is_empty() {
                let list = dev
                    .queue_sizes
                    .iter()
                    .map(|q| q.to_string())
                    .collect::<Vec<_>>()
                    .join(",");
                arg.push_str(&format!(",queue_sizes=[{}]", list));
            }
            args.push(arg);
        }

        // vsock device for the in-guest identity agent (source guests only;
        // the clone reopens it from config.json on restore).
        if let Some(ref v) = self.vsock {
            args.push("--vsock".to_string());
            args.push(format!("cid={},socket={}", v.cid, v.socket));
        }

        args
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_disk_boot_config() -> VmConfig {
        VmConfig {
            disk_path: "/data/image.raw".to_string(),
            memory_mib: 2048,
            cpus: 2,
            kvm_hyperv: false,
            core_scheduling: None,
            api_socket: "/tmp/ch.sock".to_string(),
            seed_path: "/data/seed".to_string(),
            serial_socket_path: Some("/tmp/serial.sock".to_string()),
            firmware_path: Some("/usr/share/kubeswift-firmware/CLOUDHV.fd".to_string()),
            tap_name: Some("tap0".to_string()),
            nics: vec![],
            kernel_path: None,
            initramfs_path: None,
            kernel_cmdline: None,
            data_disk_paths: vec![],
            vfio_devices: vec![],
            fs_mounts: vec![],
            vhost_user_blk_sockets: vec![],
            generic_vhost_user: vec![],
            vsock: None,
        }
    }

    #[test]
    fn test_vsock_arg_emitted_when_set() {
        let mut cfg = make_disk_boot_config();
        cfg.vsock = Some(VsockConfig {
            cid: 7,
            socket: "/run/kubeswift/run/g/vsock.sock".to_string(),
        });
        let args = cfg.to_args();
        let i = args
            .iter()
            .position(|a| a == "--vsock")
            .expect("--vsock missing");
        assert_eq!(args[i + 1], "cid=7,socket=/run/kubeswift/run/g/vsock.sock");
        // default (no vsock) emits nothing
        let none = make_disk_boot_config().to_args();
        assert!(
            !none.iter().any(|a| a == "--vsock"),
            "vsock leaked when None"
        );
    }

    #[test]
    fn test_cpus_kvm_hyperv_for_windows() {
        // Default (Linux): plain boot=N, no enlightenments.
        let cfg = make_disk_boot_config();
        let joined = cfg.to_args().join(" ");
        assert!(
            joined.contains("--cpus boot=2") && !joined.contains("kvm_hyperv"),
            "linux --cpus should be plain boot=N: {}",
            joined
        );
        // Windows: boot=N,kvm_hyperv=on.
        let mut win = make_disk_boot_config();
        win.kvm_hyperv = true;
        let joined = win.to_args().join(" ");
        assert!(
            joined.contains("--cpus boot=2,kvm_hyperv=on"),
            "windows --cpus should add kvm_hyperv=on: {}",
            joined
        );
    }

    #[test]
    fn test_fs_mounts_emit_fs_args() {
        let mut cfg = make_disk_boot_config();
        cfg.fs_mounts = vec![
            FsMount {
                tag: "share".to_string(),
                socket: "/run/x/share.fs.sock".to_string(),
            },
            FsMount {
                tag: "data".to_string(),
                socket: "/run/x/data.fs.sock".to_string(),
            },
        ];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains(
                "--fs tag=share,socket=/run/x/share.fs.sock,num_queues=1,queue_size=1024"
            ),
            "missing first --fs: {}",
            joined
        );
        assert!(
            joined
                .contains("--fs tag=data,socket=/run/x/data.fs.sock,num_queues=1,queue_size=1024"),
            "missing second --fs: {}",
            joined
        );
        // No virtiofs shares -> no --fs at all.
        let none = make_disk_boot_config().to_args().join(" ");
        assert!(
            !none.contains("--fs"),
            "unexpected --fs without mounts: {}",
            none
        );
    }

    #[test]
    fn test_vhost_user_blk_and_generic_emit_args() {
        let mut cfg = make_disk_boot_config();
        cfg.vhost_user_blk_sockets = vec!["/run/spdk/vhost.0".to_string()];
        cfg.generic_vhost_user = vec![
            GenericVhostUser {
                virtio_id: "block".to_string(),
                socket: "/run/x/gen.sock".to_string(),
                queue_sizes: vec![1024, 1024],
            },
            GenericVhostUser {
                virtio_id: "fs".to_string(),
                socket: "/run/x/gen2.sock".to_string(),
                queue_sizes: vec![],
            },
        ];
        let joined = cfg.to_args().join(" ");
        // vhost-user-blk: a --disk with vhost_user=on,socket= and NO path=.
        assert!(
            joined.contains("--disk vhost_user=on,socket=/run/spdk/vhost.0"),
            "missing vhost-user-blk --disk: {}",
            joined
        );
        // generic with queue_sizes -> bracketed list.
        assert!(
            joined.contains(
                "--generic-vhost-user virtio_id=block,socket=/run/x/gen.sock,queue_sizes=[1024,1024]"
            ),
            "missing generic w/ queue_sizes: {}",
            joined
        );
        // generic without queue_sizes -> no queue_sizes param.
        assert!(
            joined.contains("--generic-vhost-user virtio_id=fs,socket=/run/x/gen2.sock")
                && !joined.contains("virtio_id=fs,socket=/run/x/gen2.sock,queue_sizes"),
            "generic w/o queue_sizes wrong: {}",
            joined
        );
        // None set -> no vhost-user-blk / generic args.
        let none = make_disk_boot_config().to_args().join(" ");
        assert!(!none.contains("vhost_user=on"), "unexpected blk: {}", none);
        assert!(
            !none.contains("--generic-vhost-user"),
            "unexpected generic: {}",
            none
        );
    }

    #[test]
    fn test_core_scheduling_emits_on_cpus() {
        let mut cfg = make_disk_boot_config();
        cfg.core_scheduling = Some("vm".to_string());
        let joined = cfg.to_args().join(" ");
        assert!(
            joined.contains("--cpus boot=") && joined.contains(",core_scheduling=vm"),
            "missing core_scheduling on --cpus: {}",
            joined
        );
        // None -> no core_scheduling param.
        let none = make_disk_boot_config().to_args().join(" ");
        assert!(
            !none.contains("core_scheduling"),
            "unexpected core_scheduling: {}",
            none
        );
    }

    #[test]
    fn test_memory_carries_shared_on() {
        // Guest RAM must be memfd-MAP_SHARED (shared=on), not the default
        // MAP_PRIVATE CoW which doubles the launcher's guest-memory footprint
        // (~2x touched RAM) and OOMs memory-snapshot captures. Both boot paths.
        let disk = make_disk_boot_config();
        assert!(
            disk.to_args()
                .join(" ")
                .contains("--memory size=2048M,shared=on"),
            "disk-boot --memory must carry shared=on: {}",
            disk.to_args().join(" ")
        );
        let mut kern = make_disk_boot_config();
        kern.kernel_path = Some("/k/bzImage".to_string());
        kern.firmware_path = None;
        assert!(
            kern.to_args()
                .join(" ")
                .contains("--memory size=2048M,shared=on"),
            "kernel-boot --memory must carry shared=on: {}",
            kern.to_args().join(" ")
        );
    }

    #[test]
    fn test_disks_carry_image_type_raw() {
        let mut cfg = make_disk_boot_config();
        cfg.data_disk_paths = vec!["/data/extra.raw".to_string()];
        let args = cfg.to_args();
        // Every --disk value carries image_type=raw (CH v52 deprecates disk
        // image-type autodetection; raw is the runtime invariant).
        let disk_idx = args
            .iter()
            .position(|a| a == "--disk")
            .expect("--disk missing");
        for v in &args[disk_idx + 1..] {
            if v.starts_with("--") {
                break;
            }
            assert!(
                v.contains(",image_type=raw"),
                "disk value missing image_type=raw: {}",
                v
            );
        }
        let joined = args.join(" ");
        assert!(joined.contains("path=/data/image.raw,image_type=raw"));
        assert!(joined.contains("path=/data/seed,image_type=raw"));
        assert!(joined.contains("path=/data/extra.raw,image_type=raw"));
        // The VFIO --device path= (none here) must never get image_type.
    }

    /// O_DIRECT (cache=none) is applied per-disk-ROLE: the ROOT and DATA
    /// disks (always PVC-backed, O_DIRECT-capable) carry `,direct=on` to
    /// bypass the host page cache; the SEED disk (emptyDir/tmpfs-backed,
    /// which rejects O_DIRECT with EINVAL) must NEVER carry it. A future
    /// refactor that blanket-appends direct=on to every --disk would break
    /// the seed mount on tmpfs and fail boot — this test pins that invariant.
    #[test]
    fn test_disks_direct_io_per_role() {
        // Disk-boot: root + seed + data.
        let mut cfg = make_disk_boot_config();
        cfg.data_disk_paths = vec!["/data/extra.raw".to_string()];
        let joined = cfg.to_args().join(" ");
        // Root (PVC) and data (PVC) bypass the cache.
        assert!(
            joined.contains("path=/data/image.raw,image_type=raw,direct=on"),
            "root disk must carry direct=on: {}",
            joined
        );
        assert!(
            joined.contains("path=/data/extra.raw,image_type=raw,direct=on"),
            "data disk must carry direct=on: {}",
            joined
        );
        // Seed (emptyDir/tmpfs) stays buffered — direct=on would EINVAL.
        assert!(
            joined.contains("path=/data/seed,image_type=raw")
                && !joined.contains("path=/data/seed,image_type=raw,direct=on"),
            "seed disk must NOT carry direct=on (tmpfs rejects O_DIRECT): {}",
            joined
        );

        // Kernel-boot: seed + data (no root --disk). Same per-role policy.
        let mut kcfg = make_disk_boot_config();
        kcfg.kernel_path = Some("/k/bzImage".to_string());
        kcfg.firmware_path = None;
        kcfg.data_disk_paths = vec!["/data/extra.raw".to_string()];
        let kjoined = kcfg.to_args().join(" ");
        assert!(
            kjoined.contains("path=/data/extra.raw,image_type=raw,direct=on"),
            "kernel-boot data disk must carry direct=on: {}",
            kjoined
        );
        assert!(
            kjoined.contains("path=/data/seed,image_type=raw")
                && !kjoined.contains("path=/data/seed,image_type=raw,direct=on"),
            "kernel-boot seed disk must NOT carry direct=on: {}",
            kjoined
        );
    }

    #[test]
    fn test_disk_boot_data_disk() {
        let mut cfg = make_disk_boot_config();
        cfg.data_disk_paths = vec!["/data/extra.raw".to_string()];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("path=/data/extra.raw"),
            "missing data disk in disk boot args: {}",
            joined
        );
    }

    #[test]
    fn test_disk_boot_no_data_disk() {
        let cfg = make_disk_boot_config();
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            !joined.contains("extra.raw"),
            "unexpected data disk in args: {}",
            joined
        );
    }

    #[test]
    fn test_kernel_boot_data_disk() {
        let mut cfg = make_disk_boot_config();
        cfg.kernel_path = Some("/kernels/bzImage".to_string());
        cfg.initramfs_path = Some("/kernels/rootfs.cpio.gz".to_string());
        cfg.kernel_cmdline = Some("console=ttyS0".to_string());
        cfg.data_disk_paths = vec!["/data/extra.raw".to_string()];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("path=/data/extra.raw"),
            "missing data disk in kernel boot args: {}",
            joined
        );
    }

    #[test]
    fn test_kernel_boot_no_data_disk() {
        let mut cfg = make_disk_boot_config();
        cfg.kernel_path = Some("/kernels/bzImage".to_string());
        cfg.initramfs_path = Some("/kernels/rootfs.cpio.gz".to_string());
        cfg.kernel_cmdline = Some("console=ttyS0".to_string());
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            !joined.contains("extra.raw"),
            "unexpected data disk in kernel boot args: {}",
            joined
        );
    }

    #[test]
    fn test_ch_args_single_nic() {
        let mut cfg = make_disk_boot_config();
        cfg.tap_name = None; // Clear legacy
        cfg.nics = vec![NICConfig {
            tap_name: "tap0".to_string(),
            mac: "52:54:00:aa:bb:cc".to_string(),
            vhost_user_socket: None,
        }];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("tap=tap0,mac=52:54:00:aa:bb:cc"),
            "single NIC: {}",
            joined
        );
    }

    #[test]
    fn test_ch_args_vhost_user_nic() {
        let mut cfg = make_disk_boot_config();
        cfg.tap_name = None;
        cfg.nics = vec![
            NICConfig {
                tap_name: "tap0".to_string(),
                mac: "52:54:00:aa:bb:01".to_string(),
                vhost_user_socket: None,
            },
            NICConfig {
                tap_name: String::new(),
                mac: "52:54:00:aa:bb:fe".to_string(),
                vhost_user_socket: Some("/var/run/vhost/fast0.sock".to_string()),
            },
        ];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert_eq!(args.iter().filter(|a| *a == "--net").count(), 2);
        // Bridge NIC stays tap-based.
        assert!(
            joined.contains("tap=tap0,mac=52:54:00:aa:bb:01"),
            "bridge NIC: {}",
            joined
        );
        // vhost-user NIC emits vhost_user=on,socket=...
        assert!(
            joined.contains("vhost_user=on,socket=/var/run/vhost/fast0.sock,mac=52:54:00:aa:bb:fe"),
            "vhost-user NIC: {}",
            joined
        );
        // No tap= for the vhost-user NIC.
        assert!(
            !joined.contains("tap=,"),
            "vhost-user NIC must not emit an empty tap: {}",
            joined
        );
    }

    #[test]
    fn test_ch_args_multi_nic() {
        let mut cfg = make_disk_boot_config();
        cfg.tap_name = None;
        cfg.nics = vec![
            NICConfig {
                tap_name: "tap0".to_string(),
                mac: "52:54:00:aa:bb:01".to_string(),
                vhost_user_socket: None,
            },
            NICConfig {
                tap_name: "tap1".to_string(),
                mac: "52:54:00:aa:bb:02".to_string(),
                vhost_user_socket: None,
            },
            NICConfig {
                tap_name: "tap2".to_string(),
                mac: "52:54:00:aa:bb:03".to_string(),
                vhost_user_socket: None,
            },
        ];
        let args = cfg.to_args();
        let joined = args.join(" ");
        // Should have 3 --net flags
        let net_count = args.iter().filter(|a| *a == "--net").count();
        assert_eq!(net_count, 3, "expected 3 --net flags, got {}", net_count);
        assert!(
            joined.contains("tap=tap0,mac=52:54:00:aa:bb:01"),
            "missing tap0: {}",
            joined
        );
        assert!(
            joined.contains("tap=tap1,mac=52:54:00:aa:bb:02"),
            "missing tap1: {}",
            joined
        );
        assert!(
            joined.contains("tap=tap2,mac=52:54:00:aa:bb:03"),
            "missing tap2: {}",
            joined
        );
    }

    #[test]
    fn test_ch_args_no_nics_legacy() {
        // Legacy mode: nics empty, tap_name set -> single --net tap=tap0
        let cfg = make_disk_boot_config(); // has tap_name=Some("tap0"), nics=vec![]
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(joined.contains("tap=tap0"), "legacy tap: {}", joined);
        // Should NOT contain mac= (legacy mode doesn't set MAC in CH)
        assert!(
            !joined.contains("mac="),
            "legacy should not have mac: {}",
            joined
        );
    }

    #[test]
    fn test_ch_args_sriov_vfio_device() {
        let mut cfg = make_disk_boot_config();
        cfg.tap_name = None;
        cfg.nics = vec![];
        cfg.vfio_devices = vec![VFIODeviceConfig {
            sysfs_path: "/sys/bus/pci/devices/0000:3b:0a.0/".to_string(),
            gpu_direct_clique: -1,
        }];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("--device"),
            "missing --device for VFIO: {}",
            joined
        );
        assert!(
            joined.contains("path=/sys/bus/pci/devices/0000:3b:0a.0/"),
            "missing sysfs path: {}",
            joined
        );
    }

    #[test]
    fn test_ch_args_mixed_bridge_and_sriov() {
        let mut cfg = make_disk_boot_config();
        cfg.tap_name = None;
        cfg.nics = vec![NICConfig {
            tap_name: "tap0".to_string(),
            mac: "52:54:00:aa:bb:01".to_string(),
            vhost_user_socket: None,
        }];
        cfg.vfio_devices = vec![VFIODeviceConfig {
            sysfs_path: "/sys/bus/pci/devices/0000:3b:0a.0/".to_string(),
            gpu_direct_clique: -1,
        }];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("--net"),
            "missing --net for bridge NIC: {}",
            joined
        );
        assert!(joined.contains("tap=tap0"), "missing tap: {}", joined);
        assert!(
            joined.contains("--device"),
            "missing --device for VFIO: {}",
            joined
        );
        assert!(
            joined.contains("path=/sys/bus/pci/devices/0000:3b:0a.0/"),
            "missing sysfs: {}",
            joined
        );
    }

    #[test]
    fn test_ch_args_gpu_with_clique() {
        let mut cfg = make_disk_boot_config();
        cfg.tap_name = None;
        cfg.nics = vec![];
        cfg.vfio_devices = vec![VFIODeviceConfig {
            sysfs_path: "/sys/bus/pci/devices/0000:41:00.0/".to_string(),
            gpu_direct_clique: 0,
        }];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("x_nv_gpudirect_clique=0"),
            "NVIDIA GPU should have clique: {}",
            joined
        );
    }

    #[test]
    fn test_ch_args_gpu_no_clique() {
        let mut cfg = make_disk_boot_config();
        cfg.tap_name = None;
        cfg.nics = vec![];
        cfg.vfio_devices = vec![VFIODeviceConfig {
            sysfs_path: "/sys/bus/pci/devices/0000:03:00.0/".to_string(),
            gpu_direct_clique: -1, // AMD/Intel -- no clique
        }];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            !joined.contains("x_nv_gpudirect_clique"),
            "non-NVIDIA GPU should NOT have clique: {}",
            joined
        );
        assert!(
            joined.contains("path=/sys/bus/pci/devices/0000:03:00.0/"),
            "missing device path: {}",
            joined
        );
    }

    // W9 Component 3 — Block-mode root disk path passes through the
    // CH args generator unchanged. The architect's Q2 read held: this
    // crate has zero suffix-detection logic; the disk_path field is
    // opaque. These tests lock that opacity in so a future commit
    // that introduces, e.g., `if disk_path.ends_with(".raw") { ... }`
    // produces a visible test failure.

    /// W9 — disk-boot path with a `/dev/...` device path produces
    /// `--disk path=/dev/kubeswift-root` exactly once, alongside
    /// firmware + seed disk path. CH treats device paths and file
    /// paths identically through the `--disk path=` argument; this
    /// test pins that contract.
    #[test]
    fn test_disk_boot_block_device_path() {
        let mut cfg = make_disk_boot_config();
        cfg.disk_path = "/dev/kubeswift-root".to_string();
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("path=/dev/kubeswift-root"),
            "Block-mode disk path missing from CH args: {}",
            joined
        );
        // The legacy filesystem path must NOT appear when disk_path is a
        // device path — the substitution is total, not additive.
        assert!(
            !joined.contains("path=/data/image.raw"),
            "filesystem disk path leaked into Block-mode args: {}",
            joined
        );
        // --disk for the firmware-driven disk-boot path takes multiple
        // values (root + optional seed); both should be present and the
        // root value should be the device path.
        let disk_idx = args
            .iter()
            .position(|a| a == "--disk")
            .expect("--disk flag missing");
        assert!(
            args.get(disk_idx + 1)
                .map(|v| v == "path=/dev/kubeswift-root,image_type=raw,direct=on")
                .unwrap_or(false),
            "first --disk value should be the root device path with image_type=raw,direct=on; args: {:?}",
            args
        );
    }

    /// W9 — Block-mode root with a Filesystem-mode data disk produces
    /// CH args carrying both surfaces independently. The mixed case
    /// from architect Q4 mirrored at the swift-ch-client layer:
    /// disk_path (device) and data_disk_paths (file) coexist on the
    /// same `--disk` arg list with no suffix-driven re-routing.
    #[test]
    fn test_disk_boot_block_root_with_filesystem_data() {
        let mut cfg = make_disk_boot_config();
        cfg.disk_path = "/dev/kubeswift-root".to_string();
        cfg.data_disk_paths = vec!["/data/extra.raw".to_string()];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("path=/dev/kubeswift-root"),
            "Block root path missing: {}",
            joined
        );
        assert!(
            joined.contains("path=/data/extra.raw"),
            "Filesystem data path missing: {}",
            joined
        );
    }
}

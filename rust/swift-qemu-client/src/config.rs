//! QEMU command-line configuration for VM boot (Phase 1: no GPU).

use std::path::PathBuf;

/// Default QEMU binary. Override with KUBESWIFT_QEMU_BINARY env.
pub const DEFAULT_QEMU_BINARY: &str = "qemu-system-x86_64";

/// VFIO device for passthrough (GPU or SR-IOV NIC).
#[derive(Debug, Clone)]
pub struct QemuVFIODevice {
    /// Host PCI BDF address (e.g., "0000:3b:0a.0").
    pub host_address: String,
}

/// Network interface configuration for multi-NIC mode.
#[derive(Debug, Clone)]
pub struct QemuNICConfig {
    /// Tap device name (tap0, tap1, etc.)
    pub tap_name: String,
    /// MAC address for the interface.
    pub mac: String,
    /// Unique netdev ID (net0, net1, etc.)
    pub netdev_id: String,
}

/// QEMU VM configuration for disk boot (Phase 1 — no GPU/NUMA/hugepages).
#[derive(Debug, Clone)]
pub struct QemuConfig {
    /// Guest name embedded in -name arg.
    pub guest_id: String,
    /// Number of vCPUs.
    pub cpus: u32,
    /// Memory in MiB.
    pub memory_mib: u32,
    /// OVMF firmware code image (read-only pflash).
    pub ovmf_code: PathBuf,
    /// OVMF vars image (writable pflash, per-VM copy in runtime dir).
    pub ovmf_vars: PathBuf,
    /// Root disk image path (raw format).
    pub disk_path: String,
    /// Seed ISO path (cloud-init). Empty = no seed.
    pub seed_path: String,
    /// TAP device name for virtio-net (legacy single-NIC mode). None = no network.
    pub tap_name: Option<String>,
    /// MAC address for the virtio-net device (legacy single-NIC mode).
    pub mac: String,
    /// Network interfaces for multi-NIC mode. When non-empty, overrides tap_name/mac.
    pub nics: Vec<QemuNICConfig>,
    /// Serial console Unix socket path.
    pub serial_socket: PathBuf,
    /// QMP (QEMU Machine Protocol) Unix socket path.
    pub qmp_socket: PathBuf,
    /// Secondary data disk paths, in order. Empty = no data disks. Each
    /// produces a `-drive file=<p>,format=raw,if=virtio` argument after
    /// the root disk.
    pub data_disk_paths: Vec<String>,
    /// VFIO devices to pass through (SR-IOV VFs, GPUs).
    /// Each produces a -device vfio-pci,host=<address> argument.
    pub vfio_devices: Vec<QemuVFIODevice>,
}

impl QemuConfig {
    /// Build the full qemu-system-x86_64 argument list.
    pub fn to_args(&self) -> Vec<String> {
        let mut args = vec![
            "-name".to_string(),
            format!("guest={},debug-threads=on", self.guest_id),
            "-enable-kvm".to_string(),
            // memory-backend=ram0 routes the machine's main RAM to a shared
            // memfd backend (defined below) instead of QEMU's default private
            // anonymous mmap. share=on makes guest RAM host-visible/shared --
            // the QEMU analog of Cloud Hypervisor's `--memory shared=on`. It is
            // required for clean VFIO/GPU passthrough (the IOMMU pins guest RAM
            // for device DMA) and is the standard backing for snapshot/live-
            // migration-capable guests. Tier-1 PCIe GPUs run on Cloud Hypervisor
            // (already shared via #165); this covers the QEMU path used by
            // Tier-2/3 HGX GPUs (and non-GPU QEMU via the hypervisor override).
            "-machine".to_string(),
            "q35,accel=kvm,memory-backend=ram0".to_string(),
            "-cpu".to_string(),
            "host".to_string(),
            "-smp".to_string(),
            self.cpus.max(1).to_string(),
            "-m".to_string(),
            format!("{}M", self.memory_mib.max(128)),
            // Shared memfd backend for the main RAM (referenced by -machine
            // above). size MUST match -m. No prealloc here: VFIO pins on device
            // attach, and non-GPU guests keep lazy allocation.
            "-object".to_string(),
            format!(
                "memory-backend-memfd,id=ram0,size={}M,share=on",
                self.memory_mib.max(128)
            ),
        ];

        // OVMF firmware: code (read-only) + vars (writable, per-VM copy)
        args.extend([
            "-drive".to_string(),
            format!(
                "if=pflash,format=raw,readonly=on,file={}",
                self.ovmf_code.display()
            ),
        ]);
        args.extend([
            "-drive".to_string(),
            format!("if=pflash,format=raw,file={}", self.ovmf_vars.display()),
        ]);

        // Root disk (raw)
        if !self.disk_path.is_empty() {
            args.extend([
                "-drive".to_string(),
                format!("file={},format=raw,if=virtio", self.disk_path),
            ]);
        }

        // Seed ISO (cloud-init NoCloud)
        if !self.seed_path.is_empty() {
            args.extend([
                "-drive".to_string(),
                format!("file={},format=raw,if=virtio", self.seed_path),
            ]);
        }

        // Data disks (secondary, appear as /dev/vdb, /dev/vdc, ...)
        for p in &self.data_disk_paths {
            if p.is_empty() {
                continue;
            }
            args.extend([
                "-drive".to_string(),
                format!("file={},format=raw,if=virtio", p),
            ]);
        }

        // Virtio-net via TAP
        if !self.nics.is_empty() {
            // Multi-NIC mode: one netdev+device pair per NIC.
            for nic in &self.nics {
                args.extend([
                    "-netdev".to_string(),
                    format!(
                        "tap,id={},ifname={},script=no,downscript=no",
                        nic.netdev_id, nic.tap_name
                    ),
                ]);
                args.extend([
                    "-device".to_string(),
                    format!("virtio-net-pci,netdev={},mac={}", nic.netdev_id, nic.mac),
                ]);
            }
        } else if let Some(ref tap) = self.tap_name {
            // Legacy single-NIC mode.
            args.extend([
                "-netdev".to_string(),
                format!("tap,id=net0,ifname={},script=no,downscript=no", tap),
            ]);
            args.extend([
                "-device".to_string(),
                format!("virtio-net-pci,netdev=net0,mac={}", self.mac),
            ]);
        }

        // VFIO passthrough devices (SR-IOV VFs, GPUs).
        for dev in &self.vfio_devices {
            args.extend([
                "-device".to_string(),
                format!("vfio-pci,host={}", dev.host_address),
            ]);
        }

        // Serial console socket — server=on so guest can connect after QEMU starts
        args.extend([
            "-chardev".to_string(),
            format!(
                "socket,id=serial0,path={},server=on,wait=off",
                self.serial_socket.display()
            ),
        ]);
        args.extend(["-serial".to_string(), "chardev:serial0".to_string()]);

        // QMP socket for lifecycle management
        args.extend([
            "-qmp".to_string(),
            format!("unix:{},server=on,wait=off", self.qmp_socket.display()),
        ]);

        // No graphical display
        args.push("-nographic".to_string());
        args.extend(["-monitor".to_string(), "none".to_string()]);

        args
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_config() -> QemuConfig {
        QemuConfig {
            guest_id: "default/test".to_string(),
            cpus: 2,
            memory_mib: 2048,
            ovmf_code: PathBuf::from("/usr/share/OVMF/OVMF_CODE.fd"),
            ovmf_vars: PathBuf::from("/tmp/run/OVMF_VARS.fd"),
            disk_path: "/data/image.raw".to_string(),
            seed_path: "/data/seed.iso".to_string(),
            tap_name: Some("tap0".to_string()),
            mac: "52:54:00:12:34:56".to_string(),
            nics: vec![],
            vfio_devices: vec![],
            serial_socket: PathBuf::from("/tmp/run/serial.sock"),
            qmp_socket: PathBuf::from("/tmp/run/qmp.sock"),
            data_disk_paths: vec![],
        }
    }

    #[test]
    fn test_to_args_contains_machine() {
        let args = make_config().to_args();
        let joined = args.join(" ");
        assert!(joined.contains("q35,accel=kvm"), "missing q35 machine type");
        assert!(joined.contains("-enable-kvm"), "missing -enable-kvm");
    }

    #[test]
    fn test_to_args_memory_shared_memfd() {
        // Guest RAM must be a shared memfd backend (share=on), the QEMU analog
        // of CH's --memory shared=on: VFIO/GPU-DMA-friendly + migration/snapshot
        // capable. The machine must route its main RAM to that backend, and the
        // backend size must match -m.
        let cfg = make_config();
        let mem = cfg.memory_mib.max(128);
        let joined = cfg.to_args().join(" ");
        assert!(
            joined.contains("q35,accel=kvm,memory-backend=ram0"),
            "machine must reference the ram0 backend: {}",
            joined
        );
        assert!(
            joined.contains(&format!(
                "memory-backend-memfd,id=ram0,size={}M,share=on",
                mem
            )),
            "missing shared memfd backend matching -m: {}",
            joined
        );
        assert!(
            joined.contains(&format!("-m {}M", mem)),
            "missing -m size: {}",
            joined
        );
    }

    #[test]
    fn test_to_args_ovmf_pflash() {
        let args = make_config().to_args();
        let joined = args.join(" ");
        assert!(joined.contains("if=pflash"), "missing pflash");
        assert!(joined.contains("OVMF_CODE.fd"), "missing OVMF_CODE");
        assert!(joined.contains("OVMF_VARS.fd"), "missing OVMF_VARS");
    }

    #[test]
    fn test_to_args_serial_qmp() {
        let args = make_config().to_args();
        let joined = args.join(" ");
        assert!(joined.contains("serial.sock"), "missing serial socket");
        assert!(joined.contains("qmp.sock"), "missing QMP socket");
    }

    #[test]
    fn test_to_args_no_network() {
        let mut cfg = make_config();
        cfg.tap_name = None;
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            !joined.contains("-netdev"),
            "unexpected -netdev without tap"
        );
        assert!(
            !joined.contains("virtio-net"),
            "unexpected virtio-net without tap"
        );
    }

    #[test]
    fn test_to_args_no_seed() {
        let mut cfg = make_config();
        cfg.seed_path = String::new();
        let count = cfg
            .to_args()
            .iter()
            .filter(|a| a.contains("seed.iso"))
            .count();
        assert_eq!(count, 0, "seed path should be absent");
    }

    #[test]
    fn test_to_args_data_disk() {
        let mut cfg = make_config();
        cfg.data_disk_paths = vec!["/data/extra.raw".to_string()];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("file=/data/extra.raw,format=raw,if=virtio"),
            "missing data disk drive: {}",
            joined
        );
    }

    #[test]
    fn test_to_args_no_data_disk() {
        let cfg = make_config();
        let args = cfg.to_args();
        // Count -drive entries: should be exactly 3 (OVMF_CODE, OVMF_VARS, root disk + seed)
        let drive_count = args
            .iter()
            .filter(|a| a.starts_with("file=") || a.starts_with("if=pflash"))
            .count();
        // Just ensure no extra "extra.raw" appears
        let joined = args.join(" ");
        assert!(
            !joined.contains("extra.raw"),
            "unexpected data disk in args: {}",
            joined
        );
        assert!(drive_count > 0, "should have at least one drive arg");
    }

    #[test]
    fn test_empty_intent_uses_defaults() {
        // CH path: no disk, no seed, no network — just boot
        let mut cfg = make_config();
        cfg.disk_path = String::new();
        cfg.seed_path = String::new();
        cfg.tap_name = None;
        let args = cfg.to_args();
        let joined = args.join(" ");
        // Should still have firmware, serial, QMP
        assert!(joined.contains("OVMF_CODE.fd"));
        assert!(joined.contains("serial.sock"));
        assert!(joined.contains("qmp.sock"));
    }

    #[test]
    fn test_qemu_args_single_nic() {
        let mut cfg = make_config();
        cfg.tap_name = None;
        cfg.nics = vec![QemuNICConfig {
            tap_name: "tap0".to_string(),
            mac: "52:54:00:aa:bb:cc".to_string(),
            netdev_id: "net0".to_string(),
        }];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("tap,id=net0,ifname=tap0"),
            "netdev: {}",
            joined
        );
        assert!(
            joined.contains("virtio-net-pci,netdev=net0,mac=52:54:00:aa:bb:cc"),
            "device: {}",
            joined
        );
    }

    #[test]
    fn test_qemu_args_multi_nic() {
        let mut cfg = make_config();
        cfg.tap_name = None;
        cfg.nics = vec![
            QemuNICConfig {
                tap_name: "tap0".to_string(),
                mac: "52:54:00:01:00:00".to_string(),
                netdev_id: "net0".to_string(),
            },
            QemuNICConfig {
                tap_name: "tap1".to_string(),
                mac: "52:54:00:01:00:01".to_string(),
                netdev_id: "net1".to_string(),
            },
            QemuNICConfig {
                tap_name: "tap2".to_string(),
                mac: "52:54:00:01:00:02".to_string(),
                netdev_id: "net2".to_string(),
            },
        ];
        let args = cfg.to_args();
        let joined = args.join(" ");
        // 3 netdev + 3 device pairs
        let netdev_count = args.iter().filter(|a| a.starts_with("tap,id=")).count();
        assert_eq!(netdev_count, 3, "expected 3 netdevs: {}", joined);
        assert!(joined.contains("ifname=tap0"), "missing tap0: {}", joined);
        assert!(joined.contains("ifname=tap1"), "missing tap1: {}", joined);
        assert!(joined.contains("ifname=tap2"), "missing tap2: {}", joined);
    }

    #[test]
    fn test_qemu_args_no_nics_legacy() {
        // Legacy mode: nics empty, tap_name set -> single netdev+device
        let cfg = make_config(); // has tap_name=Some("tap0"), nics=vec![], mac="52:54:00:12:34:56"
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("tap,id=net0,ifname=tap0"),
            "legacy netdev: {}",
            joined
        );
        assert!(
            joined.contains("mac=52:54:00:12:34:56"),
            "legacy mac: {}",
            joined
        );
    }

    #[test]
    fn test_qemu_args_sriov_vfio_device() {
        let mut cfg = make_config();
        cfg.tap_name = None;
        cfg.nics = vec![];
        cfg.vfio_devices = vec![QemuVFIODevice {
            host_address: "0000:3b:0a.0".to_string(),
        }];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("vfio-pci,host=0000:3b:0a.0"),
            "missing vfio-pci device: {}",
            joined
        );
        // Should NOT have virtio-net
        assert!(
            !joined.contains("virtio-net"),
            "unexpected virtio-net with VFIO only: {}",
            joined
        );
    }

    #[test]
    fn test_qemu_args_mixed_bridge_and_sriov() {
        let mut cfg = make_config();
        cfg.tap_name = None;
        cfg.nics = vec![QemuNICConfig {
            tap_name: "tap0".to_string(),
            mac: "52:54:00:01:00:00".to_string(),
            netdev_id: "net0".to_string(),
        }];
        cfg.vfio_devices = vec![QemuVFIODevice {
            host_address: "0000:3b:0a.0".to_string(),
        }];
        let args = cfg.to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("virtio-net-pci"),
            "missing virtio-net for bridge: {}",
            joined
        );
        assert!(
            joined.contains("vfio-pci,host=0000:3b:0a.0"),
            "missing vfio-pci for SR-IOV: {}",
            joined
        );
    }
}

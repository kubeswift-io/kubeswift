//! QEMU command-line configuration for VM boot.
//!
//! Tier-2/3 HGX GPU topology (pcie-root-port per device, x-no-mmap, NUMA
//! memory backends, hugepages) follows the NVIDIA HGX Shared NVSwitch GPU
//! Passthrough Virtualization Integration Guide (WP-12736-002) and QEMU's
//! docs/pcie.txt: SXM-class GPUs must sit behind a PCI Express Root Port —
//! devices plugged straight into pcie.0 are Root Complex Integrated
//! Endpoints, which guest drivers/CUDA reject (CUDA init error 3 in the
//! field) — and each root port needs a unique (chassis, slot) pair.

use std::path::PathBuf;

/// Default QEMU binary. Override with KUBESWIFT_QEMU_BINARY env.
pub const DEFAULT_QEMU_BINARY: &str = "qemu-system-x86_64";

/// VFIO device for passthrough (GPU, NVSwitch, or SR-IOV NIC).
#[derive(Debug, Clone)]
pub struct QemuVFIODevice {
    /// Host PCI BDF address (e.g., "0000:3b:0a.0").
    pub host_address: String,
    /// Place the device behind its own pcie-root-port (Tier-2/3 SXM GPUs:
    /// CUDA refuses a flat topology). false = attach to pcie.0 directly
    /// (SR-IOV VFs, Tier-1-style devices).
    pub pcie_root_port: bool,
    /// Emit x-no-mmap=true — required for very large BARs (e.g. B200's
    /// 256 GiB region 2) to avoid multi-minute BAR-mapping boot stalls.
    pub no_mmap: bool,
}

/// One virtual guest NUMA node. Rendered as a shared memory backend plus a
/// `-numa node,...,memdev=` binding; the per-node sizes are authoritative for
/// total guest RAM (QEMU requires sum(memdev sizes) == -m).
#[derive(Debug, Clone)]
pub struct QemuNUMANode {
    /// Guest NUMA node id (0-based).
    pub id: u32,
    /// Guest CPU list in Linux range syntax (e.g., "0-39").
    pub cpus: String,
    /// Node memory in MiB.
    pub memory_mib: u64,
}

/// One vCPU→host-CPU pin. QEMU has no CLI for thread affinity, so pinning is
/// applied post-spawn: QMP query-cpus-fast maps vcpu index → host thread id,
/// then sched_setaffinity pins each thread (see `pinning`).
#[derive(Debug, Clone, Copy)]
pub struct QemuVCPUPin {
    pub vcpu: u32,
    pub host_cpu: u32,
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
    /// VFIO devices to pass through (SR-IOV VFs, GPUs, NVSwitches). A device
    /// with pcie_root_port=true gets its own pcie-root-port (unique chassis +
    /// slot per QEMU docs/pcie.txt); flat devices attach to pcie.0.
    pub vfio_devices: Vec<QemuVFIODevice>,
    /// Virtual guest NUMA topology (Tier-2/3 HGX). Empty = flat (single node,
    /// the shared memfd backend). Non-empty switches RAM to per-node shared
    /// backends bound with -numa node,memdev= — the per-node sizes then define
    /// total guest RAM (QEMU requires the sum to equal -m).
    pub numa_nodes: Vec<QemuNUMANode>,
    /// Hugepage size for guest RAM backing: "1G", "2M", or "" (none). Backs
    /// RAM with memory-backend-file on /dev/hugepages-… + prealloc=on (fail
    /// fast when the pool is short — a partially-backed GPU guest is a silent
    /// perf failure).
    pub hugepages: String,
    /// vCPU→host-CPU pins, applied post-spawn (not CLI-expressible).
    pub vcpu_pinning: Vec<QemuVCPUPin>,
}

impl QemuConfig {
    /// SMP topology: with a NUMA layout, expose sockets=<node count> so the
    /// guest sees one socket per NUMA node (matching the -numa cpu ranges the
    /// controller computed). QEMU requires sockets*cores*threads == vcpus, so
    /// fall back to a flat -smp when it doesn't divide evenly.
    fn smp_arg(&self) -> String {
        let cpus = self.cpus.max(1);
        let sockets = self.numa_nodes.len() as u32;
        if sockets > 1 && cpus % sockets == 0 {
            format!(
                "{},sockets={},cores={},threads=1",
                cpus,
                sockets,
                cpus / sockets
            )
        } else {
            cpus.to_string()
        }
    }

    /// The hugepage mount path for the configured page size. Kubernetes mounts
    /// hugepage emptyDirs per size; the launcher pod exposes them at the
    /// standard /dev/hugepages (1G — the GPU default) so keep the sketch's
    /// canonical path for 1G and the sized path for 2M.
    fn hugepage_path(&self) -> &'static str {
        match self.hugepages.as_str() {
            "2M" => "/dev/hugepages-2Mi",
            _ => "/dev/hugepages",
        }
    }

    /// One memory backend object string (shared, optionally hugepage-backed).
    fn memory_backend(&self, id: &str, size_mib: u64) -> String {
        if self.hugepages.is_empty() {
            // Shared memfd: the QEMU analog of CH's --memory shared=on. VFIO
            // pins guest RAM on device attach; non-GPU guests keep lazy alloc.
            format!("memory-backend-memfd,id={},size={}M,share=on", id, size_mib)
        } else {
            // Hugepage-backed file backend. prealloc=on: fail at spawn when
            // the pool is short instead of stalling at first touch.
            format!(
                "memory-backend-file,id={},size={}M,mem-path={},share=on,prealloc=on",
                id,
                size_mib,
                self.hugepage_path()
            )
        }
    }

    /// Build the full qemu-system-x86_64 argument list.
    pub fn to_args(&self) -> Vec<String> {
        // With a NUMA layout the per-node sizes are authoritative (QEMU
        // requires sum(memdev sizes) == -m); flat guests use memory_mib.
        let total_mib: u64 = if self.numa_nodes.is_empty() {
            self.memory_mib.max(128) as u64
        } else {
            self.numa_nodes.iter().map(|n| n.memory_mib).sum()
        };

        let mut args = vec![
            "-name".to_string(),
            format!("guest={},debug-threads=on", self.guest_id),
            "-enable-kvm".to_string(),
        ];

        if self.numa_nodes.is_empty() {
            // memory-backend=ram0 routes the machine's main RAM to a shared
            // backend (defined below) instead of QEMU's default private
            // anonymous mmap. share=on makes guest RAM host-visible/shared --
            // the QEMU analog of Cloud Hypervisor's `--memory shared=on`. It is
            // required for clean VFIO/GPU passthrough (the IOMMU pins guest RAM
            // for device DMA) and is the standard backing for snapshot/live-
            // migration-capable guests.
            args.extend([
                "-machine".to_string(),
                "q35,accel=kvm,memory-backend=ram0".to_string(),
            ]);
        } else {
            // NUMA mode: RAM comes from the per-node backends bound via
            // -numa node,memdev= below — the machine-level memory-backend=
            // must NOT be set as well (QEMU rejects the combination).
            args.extend(["-machine".to_string(), "q35,accel=kvm".to_string()]);
        }

        args.extend([
            "-cpu".to_string(),
            "host".to_string(),
            "-smp".to_string(),
            self.smp_arg(),
            "-m".to_string(),
            format!("{}M", total_mib),
        ]);

        if self.numa_nodes.is_empty() {
            args.extend([
                "-object".to_string(),
                self.memory_backend("ram0", total_mib),
            ]);
        } else {
            // Per-node shared backends + the NUMA bindings. cpus= uses the
            // Linux range syntax the controller computed from the profile.
            for node in &self.numa_nodes {
                args.extend([
                    "-object".to_string(),
                    self.memory_backend(&format!("ram{}", node.id), node.memory_mib),
                ]);
            }
            for node in &self.numa_nodes {
                args.extend([
                    "-numa".to_string(),
                    format!(
                        "node,nodeid={},cpus={},memdev=ram{}",
                        node.id, node.cpus, node.id
                    ),
                ]);
            }
        }

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

        // VFIO passthrough devices (SR-IOV VFs, GPUs, NVSwitches).
        //
        // Tier-2/3 SXM devices sit each behind their own pcie-root-port: a
        // device plugged straight into pcie.0 is a Root Complex Integrated
        // Endpoint, which the NVIDIA driver/CUDA reject (CUDA init error 3).
        // Per QEMU docs/pcie.txt the (chassis, slot) pair is mandatory and
        // must be unique per root port — derive both from the device index.
        // x-no-mmap=true avoids BAR-mapping boot stalls on very large BARs.
        for (i, dev) in self.vfio_devices.iter().enumerate() {
            let no_mmap = if dev.no_mmap { ",x-no-mmap=true" } else { "" };
            if dev.pcie_root_port {
                args.extend([
                    "-device".to_string(),
                    format!(
                        "pcie-root-port,id=rp{},bus=pcie.0,chassis={},slot={}",
                        i,
                        i + 1,
                        i + 1
                    ),
                ]);
                args.extend([
                    "-device".to_string(),
                    format!("vfio-pci,host={},bus=rp{}{}", dev.host_address, i, no_mmap),
                ]);
            } else {
                args.extend([
                    "-device".to_string(),
                    format!("vfio-pci,host={}{}", dev.host_address, no_mmap),
                ]);
            }
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
            numa_nodes: vec![],
            hugepages: String::new(),
            vcpu_pinning: vec![],
        }
    }

    fn flat_vfio(addr: &str) -> QemuVFIODevice {
        QemuVFIODevice {
            host_address: addr.to_string(),
            pcie_root_port: false,
            no_mmap: false,
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
        cfg.vfio_devices = vec![flat_vfio("0000:3b:0a.0")];
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
        cfg.vfio_devices = vec![flat_vfio("0000:3b:0a.0")];
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

#[cfg(test)]
mod hgx_tests {
    use super::*;

    fn rp_vfio(addr: &str, no_mmap: bool) -> QemuVFIODevice {
        QemuVFIODevice {
            host_address: addr.to_string(),
            pcie_root_port: true,
            no_mmap,
        }
    }

    /// A Tier-2 hgx-shared shape: 4 SXM GPUs (real HGX H100/H200 BDFs from the
    /// NVIDIA integration guide), 2 virtual NUMA nodes, 1G hugepages.
    fn tier2_config() -> QemuConfig {
        QemuConfig {
            guest_id: "default/hgx-tier2".to_string(),
            cpus: 80,
            memory_mib: 0, // NUMA sizes are authoritative
            ovmf_code: PathBuf::from("/usr/share/OVMF/OVMF_CODE.fd"),
            ovmf_vars: PathBuf::from("/tmp/run/OVMF_VARS.fd"),
            disk_path: "/data/image.raw".to_string(),
            seed_path: String::new(),
            tap_name: None,
            mac: String::new(),
            nics: vec![],
            serial_socket: PathBuf::from("/tmp/run/serial.sock"),
            qmp_socket: PathBuf::from("/tmp/run/qmp.sock"),
            data_disk_paths: vec![],
            vfio_devices: vec![
                rp_vfio("0000:0f:00.0", true),
                rp_vfio("0000:10:00.0", true),
                rp_vfio("0000:41:00.0", true),
                rp_vfio("0000:44:00.0", true),
            ],
            numa_nodes: vec![
                QemuNUMANode {
                    id: 0,
                    cpus: "0-39".to_string(),
                    memory_mib: 491_520,
                },
                QemuNUMANode {
                    id: 1,
                    cpus: "40-79".to_string(),
                    memory_mib: 491_520,
                },
            ],
            hugepages: "1G".to_string(),
            vcpu_pinning: vec![
                QemuVCPUPin {
                    vcpu: 0,
                    host_cpu: 8,
                },
                QemuVCPUPin {
                    vcpu: 1,
                    host_cpu: 9,
                },
            ],
        }
    }

    #[test]
    fn tier2_root_port_per_device() {
        // Each SXM GPU sits behind its OWN pcie-root-port with a unique
        // (chassis, slot) pair (QEMU docs/pcie.txt) and binds to it via
        // bus=rpN. Flat placement is what CUDA rejects (init error 3).
        let args = tier2_config().to_args();
        let joined = args.join(" ");
        for (i, bdf) in [
            "0000:0f:00.0",
            "0000:10:00.0",
            "0000:41:00.0",
            "0000:44:00.0",
        ]
        .iter()
        .enumerate()
        {
            let rp = format!(
                "pcie-root-port,id=rp{},bus=pcie.0,chassis={},slot={}",
                i,
                i + 1,
                i + 1
            );
            assert!(joined.contains(&rp), "missing root port {}: {}", rp, joined);
            let dev = format!("vfio-pci,host={},bus=rp{},x-no-mmap=true", bdf, i);
            assert!(joined.contains(&dev), "missing device {}: {}", dev, joined);
        }
        // The root port precedes its device (QEMU resolves bus= by prior definition).
        let rp0 = args
            .iter()
            .position(|a| a.contains("pcie-root-port,id=rp0"))
            .unwrap();
        let dev0 = args
            .iter()
            .position(|a| a.contains("host=0000:0f:00.0"))
            .unwrap();
        assert!(rp0 < dev0, "root port must be defined before its device");
    }

    #[test]
    fn tier2_numa_memory_layout() {
        // NUMA mode: no machine-level memory-backend=; one shared hugepage
        // backend per node; -numa node bindings; -m equals the node sum.
        let args = tier2_config().to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("-machine q35,accel=kvm ") || args.iter().any(|a| a == "q35,accel=kvm"),
            "NUMA mode must not set machine memory-backend=: {}",
            joined
        );
        assert!(
            !joined.contains("memory-backend=ram0"),
            "machine memory-backend= is invalid with -numa memdev: {}",
            joined
        );
        for n in 0..2 {
            assert!(
                joined.contains(&format!(
                    "memory-backend-file,id=ram{},size=491520M,mem-path=/dev/hugepages,share=on,prealloc=on",
                    n
                )),
                "missing hugepage backend ram{}: {}",
                n,
                joined
            );
        }
        assert!(
            joined.contains("node,nodeid=0,cpus=0-39,memdev=ram0"),
            "missing numa node 0: {}",
            joined
        );
        assert!(
            joined.contains("node,nodeid=1,cpus=40-79,memdev=ram1"),
            "missing numa node 1: {}",
            joined
        );
        assert!(
            joined.contains("-m 983040M"),
            "-m must equal node sum: {}",
            joined
        );
    }

    #[test]
    fn tier2_smp_sockets_match_numa() {
        let args = tier2_config().to_args();
        let joined = args.join(" ");
        assert!(
            joined.contains("-smp 80,sockets=2,cores=40,threads=1"),
            "smp topology must expose one socket per NUMA node: {}",
            joined
        );
    }

    #[test]
    fn smp_falls_back_flat_when_indivisible() {
        let mut cfg = tier2_config();
        cfg.cpus = 81; // not divisible by 2 sockets
        let joined = cfg.to_args().join(" ");
        assert!(
            joined.contains("-smp 81 "),
            "indivisible cpus must fall back to flat -smp: {}",
            joined
        );
    }

    #[test]
    fn flat_hugepages_backend() {
        // Flat (non-NUMA) + hugepages: the single ram0 backend switches from
        // memfd to a hugepage file backend; the machine still routes to it.
        let mut cfg = tier2_config();
        cfg.numa_nodes = vec![];
        cfg.memory_mib = 8192;
        let joined = cfg.to_args().join(" ");
        assert!(
            joined.contains("q35,accel=kvm,memory-backend=ram0"),
            "flat mode keeps machine memory-backend=: {}",
            joined
        );
        assert!(
            joined.contains(
                "memory-backend-file,id=ram0,size=8192M,mem-path=/dev/hugepages,share=on,prealloc=on"
            ),
            "flat hugepage backend: {}",
            joined
        );
        assert!(joined.contains("-m 8192M"), "{}", joined);
    }

    #[test]
    fn mixed_sriov_flat_and_gpu_root_port() {
        // An SR-IOV VF stays flat on pcie.0 while the GPU gets a root port;
        // chassis/slot derive from the combined index so they stay unique.
        let mut cfg = tier2_config();
        cfg.numa_nodes = vec![];
        cfg.memory_mib = 4096;
        cfg.hugepages = String::new();
        cfg.vfio_devices = vec![
            QemuVFIODevice {
                host_address: "0000:3b:0a.0".to_string(),
                pcie_root_port: false,
                no_mmap: false,
            },
            rp_vfio("0000:0f:00.0", false),
        ];
        let joined = cfg.to_args().join(" ");
        assert!(
            joined.contains("vfio-pci,host=0000:3b:0a.0 "),
            "SR-IOV VF must stay flat (no bus=): {}",
            joined
        );
        assert!(
            joined.contains("pcie-root-port,id=rp1,bus=pcie.0,chassis=2,slot=2"),
            "GPU root port at combined index: {}",
            joined
        );
        assert!(
            joined.contains("vfio-pci,host=0000:0f:00.0,bus=rp1"),
            "GPU binds to its root port: {}",
            joined
        );
        assert!(
            !joined.contains("host=0000:0f:00.0,bus=rp1,x-no-mmap"),
            "no x-no-mmap when not requested: {}",
            joined
        );
    }

    #[test]
    fn non_gpu_args_unchanged_regression() {
        // The flat/non-GPU arg shape must be byte-stable across the topology
        // work (the cluster's hypervisor-override QEMU guests ride it).
        let cfg = QemuConfig {
            guest_id: "default/plain".to_string(),
            cpus: 2,
            memory_mib: 2048,
            ovmf_code: PathBuf::from("/usr/share/OVMF/OVMF_CODE.fd"),
            ovmf_vars: PathBuf::from("/tmp/run/OVMF_VARS.fd"),
            disk_path: "/data/image.raw".to_string(),
            seed_path: String::new(),
            tap_name: None,
            mac: String::new(),
            nics: vec![],
            serial_socket: PathBuf::from("/tmp/run/serial.sock"),
            qmp_socket: PathBuf::from("/tmp/run/qmp.sock"),
            data_disk_paths: vec![],
            vfio_devices: vec![],
            numa_nodes: vec![],
            hugepages: String::new(),
            vcpu_pinning: vec![],
        };
        let joined = cfg.to_args().join(" ");
        assert!(
            joined.contains("q35,accel=kvm,memory-backend=ram0"),
            "{}",
            joined
        );
        assert!(
            joined.contains("memory-backend-memfd,id=ram0,size=2048M,share=on"),
            "{}",
            joined
        );
        assert!(joined.contains("-smp 2 "), "{}", joined);
        assert!(!joined.contains("pcie-root-port"), "{}", joined);
        assert!(!joined.contains("-numa"), "{}", joined);
    }
}

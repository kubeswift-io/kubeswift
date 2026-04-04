//! VM configuration for Cloud Hypervisor.

/// Cloud Hypervisor binary name. Override with KUBESWIFT_CH_BINARY env.
pub const DEFAULT_CH_BINARY: &str = "cloud-hypervisor";

/// Network interface configuration for multi-NIC mode.
#[derive(Debug, Clone)]
pub struct NICConfig {
    /// Tap device name (tap0, tap1, etc.)
    pub tap_name: String,
    /// MAC address for the interface.
    pub mac: String,
}

/// VM configuration derived from runtime intent.
#[derive(Debug, Clone)]
pub struct VmConfig {
    /// Path to root disk image.
    pub disk_path: String,
    /// Memory size in MiB.
    pub memory_mib: u32,
    /// Number of CPUs.
    pub cpus: u32,
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
    /// Optional secondary data disk path. Empty = no data disk.
    pub data_disk_path: String,
}

impl VmConfig {
    /// Build CH process arguments. Unix socket only; no TCP.
    /// Each option and its value must be separate argv elements (e.g. ["--api-socket", "path=/foo"]).
    /// For --disk, multiple disks are passed as multiple values to a single --disk (not repeated --disk).
    pub fn to_args(&self) -> Vec<String> {
        let mut args = vec![
            "--api-socket".to_string(),
            format!("path={}", self.api_socket),
            "--memory".to_string(),
            format!("size={}M", self.memory_mib),
            "--cpus".to_string(),
            format!("boot={}", self.cpus.max(1)),
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
                args.push("--disk".to_string());
                args.push(format!("path={}", self.seed_path));
            }
            if !self.data_disk_path.is_empty() {
                args.push("--disk".to_string());
                args.push(format!("path={}", self.data_disk_path));
            }
        } else {
            // --kernel (CLOUDHV.fd UEFI firmware) required for disk boot.
            if let Some(ref path) = self.firmware_path {
                args.push("--kernel".to_string());
                args.push(path.clone());
            }

            // --disk accepts multiple values: --disk path=/foo path=/bar
            args.push("--disk".to_string());
            args.push(format!("path={}", self.disk_path));
            if !self.seed_path.is_empty() {
                // Cloud Hypervisor: second disk for cloud-init NoCloud.
                // CH expects ISO or vfat; we pass directory path (see swift-ch-client README).
                args.push(format!("path={}", self.seed_path));
            }
            if !self.data_disk_path.is_empty() {
                args.push(format!("path={}", self.data_disk_path));
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
            // Multi-NIC mode: one --net per NIC with tap and mac.
            for nic in &self.nics {
                args.push("--net".to_string());
                args.push(format!("tap={},mac={}", nic.tap_name, nic.mac));
            }
        } else if let Some(ref tap) = self.tap_name {
            // Legacy single-NIC mode.
            args.push("--net".to_string());
            args.push(format!("tap={}", tap));
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
            api_socket: "/tmp/ch.sock".to_string(),
            seed_path: "/data/seed".to_string(),
            serial_socket_path: Some("/tmp/serial.sock".to_string()),
            firmware_path: Some("/usr/share/kubeswift-firmware/CLOUDHV.fd".to_string()),
            tap_name: Some("tap0".to_string()),
            nics: vec![],
            kernel_path: None,
            initramfs_path: None,
            kernel_cmdline: None,
            data_disk_path: String::new(),
        }
    }

    #[test]
    fn test_disk_boot_data_disk() {
        let mut cfg = make_disk_boot_config();
        cfg.data_disk_path = "/data/extra.raw".to_string();
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
        cfg.data_disk_path = "/data/extra.raw".to_string();
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
    fn test_ch_args_multi_nic() {
        let mut cfg = make_disk_boot_config();
        cfg.tap_name = None;
        cfg.nics = vec![
            NICConfig {
                tap_name: "tap0".to_string(),
                mac: "52:54:00:aa:bb:01".to_string(),
            },
            NICConfig {
                tap_name: "tap1".to_string(),
                mac: "52:54:00:aa:bb:02".to_string(),
            },
            NICConfig {
                tap_name: "tap2".to_string(),
                mac: "52:54:00:aa:bb:03".to_string(),
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
}

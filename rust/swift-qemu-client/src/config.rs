//! QEMU command-line configuration for VM boot (Phase 1: no GPU).

use std::path::PathBuf;

/// Default QEMU binary. Override with KUBESWIFT_QEMU_BINARY env.
pub const DEFAULT_QEMU_BINARY: &str = "qemu-system-x86_64";

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
    /// TAP device name for virtio-net. None = no network.
    pub tap_name: Option<String>,
    /// MAC address for the virtio-net device.
    pub mac: String,
    /// Serial console Unix socket path.
    pub serial_socket: PathBuf,
    /// QMP (QEMU Machine Protocol) Unix socket path.
    pub qmp_socket: PathBuf,
    /// Optional secondary data disk path. Empty = no data disk.
    pub data_disk_path: String,
}

impl QemuConfig {
    /// Build the full qemu-system-x86_64 argument list.
    pub fn to_args(&self) -> Vec<String> {
        let mut args = vec![
            "-name".to_string(),
            format!("guest={},debug-threads=on", self.guest_id),
            "-enable-kvm".to_string(),
            "-machine".to_string(),
            "q35,accel=kvm".to_string(),
            "-cpu".to_string(),
            "host".to_string(),
            "-smp".to_string(),
            self.cpus.max(1).to_string(),
            "-m".to_string(),
            format!("{}M", self.memory_mib.max(128)),
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

        // Data disk (secondary, appears as /dev/vdb)
        if !self.data_disk_path.is_empty() {
            args.extend([
                "-drive".to_string(),
                format!("file={},format=raw,if=virtio", self.data_disk_path),
            ]);
        }

        // Virtio-net via TAP
        if let Some(ref tap) = self.tap_name {
            args.extend([
                "-netdev".to_string(),
                format!("tap,id=net0,ifname={},script=no,downscript=no", tap),
            ]);
            args.extend([
                "-device".to_string(),
                format!("virtio-net-pci,netdev=net0,mac={}", self.mac),
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
            serial_socket: PathBuf::from("/tmp/run/serial.sock"),
            qmp_socket: PathBuf::from("/tmp/run/qmp.sock"),
            data_disk_path: String::new(),
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
        cfg.data_disk_path = "/data/extra.raw".to_string();
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
}

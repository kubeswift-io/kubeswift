//! VM configuration for Cloud Hypervisor.

/// Cloud Hypervisor binary name. Override with KUBESWIFT_CH_BINARY env.
pub const DEFAULT_CH_BINARY: &str = "cloud-hypervisor";

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
    /// Optional path to firmware (e.g. hypervisor-fw). Required for disk boot; CH creates serial device when VM is properly initialized.
    pub firmware_path: Option<String>,
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

        // --kernel (firmware) required for disk boot; CH creates serial device when VM is properly initialized.
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

        if let Some(ref path) = self.serial_socket_path {
            // Serial socket: bidirectional; connect with socat for interactive console.
            // Force kernel to use serial (ttyS0) regardless of image default; some images use hvc0.
            // Include root= for firmware boot; Ubuntu cloud images use /dev/vda1.
            args.push("--cmdline".to_string());
            args.push("console=ttyS0,115200n8 root=/dev/vda1 rw".to_string());
            args.push("--serial".to_string());
            args.push(format!("socket={}", path));
            // Disable virtio-console; serial is the interactive console.
            args.push("--console".to_string());
            args.push("off".to_string());
        }

        args
    }
}

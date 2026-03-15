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
            // Linux cloud images use console=ttyS0 (serial) for kernel/login output.
            args.push("--serial".to_string());
            args.push(format!("socket={}", path));
            // Disable virtio-console; serial is the interactive console.
            args.push("--console".to_string());
            args.push("off".to_string());
        }

        args
    }
}

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
}

impl VmConfig {
    /// Build CH process arguments. Unix socket only; no TCP.
    pub fn to_args(&self) -> Vec<String> {
        let mut args = vec![
            format!("--api-socket path={}", self.api_socket),
            format!("--disk path={}", self.disk_path),
            format!("--memory size={}M", self.memory_mib),
            format!("--cpus boot={}", self.cpus.max(1)),
        ];

        if !self.seed_path.is_empty() {
            // Cloud Hypervisor: second disk for cloud-init NoCloud.
            // CH expects ISO or vfat; we pass directory path (see swift-ch-client README).
            args.push(format!("--disk path={}", self.seed_path));
        }

        args
    }
}

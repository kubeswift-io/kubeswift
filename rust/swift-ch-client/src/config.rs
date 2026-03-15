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
    /// Each option and its value must be separate argv elements (e.g. ["--api-socket", "path=/foo"]).
    pub fn to_args(&self) -> Vec<String> {
        let mut args = vec![
            "--api-socket".to_string(),
            format!("path={}", self.api_socket),
            "--disk".to_string(),
            format!("path={}", self.disk_path),
            "--memory".to_string(),
            format!("size={}M", self.memory_mib),
            "--cpus".to_string(),
            format!("boot={}", self.cpus.max(1)),
        ];

        if !self.seed_path.is_empty() {
            // Cloud Hypervisor: second disk for cloud-init NoCloud.
            // CH expects ISO or vfat; we pass directory path (see swift-ch-client README).
            args.push("--disk".to_string());
            args.push(format!("path={}", self.seed_path));
        }

        args
    }
}

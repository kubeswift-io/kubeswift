//! Per-guest runtime directory creation and layout.

use std::path::{Path, PathBuf};

/// Environment variable for base runtime directory. Default: /var/lib/kubeswift/run
pub const ENV_RUN_DIR: &str = "KUBESWIFT_RUN_DIR";

/// Default base path for runtime directories.
pub const DEFAULT_RUN_DIR: &str = "/var/lib/kubeswift/run";

/// Subdirectory for NoCloud seed output.
pub const SEED_SUBDIR: &str = "seed";

/// Cloud Hypervisor API socket filename.
pub const CH_SOCKET_NAME: &str = "ch.sock";

/// Per-guest runtime directory paths.
#[derive(Debug)]
pub struct RuntimeDir {
    root: PathBuf,
}

impl RuntimeDir {
    /// Returns the root path of the runtime directory.
    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Returns the path for NoCloud seed output.
    pub fn seed_dir(&self) -> PathBuf {
        self.root.join(SEED_SUBDIR)
    }

    /// Returns the path for the Cloud Hypervisor API socket.
    pub fn api_socket(&self) -> PathBuf {
        self.root.join(CH_SOCKET_NAME)
    }
}

/// Creates a per-guest runtime directory.
///
/// `guest_id` is used for the directory name (e.g., "default/guest1").
/// Slashes are replaced with hyphens for filesystem safety.
/// `base_path` is the parent directory (e.g., /var/lib/kubeswift/run).
pub fn create_runtime_dir(guest_id: &str, base_path: &Path) -> Result<RuntimeDir, std::io::Error> {
    let safe_id = guest_id.replace('/', "-");
    let root = base_path.join(&safe_id);
    std::fs::create_dir_all(root.join(SEED_SUBDIR))?;
    Ok(RuntimeDir { root })
}

/// Returns the base runtime directory from env or default.
pub fn base_run_dir() -> PathBuf {
    std::env::var(ENV_RUN_DIR)
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from(DEFAULT_RUN_DIR))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_create_runtime_dir() {
        let tmp = tempfile::tempdir().unwrap();
        let base = tmp.path();

        let rt = create_runtime_dir("default/guest1", base).unwrap();
        assert!(rt.root().exists());
        assert!(rt.seed_dir().exists());
        assert!(rt.seed_dir().ends_with("seed"));
        assert!(rt.api_socket().ends_with("ch.sock"));
    }

    #[test]
    fn test_guest_id_sanitized() {
        let tmp = tempfile::tempdir().unwrap();
        let rt = create_runtime_dir("ns/name", tmp.path()).unwrap();
        assert!(rt.root().to_string_lossy().contains("ns-name"));
    }
}

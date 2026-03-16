//! NoCloud datasource directory layout builder.
//!
//! Reads from a ConfigMap mount path (user-data, meta-data, network-config)
//! and builds the NoCloud layout: three flat files at root level.
//! No subdirectories. No JSON wrapping. No OpenStack ConfigDrive layout.

use std::fs;
use std::path::Path;

/// ConfigMap key names (match internal/seed/configmap.go).
const KEY_USER_DATA: &str = "user-data";
const KEY_META_DATA: &str = "meta-data";
const KEY_NETWORK_CONFIG: &str = "network-config";

/// NoCloud output filenames (root level, plain text, exact names cloud-init expects).
const USER_DATA_FILE: &str = "user-data";
const META_DATA_FILE: &str = "meta-data";
const NETWORK_CONFIG_FILE: &str = "network-config";

/// Builds the NoCloud directory from ConfigMap mount path to output path.
///
/// Reads user-data, meta-data, network-config from `configmap_path` (directory
/// with files from Kubernetes ConfigMap volume) and writes three flat files
/// at `output_path` root. Content is copied as-is (plain text, no JSON).
pub fn build_nocloud_dir(configmap_path: &Path, output_path: &Path) -> Result<(), std::io::Error> {
    fs::create_dir_all(output_path)?;

    copy_if_exists(
        &configmap_path.join(KEY_USER_DATA),
        &output_path.join(USER_DATA_FILE),
    )?;
    copy_if_exists(
        &configmap_path.join(KEY_META_DATA),
        &output_path.join(META_DATA_FILE),
    )?;
    copy_if_exists(
        &configmap_path.join(KEY_NETWORK_CONFIG),
        &output_path.join(NETWORK_CONFIG_FILE),
    )?;

    Ok(())
}

fn copy_if_exists(src: &Path, dst: &Path) -> Result<(), std::io::Error> {
    if src.exists() {
        fs::copy(src, dst)?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_build_nocloud_dir() {
        let tmp = tempfile::tempdir().unwrap();
        let configmap = tmp.path().join("configmap");
        fs::create_dir_all(&configmap).unwrap();
        fs::write(configmap.join(KEY_USER_DATA), b"#cloud-config\n").unwrap();
        fs::write(
            configmap.join(KEY_META_DATA),
            "instance-id: kubeswift-001\n",
        )
        .unwrap();
        fs::write(configmap.join(KEY_NETWORK_CONFIG), "version: 2\n").unwrap();

        let out = tmp.path().join("nocloud");
        build_nocloud_dir(&configmap, &out).unwrap();

        assert!(out.join(USER_DATA_FILE).exists());
        assert!(out.join(META_DATA_FILE).exists());
        assert!(out.join(NETWORK_CONFIG_FILE).exists());
        assert!(!out.join("openstack").exists());
    }
}

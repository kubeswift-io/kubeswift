//! NoCloud datasource directory layout builder.
//!
//! Reads from a ConfigMap mount path (user-data, meta-data, network-config)
//! and builds the NoCloud v2 layout: openstack/latest/user_data, meta_data.json, network_config.json.

use std::fs;
use std::path::Path;

/// ConfigMap key names (match internal/seed/configmap.go).
const KEY_USER_DATA: &str = "user-data";
const KEY_META_DATA: &str = "meta-data";
const KEY_NETWORK_CONFIG: &str = "network-config";

/// NoCloud v2 subdirectory.
const OPENSTACK_LATEST: &str = "openstack/latest";

/// NoCloud v2 output filenames.
const USER_DATA_FILE: &str = "user_data";
const META_DATA_FILE: &str = "meta_data.json";
const NETWORK_CONFIG_FILE: &str = "network_config.json";

/// Builds the NoCloud directory from ConfigMap mount path to output path.
///
/// Reads user-data, meta-data, network-config from `configmap_path` (directory
/// with files from Kubernetes ConfigMap volume) and writes NoCloud v2 layout
/// to `output_path`.
pub fn build_nocloud_dir(configmap_path: &Path, output_path: &Path) -> Result<(), std::io::Error> {
    let openstack_latest = output_path.join(OPENSTACK_LATEST);
    fs::create_dir_all(&openstack_latest)?;

    copy_if_exists(
        &configmap_path.join(KEY_USER_DATA),
        &openstack_latest.join(USER_DATA_FILE),
    )?;
    copy_if_exists(
        &configmap_path.join(KEY_META_DATA),
        &openstack_latest.join(META_DATA_FILE),
    )?;
    copy_if_exists(
        &configmap_path.join(KEY_NETWORK_CONFIG),
        &openstack_latest.join(NETWORK_CONFIG_FILE),
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
        fs::write(configmap.join(KEY_META_DATA), "{}").unwrap();

        let out = tmp.path().join("nocloud");
        build_nocloud_dir(&configmap, &out).unwrap();

        assert!(out.join(OPENSTACK_LATEST).join(USER_DATA_FILE).exists());
        assert!(out.join(OPENSTACK_LATEST).join(META_DATA_FILE).exists());
    }
}

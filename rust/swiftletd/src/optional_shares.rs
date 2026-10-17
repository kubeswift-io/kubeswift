//! Optional artifacts of a cold sandbox.
//!
//! Each optional artifact's init container runs `sandbox-materialize
//! --optional`, which never fails the pod: on success it writes an ok token
//! (the digest it materialized) into a pod emptyDir; on any failure it writes
//! none. Before the VM boots, a share whose token is missing, or names a
//! different digest than the share's cache entry, is dropped together with its
//! `kubeswift.mounts` entry, and the guest starts without it.
//!
//! The decision is this pod's own init outcome, never the state of the
//! shared node cache: an entry another sandbox materialized (or verified with
//! another key) is not this sandbox's to mount.

use crate::intent::{FilesystemIntent, RuntimeIntent};
use std::path::Path;

/// Where the controller mounts the tokens (its `artifactResultsDir`).
pub const RESULTS_DIR: &str = "/var/lib/kubeswift/artifact-results";

/// The digest a cache entry path names: `.../sha256-<hex>` or
/// `.../sha256-<hex>.oci` -> `sha256:<hex>`.
fn entry_digest(source: &str) -> Option<String> {
    let base = Path::new(source).file_name()?.to_str()?;
    let base = base.strip_suffix(".oci").unwrap_or(base);
    let hex = base.strip_prefix("sha256-")?;
    (hex.len() == 64 && hex.bytes().all(|b| b.is_ascii_hexdigit()))
        .then(|| format!("sha256:{}", hex))
}

/// Whether an optional share may be made: its token is a file directly in
/// `results`, and names the digest of the share's cache entry.
fn token_ok(fs: &FilesystemIntent, results: &Path) -> bool {
    let token = Path::new(&fs.optional_token);
    if token.parent() != Some(results) {
        return false; // only a token the controller placed
    }
    let Some(digest) = entry_digest(&fs.source_path) else {
        return false;
    };
    match std::fs::read_to_string(token) {
        Ok(s) => s.trim() == digest,
        Err(_) => false,
    }
}

/// Removes `tag`'s entry from the cmdline's `kubeswift.mounts=` list, and the
/// parameter when it becomes empty.
fn drop_mount(cmdline: &str, tag: &str) -> String {
    let mut out = Vec::new();
    for arg in cmdline.split_whitespace() {
        if let Some(list) = arg.strip_prefix("kubeswift.mounts=") {
            let kept: Vec<&str> = list
                .split(',')
                .filter(|m| m.split(':').next() != Some(tag))
                .collect();
            if !kept.is_empty() {
                out.push(format!("kubeswift.mounts={}", kept.join(",")));
            }
            continue;
        }
        out.push(arg.to_string());
    }
    out.join(" ")
}

/// Drops the optional shares this pod did not materialize. Returns the names
/// of the dropped shares.
pub fn drop_unready(intent: &mut RuntimeIntent, results: &Path) -> Vec<String> {
    let Some(filesystems) = intent.filesystems.as_mut() else {
        return vec![];
    };
    let mut dropped = Vec::new();
    filesystems.retain(|fs| {
        if fs.optional_token.is_empty() || token_ok(fs, results) {
            return true;
        }
        dropped.push(fs.tag.clone());
        false
    });
    if let Some(kb) = intent.kernel_boot.as_mut() {
        for tag in &dropped {
            kb.cmdline = drop_mount(&kb.cmdline, tag);
        }
    }
    dropped
}

#[cfg(test)]
mod tests {
    use super::*;

    const D: &str = "sha256:1111111111111111111111111111111111111111111111111111111111111111";

    fn share(tag: &str, token: &str) -> FilesystemIntent {
        FilesystemIntent {
            name: tag.into(),
            tag: tag.into(),
            source_path: format!(
                "/var/lib/kubeswift/sandbox-artifacts/{}.oci",
                D.replace(':', "-")
            ),
            read_only: true,
            optional_token: token.into(),
        }
    }

    fn intent(fs: Vec<FilesystemIntent>) -> RuntimeIntent {
        let json = serde_json::json!({
            "rootDisk": {"path": "", "format": ""}, "seedPath": "", "cpu": 1, "memory": 256,
            "lifecycle": "start", "guestId": "ns/s",
            "kernelBoot": {"kernelPath": "/k", "initramfsPath": "/i",
                "cmdline": "console=ttyS0 kubeswift.mounts=sbxart0:/a,sbxart1:/b kubeswift.idle=0"},
        });
        let mut i: RuntimeIntent = serde_json::from_value(json).unwrap();
        i.filesystems = Some(fs);
        i
    }

    #[test]
    fn keeps_required_and_tokened_drops_the_rest() {
        let results = tempfile::tempdir().unwrap();
        let ok = results.path().join("a");
        std::fs::write(&ok, format!("{}\n", D)).unwrap();
        let mut i = intent(vec![
            share("sbxart0", ok.to_str().unwrap()),
            share("sbxart1", results.path().join("b").to_str().unwrap()), // no token
            share("sbxart2", ""),                                         // required
        ]);
        let dropped = drop_unready(&mut i, results.path());
        assert_eq!(dropped, vec!["sbxart1".to_string()]);
        let tags: Vec<_> = i.filesystems.unwrap().into_iter().map(|f| f.tag).collect();
        assert_eq!(tags, vec!["sbxart0", "sbxart2"]);
        let cmdline = i.kernel_boot.unwrap().cmdline;
        assert!(
            cmdline.contains("kubeswift.mounts=sbxart0:/a"),
            "{}",
            cmdline
        );
        assert!(!cmdline.contains("sbxart1"), "{}", cmdline);
    }

    // A token for another digest, or placed outside the results directory,
    // does not count: what decides is this pod's materialization of this
    // entry.
    #[test]
    fn token_must_name_the_entry_and_live_in_results() {
        let results = tempfile::tempdir().unwrap();
        let elsewhere = tempfile::tempdir().unwrap();
        let wrong = results.path().join("a");
        std::fs::write(
            &wrong,
            "sha256:2222222222222222222222222222222222222222222222222222222222222222\n",
        )
        .unwrap();
        let outside = elsewhere.path().join("a");
        std::fs::write(&outside, format!("{}\n", D)).unwrap();
        for token in [
            wrong.to_str().unwrap(),
            outside.to_str().unwrap(),
            "/etc/hostname",
        ] {
            let mut i = intent(vec![share("sbxart0", token)]);
            assert_eq!(
                drop_unready(&mut i, results.path()),
                vec!["sbxart0".to_string()],
                "{}",
                token
            );
        }
    }

    #[test]
    fn dropping_every_mount_removes_the_parameter() {
        assert_eq!(
            drop_mount("a kubeswift.mounts=sbxart0:/a b", "sbxart0"),
            "a b"
        );
        assert_eq!(
            drop_mount("kubeswift.mounts=sbxart0:/a,sbxart10:/b", "sbxart1"),
            "kubeswift.mounts=sbxart0:/a,sbxart10:/b"
        );
    }
}

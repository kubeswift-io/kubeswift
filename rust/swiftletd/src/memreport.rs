//! Memory report for a clone restored copyonwrite (#869).
//!
//! Such a guest's RAM is a private mapping of its snapshot's memory file:
//! pages no clone has written stay file pages shared by every clone of that
//! snapshot on the node, written pages become the clone's own. swiftletd
//! reads Cloud Hypervisor's `/proc/<pid>/smaps_rollup` and reports the split
//! on its pod (`kubeswift.io/guest-memory`), which the controller exports as
//! `kubeswift_guest_memory_bytes`. PSS divides a shared page among the
//! processes that map it, so the clones' file PSS adds up to the file's
//! resident size.

use std::time::Duration;

use kube::api::{Api, Patch, PatchParams};
use serde::Serialize;
use serde_json::json;

/// The annotation the report is written to.
pub const ANNOTATION: &str = "kubeswift.io/guest-memory";

/// How often the report is refreshed.
const INTERVAL: Duration = Duration::from_secs(60);

/// A process's memory split, in bytes.
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Usage {
    pub rss_bytes: u64,
    pub pss_file_bytes: u64,
    pub pss_anon_bytes: u64,
    pub pss_shmem_bytes: u64,
}

impl Usage {
    /// Whether `other` differs from self by at least 1% of RSS (or 1 MiB).
    fn changed(&self, other: &Usage) -> bool {
        let threshold = (self.rss_bytes / 100).max(1 << 20);
        let diff = |a: u64, b: u64| a.abs_diff(b) >= threshold;
        diff(self.rss_bytes, other.rss_bytes)
            || diff(self.pss_file_bytes, other.pss_file_bytes)
            || diff(self.pss_anon_bytes, other.pss_anon_bytes)
            || diff(self.pss_shmem_bytes, other.pss_shmem_bytes)
    }
}

/// Parses `/proc/<pid>/smaps_rollup` (values in kB).
pub fn parse_smaps_rollup(text: &str) -> Usage {
    let mut u = Usage::default();
    for line in text.lines() {
        let mut parts = line.split_whitespace();
        let (Some(key), Some(val)) = (parts.next(), parts.next()) else {
            continue;
        };
        let Ok(kb) = val.parse::<u64>() else { continue };
        let bytes = kb * 1024;
        match key {
            "Rss:" => u.rss_bytes = bytes,
            "Pss_File:" => u.pss_file_bytes = bytes,
            "Pss_Anon:" => u.pss_anon_bytes = bytes,
            "Pss_Shmem:" => u.pss_shmem_bytes = bytes,
            _ => {}
        }
    }
    u
}

/// Reports `pid`'s memory on pod `namespace/name` every minute, while the
/// process runs, when it changed. Runs on its own thread.
pub fn spawn(
    rt: std::sync::Arc<tokio::runtime::Runtime>,
    namespace: String,
    name: String,
    pid: u32,
) {
    std::thread::spawn(move || {
        let path = format!("/proc/{}/smaps_rollup", pid);
        let mut last: Option<Usage> = None;
        loop {
            let text = match std::fs::read_to_string(&path) {
                Ok(t) => t,
                Err(_) => return, // the process is gone
            };
            let usage = parse_smaps_rollup(&text);
            if last.is_none_or(|l| l.changed(&usage)) {
                match rt.block_on(report(&namespace, &name, &usage)) {
                    Ok(()) => last = Some(usage),
                    Err(e) => log::warn!("guest_memory_report_failed: {}", e),
                }
            }
            std::thread::sleep(INTERVAL);
        }
    });
}

async fn report(namespace: &str, name: &str, usage: &Usage) -> Result<(), String> {
    let client = crate::kube_client::create_client()
        .await
        .map_err(|e| e.to_string())?;
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client, namespace);
    let value = serde_json::to_string(usage).map_err(|e| e.to_string())?;
    let patch = json!({"metadata": {"annotations": {ANNOTATION: value}}});
    api.patch(name, &PatchParams::default(), &Patch::Merge(&patch))
        .await
        .map(|_| ())
        .map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    const ROLLUP: &str =
        "55a0c0000000-7ffd1e3fe000 ---p 00000000 00:00 0                      [rollup]
Rss:              421888 kB
Pss:              412160 kB
Pss_Dirty:         55296 kB
Pss_Anon:          54272 kB
Pss_File:         357888 kB
Pss_Shmem:             0 kB
Shared_Clean:      20480 kB
";

    #[test]
    fn parses_the_split() {
        let u = parse_smaps_rollup(ROLLUP);
        assert_eq!(u.rss_bytes, 421888 * 1024);
        assert_eq!(u.pss_file_bytes, 357888 * 1024);
        assert_eq!(u.pss_anon_bytes, 54272 * 1024);
        assert_eq!(u.pss_shmem_bytes, 0);
    }

    #[test]
    fn serializes_camel_case() {
        let v = serde_json::to_value(parse_smaps_rollup(ROLLUP)).unwrap();
        assert_eq!(v["pssFileBytes"], 357888 * 1024);
        assert!(v.get("rssBytes").is_some());
    }

    #[test]
    fn small_changes_are_not_reported() {
        let a = parse_smaps_rollup(ROLLUP);
        let mut b = a;
        b.pss_anon_bytes += 4096;
        assert!(!a.changed(&b));
        b.pss_anon_bytes += 64 << 20;
        assert!(a.changed(&b));
    }
}

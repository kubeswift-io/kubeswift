//! Use locks on node artifact-cache entries.
//!
//! The materializer evicts least-recently-used entries when the node cache is
//! over its limit (internal/sandbox/materialize/cachegc.go). It never evicts an
//! entry whose use lock someone holds. A launcher serving an entry (a cold
//! share, or a warm projection) takes a shared flock on
//! `<USE_DIR>/<entry>` and keeps it for its whole life: the kernel drops it
//! when the process dies, so a crash never leaks a reference. Taking it also
//! sets the file's mtime, the entry's last use. The lock files are never
//! unlinked, by anyone: a lock must stay on the inode an evictor locks.

use std::fs::{File, OpenOptions};
use std::os::unix::io::AsRawFd;
use std::path::Path;
use std::sync::Mutex;

/// The use-lock directory, as the launcher mounts it (read-write; it holds
/// only lock files).
pub const USE_DIR: &str = "/var/lib/kubeswift/sandbox-artifacts-use";

/// Held for the launcher's life.
static HELD: Mutex<Vec<File>> = Mutex::new(Vec::new());

fn valid_entry(name: &str) -> bool {
    let hex = name
        .strip_prefix("sha256-")
        .map(|s| s.strip_suffix(".oci").unwrap_or(s));
    matches!(hex, Some(h) if h.len() == 64 && h.bytes().all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)))
}

/// Takes and keeps a shared use lock on cache entry `entry` (a base name) in
/// `use_dir`, and records the use. Blocks only while an eviction holds the
/// exclusive lock (a short critical section): afterwards the caller's check
/// of the entry sees whether it is still there.
pub fn hold(use_dir: &Path, entry: &str) -> Result<(), String> {
    if !valid_entry(entry) {
        return Err(format!("use lock: {:?} is not a cache entry", entry));
    }
    let path = use_dir.join(entry);
    let f = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(&path)
        .map_err(|e| format!("use lock {}: {}", path.display(), e))?;
    // SAFETY: flock on an fd we own; no memory is shared.
    if unsafe { libc::flock(f.as_raw_fd(), libc::LOCK_SH) } != 0 {
        return Err(format!(
            "use lock {}: {}",
            path.display(),
            std::io::Error::last_os_error()
        ));
    }
    let _ = f.set_modified(std::time::SystemTime::now());
    HELD.lock().unwrap_or_else(|p| p.into_inner()).push(f);
    Ok(())
}

/// Like `hold`, but a missing use directory (a launcher pod built before use
/// locks) is not an error: logged, and the entry is served without a lock,
/// as before.
pub fn hold_if_available(use_dir: &Path, entry: &str) {
    if !use_dir.is_dir() {
        log::warn!(
            "use_lock_unavailable dir={} entry={} (served without a lock)",
            use_dir.display(),
            entry
        );
        return;
    }
    if let Err(e) = hold(use_dir, entry) {
        log::warn!("use_lock_failed {}", e);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn holds_a_shared_lock_an_exclusive_one_cannot_take() {
        let dir = tempfile::tempdir().unwrap();
        let entry = format!("sha256-{}.oci", "a".repeat(64));
        hold(dir.path(), &entry).unwrap();
        let other = File::open(dir.path().join(&entry)).unwrap();
        // SAFETY: flock on an fd we own.
        let rc = unsafe { libc::flock(other.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) };
        assert_ne!(rc, 0, "an evictor could lock an entry in use");
        // Another shared holder is fine.
        let rc = unsafe { libc::flock(other.as_raw_fd(), libc::LOCK_SH | libc::LOCK_NB) };
        assert_eq!(rc, 0);
    }

    #[test]
    fn refuses_names_that_are_not_entries() {
        let dir = tempfile::tempdir().unwrap();
        for bad in ["../x", "/etc/passwd", "sha256-zz", "", "sha256-AAAA"] {
            assert!(hold(dir.path(), bad).is_err(), "{:?}", bad);
        }
    }
}

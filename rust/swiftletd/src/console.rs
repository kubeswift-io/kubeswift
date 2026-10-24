//! The sandbox's workload console, for the unprivileged `console` container.
//!
//! The controller gives a sandbox launcher a `console` volume, shared
//! read-only with a `console` container that copies one file to its stdout,
//! so the console is read with the ordinary pod logs API (`get pods/log`,
//! `-c console`) instead of an exec into this privileged container. That
//! volume holds only console files: no socket, no runtime state.
//!
//! - A cold sandbox's guest console (`serial.log`): Cloud Hypervisor writes
//!   `<run>/serial.sock.log`, which is a link to it here.
//! - A warm slot's checked-out workload (`workload.log`): written here, as it
//!   runs, from the guest agent's stream, and bounded ([`WorkloadLog`]). The
//!   slot's own boot console stays in the run directory, so the console
//!   container of a slot shows the workload's output and nothing earlier.
//!
//! Without the volume (`KUBESWIFT_CONSOLE_DIR` unset: an older controller)
//! both files stay in the run directory, as before.

use std::fs::{File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

/// Environment variable naming the console volume's mount point.
pub const DIR_ENV: &str = "KUBESWIFT_CONSOLE_DIR";
/// Environment variable bounding the workload log, in bytes.
pub const MAX_BYTES_ENV: &str = "KUBESWIFT_CONSOLE_MAX_BYTES";
/// Default bound on a workload's console output kept on the node.
pub const DEFAULT_MAX_BYTES: u64 = 16 << 20;

/// File name of a cold sandbox's guest console in the console directory.
pub const SERIAL_LOG: &str = "serial.log";
/// File name of a checked-out workload's output (here and in the run dir).
pub const WORKLOAD_LOG: &str = "workload.log";

/// FIFO in the console directory this launcher holds open while it lives.
/// The console container reads it, and the kernel closes it however this
/// process ends (SIGKILL and OOM too): the console container then copies
/// what is left and exits, so the pod ends with its launcher.
pub const ALIVE_FIFO: &str = ".launcher";

/// Creates the console directory's FIFO and keeps it open for the life of
/// the process. A no-op without a console directory.
pub fn hold_alive() {
    let Some(dir) = dir() else {
        return;
    };
    if let Err(e) = hold_alive_in(&dir) {
        log::warn!("console_alive error={}", e);
    }
}

fn hold_alive_in(dir: &Path) -> std::io::Result<()> {
    use std::os::unix::ffi::OsStrExt;
    use std::os::unix::fs::OpenOptionsExt;
    std::fs::create_dir_all(dir)?;
    let path = dir.join(ALIVE_FIFO);
    let _ = std::fs::remove_file(&path);
    let c = std::ffi::CString::new(path.as_os_str().as_bytes())
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidInput, e))?;
    // SAFETY: c is a valid NUL-terminated path.
    if unsafe { libc::mkfifo(c.as_ptr(), 0o644) } != 0 {
        return Err(std::io::Error::last_os_error());
    }
    // Read-write: opening never waits for a reader, and this process is the
    // FIFO's only writer. Never closed: it goes with the process.
    let f = OpenOptions::new()
        .read(true)
        .write(true)
        .custom_flags(libc::O_CLOEXEC)
        .open(&path)?;
    std::mem::forget(f);
    Ok(())
}

/// The console directory, when the controller gave this launcher one.
pub fn dir() -> Option<PathBuf> {
    std::env::var(DIR_ENV)
        .ok()
        .filter(|s| !s.is_empty())
        .map(PathBuf::from)
}

/// Points `link` (a run-directory path readers already use) at `name` in the
/// console directory, so what is written to `link` lands there. A no-op
/// without a console directory.
pub fn link_into_console(link: &Path, name: &str) -> std::io::Result<()> {
    let Some(dir) = dir() else {
        return Ok(());
    };
    link_into(&dir, link, name)
}

fn link_into(dir: &Path, link: &Path, name: &str) -> std::io::Result<()> {
    std::fs::create_dir_all(dir)?;
    match std::fs::remove_file(link) {
        Err(e) if e.kind() != std::io::ErrorKind::NotFound => return Err(e),
        _ => {}
    }
    // Readable by the unprivileged console container, whatever mode the
    // writer would create it with (Cloud Hypervisor truncates, keeps the mode).
    {
        use std::os::unix::fs::OpenOptionsExt;
        OpenOptions::new()
            .create(true)
            .append(true)
            .mode(0o644)
            .open(dir.join(name))?;
    }
    std::os::unix::fs::symlink(dir.join(name), link)
}

/// A workload's console output, bounded: past `max` bytes one notice is
/// written and the rest is counted, not kept. Writing never fails the
/// workload and never waits on a reader: the file is local, and the console
/// container reads it at its own pace.
pub struct WorkloadLog {
    file: Option<File>,
    max: u64,
    written: u64,
    dropped: u64,
}

impl WorkloadLog {
    /// Opens the workload log for `run_dir`'s launcher: in the console
    /// directory (with the run-directory path linked to it), else in the run
    /// directory.
    pub fn open(run_dir: &Path) -> Self {
        let max = std::env::var(MAX_BYTES_ENV)
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(DEFAULT_MAX_BYTES);
        let path = match dir() {
            Some(d) => {
                if let Err(e) = link_into(&d, &run_dir.join(WORKLOAD_LOG), WORKLOAD_LOG) {
                    log::warn!("workload_console_link error={}", e);
                }
                d.join(WORKLOAD_LOG)
            }
            None => run_dir.join(WORKLOAD_LOG),
        };
        Self::at(&path, max)
    }

    fn at(path: &Path, max: u64) -> Self {
        let file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(path)
            .map_err(|e| log::warn!("workload_console_open path={} error={}", path.display(), e))
            .ok();
        WorkloadLog {
            file,
            max,
            written: 0,
            dropped: 0,
        }
    }

    /// Appends output, or counts it once the bound is reached.
    pub fn write(&mut self, data: &[u8]) {
        if self.dropped > 0 || self.written >= self.max {
            self.dropped += data.len() as u64;
            return;
        }
        let room = (self.max - self.written) as usize;
        let (keep, rest) = data.split_at(data.len().min(room));
        if let Some(f) = self.file.as_mut() {
            let _ = f.write_all(keep);
        }
        self.written += keep.len() as u64;
        if !rest.is_empty() {
            self.dropped += rest.len() as u64;
            if let Some(f) = self.file.as_mut() {
                let _ = write!(
                    f,
                    "\n[kubeswift: workload console output past {} bytes is not kept]\n",
                    self.max
                );
            }
        }
    }

    /// Bytes kept and bytes dropped.
    pub fn totals(&self) -> (u64, u64) {
        (self.written, self.dropped)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmp(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("console-test-{}-{}", tag, std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    #[test]
    fn workload_log_is_bounded_with_one_notice() {
        let d = tmp("bound");
        let path = d.join(WORKLOAD_LOG);
        let mut w = WorkloadLog::at(&path, 10);
        w.write(b"12345");
        w.write(b"67890abc");
        w.write(b"more");
        assert_eq!(w.totals(), (10, 7));
        let got = std::fs::read(&path).unwrap();
        assert!(
            got.starts_with(b"1234567890\n[kubeswift: "),
            "{:?}",
            String::from_utf8_lossy(&got)
        );
        assert_eq!(
            got.windows(9).filter(|x| x == b"kubeswift").count(),
            1,
            "one notice"
        );
    }

    #[test]
    fn binary_output_is_kept_as_is() {
        let d = tmp("bin");
        let path = d.join(WORKLOAD_LOG);
        let mut w = WorkloadLog::at(&path, 1 << 20);
        w.write(&[0, 0xff, 0xfe, b'\n', 0x1b, b'[']);
        assert_eq!(
            std::fs::read(&path).unwrap(),
            vec![0, 0xff, 0xfe, b'\n', 0x1b, b'[']
        );
    }

    #[test]
    fn a_run_dir_path_links_into_the_console_dir() {
        let d = tmp("link");
        let console = d.join("console");
        let run = d.join("run");
        std::fs::create_dir_all(&run).unwrap();
        let link = run.join("serial.sock.log");
        std::fs::write(&link, b"stale").unwrap();
        link_into(&console, &link, SERIAL_LOG).unwrap();
        std::fs::write(&link, b"boot\n").unwrap(); // what CH does
        assert_eq!(std::fs::read(console.join(SERIAL_LOG)).unwrap(), b"boot\n");
        assert!(std::fs::symlink_metadata(&link)
            .unwrap()
            .file_type()
            .is_symlink());
    }
}

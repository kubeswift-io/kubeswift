//! The sandbox's workload console, for the unprivileged `console` container.
//!
//! The controller gives a sandbox launcher a `console` volume, shared
//! read-only with a `console` container that copies one file to its stdout,
//! so the console is read with the ordinary pod logs API (`get pods/log`,
//! `-c console`) instead of an exec into this privileged container. That
//! volume holds only console files: no socket, no runtime state.
//!
//! - A cold sandbox's guest console (`serial.log`): `<run>/serial.sock.log`,
//!   which is a link to it here. Cloud Hypervisor writes the console to a FIFO
//!   ([`SERIAL_PIPE`]) and swiftletd copies it there, bounded ([`SerialPump`]):
//!   a guest that writes to its console without end cannot fill the node's
//!   disk. A warm slot's boot console goes through the same pump, into the run
//!   directory.
//! - A warm slot's checked-out workload (`workload.log`): written here, as it
//!   runs, from the guest agent's stream, and bounded ([`WorkloadLog`]). The
//!   slot's own boot console stays in the run directory, so the console
//!   container of a slot shows the workload's output and nothing earlier.
//!
//! Without the volume (`KUBESWIFT_CONSOLE_DIR` unset: an older controller)
//! both files stay in the run directory, as before.

use std::collections::VecDeque;
use std::fs::{File, OpenOptions};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

/// Environment variable naming the console volume's mount point.
pub const DIR_ENV: &str = "KUBESWIFT_CONSOLE_DIR";
/// Environment variable bounding the workload log and a sandbox's guest
/// console, in bytes (each).
pub const MAX_BYTES_ENV: &str = "KUBESWIFT_CONSOLE_MAX_BYTES";
/// Default bound on a workload's console output kept on the node.
pub const DEFAULT_MAX_BYTES: u64 = 16 << 20;
/// File name, in the run directory, of the FIFO Cloud Hypervisor writes a
/// sandbox's guest console to.
pub const SERIAL_PIPE: &str = "serial.pipe";
/// How much of the end of a guest console past the bound is kept: the
/// bridge-init's exit-code marker is in its last lines, and swiftletd reads
/// it from there (`report::SANDBOX_CONSOLE_TAIL_BYTES`).
pub const SERIAL_TAIL_BYTES: usize = 64 << 10;

fn max_bytes() -> u64 {
    std::env::var(MAX_BYTES_ENV)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(DEFAULT_MAX_BYTES)
}

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
    // Made and opened under a temporary name, then renamed into place: the
    // console container never sees the FIFO without its writer.
    let tmp = dir.join(format!("{}.tmp", ALIVE_FIFO));
    let _ = std::fs::remove_file(&tmp);
    let c = std::ffi::CString::new(tmp.as_os_str().as_bytes())
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
        .open(&tmp)?;
    std::fs::rename(&tmp, dir.join(ALIVE_FIFO))?;
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
        let max = max_bytes();
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
        let room = self.max.saturating_sub(self.written) as usize;
        let (keep, rest) = data.split_at(data.len().min(room));
        if !keep.is_empty() {
            if let Some(f) = self.file.as_mut() {
                let _ = f.write_all(keep);
            }
            self.written += keep.len() as u64;
        }
        if !rest.is_empty() {
            if self.dropped == 0 {
                if let Some(f) = self.file.as_mut() {
                    let _ = write!(
                        f,
                        "\n[kubeswift: workload console output past {} bytes is not kept]\n",
                        self.max
                    );
                }
            }
            self.dropped += rest.len() as u64;
        }
    }

    /// Bytes kept and bytes dropped.
    pub fn totals(&self) -> (u64, u64) {
        (self.written, self.dropped)
    }
}

/// A guest console, bounded: the first `max` bytes, then one notice, and when
/// the console ends, the last [`SERIAL_TAIL_BYTES`] of what was not kept. The
/// end is what matters after the bound: the exit-code marker is in it.
struct BoundedConsole {
    file: Option<File>,
    max: u64,
    written: u64,
    dropped: u64,
    tail: VecDeque<u8>,
}

impl BoundedConsole {
    fn new(file: Option<File>, max: u64) -> Self {
        BoundedConsole {
            file,
            max,
            written: 0,
            dropped: 0,
            tail: VecDeque::new(),
        }
    }

    fn put(&mut self, data: &[u8]) {
        // A write that fails (a full disk) is dropped: the guest's console is
        // never held up by its copy.
        if let Some(f) = self.file.as_mut() {
            let _ = f.write_all(data);
        }
    }

    fn write(&mut self, data: &[u8]) {
        let room = self.max.saturating_sub(self.written) as usize;
        let (keep, rest) = data.split_at(data.len().min(room));
        if !keep.is_empty() {
            self.put(keep);
            self.written += keep.len() as u64;
        }
        if rest.is_empty() {
            return;
        }
        if self.dropped == 0 {
            let notice = format!(
                "\n[kubeswift: console output past {} bytes is not kept; its last {} bytes follow when the guest stops]\n",
                self.max, SERIAL_TAIL_BYTES
            );
            self.put(notice.as_bytes());
        }
        self.dropped += rest.len() as u64;
        let rest = &rest[rest.len().saturating_sub(SERIAL_TAIL_BYTES)..];
        self.tail.extend(rest);
        let excess = self.tail.len().saturating_sub(SERIAL_TAIL_BYTES);
        self.tail.drain(..excess);
    }

    /// Writes the kept end, once the console has ended.
    fn finish(&mut self) {
        if self.dropped == 0 {
            return;
        }
        let skipped = self.dropped - self.tail.len() as u64;
        let notice = format!(
            "\n[kubeswift: {} bytes not kept; the last {} follow]\n",
            skipped,
            self.tail.len()
        );
        self.put(notice.as_bytes());
        let (a, b) = self.tail.as_slices();
        let (a, b) = (a.to_vec(), b.to_vec());
        self.put(&a);
        self.put(&b);
    }
}

/// Copies a sandbox's guest console from the FIFO Cloud Hypervisor writes to
/// into its console file, bounded. Started before Cloud Hypervisor, finished
/// once it has exited: everything it wrote is then in the FIFO, and the file
/// is complete before the exit-code marker is read from it. Dropping it
/// finishes it too.
pub struct SerialPump {
    stop: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}

impl SerialPump {
    /// Creates the FIFO at `pipe` and starts copying it into `out` (opened
    /// for append, following a link into the console directory).
    pub fn start(pipe: &Path, out: &Path) -> std::io::Result<Self> {
        Self::start_with(pipe, out, max_bytes())
    }

    fn start_with(pipe: &Path, out: &Path, max: u64) -> std::io::Result<Self> {
        use std::os::unix::ffi::OsStrExt;
        use std::os::unix::fs::OpenOptionsExt;
        match std::fs::remove_file(pipe) {
            Err(e) if e.kind() != std::io::ErrorKind::NotFound => return Err(e),
            _ => {}
        }
        let c = std::ffi::CString::new(pipe.as_os_str().as_bytes())
            .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidInput, e))?;
        // SAFETY: c is a valid NUL-terminated path.
        if unsafe { libc::mkfifo(c.as_ptr(), 0o600) } != 0 {
            return Err(std::io::Error::last_os_error());
        }
        // Read-write: neither this open nor Cloud Hypervisor's waits for the
        // other, and holding a writer means no end of file before it starts.
        // The end is the stop flag, set once Cloud Hypervisor has exited.
        let fifo = OpenOptions::new()
            .read(true)
            .write(true)
            .custom_flags(libc::O_CLOEXEC)
            .open(pipe)?;
        let file = OpenOptions::new()
            .create(true)
            .append(true)
            .mode(0o644)
            .open(out)
            .map_err(|e| log::warn!("sandbox_console_open path={} error={}", out.display(), e))
            .ok();
        let stop = Arc::new(AtomicBool::new(false));
        let flag = stop.clone();
        let thread = std::thread::Builder::new()
            .name("serial-pump".into())
            .spawn(move || pump(fifo, BoundedConsole::new(file, max), &flag))?;
        Ok(SerialPump {
            stop,
            thread: Some(thread),
        })
    }

    /// Copies what is left in the FIFO and writes the console's end.
    pub fn finish(self) {}
}

impl Drop for SerialPump {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
    }
}

fn pump(mut fifo: File, mut out: BoundedConsole, stop: &AtomicBool) {
    use std::os::unix::io::AsRawFd;
    let mut buf = vec![0u8; 64 << 10];
    loop {
        // Once stopped, only what is already in the FIFO is left: poll
        // without waiting until it is empty.
        let stopping = stop.load(Ordering::SeqCst);
        let mut pfd = libc::pollfd {
            fd: fifo.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        };
        // SAFETY: one valid pollfd.
        let n = unsafe { libc::poll(&mut pfd, 1, if stopping { 0 } else { 200 }) };
        if n < 0 {
            let e = std::io::Error::last_os_error();
            if e.kind() == std::io::ErrorKind::Interrupted {
                continue;
            }
            log::warn!("sandbox_console_poll error={}", e);
            break;
        }
        if n == 0 {
            if stopping {
                break;
            }
            continue;
        }
        match fifo.read(&mut buf) {
            Ok(0) => break,
            Ok(k) => out.write(&buf[..k]),
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(e) => {
                log::warn!("sandbox_console_read error={}", e);
                break;
            }
        }
    }
    out.finish();
    if out.dropped > 0 {
        log::info!(
            "sandbox_console_bounded kept={} dropped={}",
            out.written,
            out.dropped
        );
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
    fn the_notice_follows_a_write_that_ends_exactly_at_the_bound() {
        let d = tmp("exact");
        let path = d.join(WORKLOAD_LOG);
        let mut w = WorkloadLog::at(&path, 4);
        w.write(b"abcd");
        w.write(b"e");
        w.write(b"f");
        assert_eq!(w.totals(), (4, 2));
        let got = String::from_utf8(std::fs::read(&path).unwrap()).unwrap();
        assert!(
            got.starts_with("abcd\n[kubeswift: ") && got.matches("kubeswift").count() == 1,
            "{}",
            got
        );
    }

    #[test]
    fn the_alive_fifo_appears_with_its_writer() {
        let d = tmp("fifo");
        hold_alive_in(&d).unwrap();
        let path = d.join(ALIVE_FIFO);
        use std::os::unix::fs::FileTypeExt;
        assert!(std::fs::metadata(&path).unwrap().file_type().is_fifo());
        assert!(!d.join(format!("{}.tmp", ALIVE_FIFO)).exists());
        // A non-blocking reader finds a writer: EAGAIN, not EOF.
        use std::io::Read;
        use std::os::unix::fs::OpenOptionsExt;
        let mut r = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK)
            .open(&path)
            .unwrap();
        let err = r.read(&mut [0u8; 8]).unwrap_err();
        assert_eq!(err.kind(), std::io::ErrorKind::WouldBlock);
    }

    /// Writes `chunks` into the pump's FIFO as Cloud Hypervisor does
    /// (`File::create`: create + truncate, write-only), then finishes the pump
    /// as the launcher does once Cloud Hypervisor has exited.
    fn run_pump(tag: &str, max: u64, chunks: &[&[u8]]) -> Vec<u8> {
        let d = tmp(tag);
        let pipe = d.join(SERIAL_PIPE);
        let out = d.join("serial.sock.log");
        let pump = SerialPump::start_with(&pipe, &out, max).unwrap();
        {
            let mut w = File::create(&pipe).unwrap();
            for c in chunks {
                w.write_all(c).unwrap();
            }
        }
        pump.finish();
        std::fs::read(&out).unwrap()
    }

    #[test]
    fn a_console_under_the_bound_is_copied_whole() {
        let got = run_pump(
            "pump-small",
            1 << 20,
            &[b"boot\n", b"KUBESWIFT-EXIT-CODE=3\n"],
        );
        assert_eq!(got, b"boot\nKUBESWIFT-EXIT-CODE=3\n");
    }

    // #855: a guest that writes to its console without end keeps only the
    // head and the end, and the exit-code marker in the end is still read.
    #[test]
    fn an_endless_console_keeps_its_head_and_its_end() {
        let noise = vec![b'x'; 1 << 20];
        let mut chunks: Vec<&[u8]> = vec![b"boot\n"];
        for _ in 0..8 {
            chunks.push(&noise);
        }
        chunks.push(b"\nKUBESWIFT-EXIT-CODE=7\n");
        let got = run_pump("pump-big", 4096, &chunks);
        assert!(
            got.len() < 4096 + SERIAL_TAIL_BYTES + 512,
            "len {}",
            got.len()
        );
        assert!(got.starts_with(b"boot\n"));
        let text = String::from_utf8_lossy(&got);
        assert_eq!(text.matches("[kubeswift: ").count(), 2, "two notices");
        assert!(text.ends_with("KUBESWIFT-EXIT-CODE=7\n"));
        let d = tmp("pump-big-read");
        let f = d.join("console");
        std::fs::write(&f, &got).unwrap();
        let tail = crate::report::read_console_tail(
            f.to_str().unwrap(),
            crate::report::SANDBOX_CONSOLE_TAIL_BYTES,
        )
        .unwrap();
        assert_eq!(crate::report::parse_sandbox_exit_code(&tail), Some(7));
    }

    // The copy never holds the guest up: with its output file unwritable,
    // the FIFO is still drained, so the writer never blocks.
    #[test]
    fn the_writer_never_waits_for_the_copy() {
        let d = tmp("pump-nofile");
        let pipe = d.join(SERIAL_PIPE);
        let out = d.join("missing-dir").join("serial.sock.log");
        let pump = SerialPump::start_with(&pipe, &out, 1024).unwrap();
        let p = pipe.clone();
        let writer = std::thread::spawn(move || {
            let mut w = File::create(&p).unwrap();
            let chunk = vec![b'y'; 1 << 16];
            for _ in 0..64 {
                w.write_all(&chunk).unwrap(); // 4 MiB through a 64 KiB FIFO
            }
        });
        writer.join().unwrap();
        pump.finish();
        assert!(!out.exists());
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

//! spec.outputs: guest directories exported after the workload exits 0.
//!
//! Before boot, each output gets a fixed-size ext4 filesystem image in the
//! launcher's staging emptyDir, loop-mounted nodev,nosuid,noexec, and shared
//! read-write with the guest over virtio-fs. Its size bounds the bytes the
//! guest can write and its inode count bounds the entries. The guest never has
//! block access to the image: every write goes through virtiofsd, so the host
//! kernel only ever mounts a filesystem it wrote itself.
//!
//! After the microVM has powered off (virtiofsd is gone with it), each
//! directory is remounted read-only, walked without following links, checked
//! against its limits before a byte is written, and packed into a canonical
//! tar in the node spool, where the export Job picks it up. The result is
//! reported on the pod (kubeswift.io/sandbox-outputs). Nothing here holds or
//! needs registry credentials.

use std::collections::BTreeMap;
use std::ffi::CString;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::process::Command;

use serde::Serialize;
use sha2::{Digest, Sha256};

use crate::intent::{FilesystemIntent, OutputIntent};

/// Where the staging images and their mount points live (an emptyDir).
pub const STAGING_DIR: &str = "/var/lib/kubeswift/sandbox-output-staging";
/// Where packaged outputs are left for the export Job (this sandbox's own
/// hostPath directory, /var/lib/kubeswift/sandbox-outputs/<sandbox uid>).
pub const SPOOL_DIR: &str = "/var/lib/kubeswift/sandbox-outputs";
/// Host uid/gid range the guest's uids are mapped to on the output share:
/// guest root writes as an unprivileged host uid.
const MAPPED_ID_BASE: u32 = 100000;
const MAPPED_ID_COUNT: u32 = 65536;
/// Entries beyond maxEntries the image needs for itself.
const RESERVED_INODES: u64 = 16;
/// Free space the spool keeps beyond what a package needs.
const SPOOL_FLOOR: u64 = 64 << 20;
const MAX_DEPTH: usize = 64;
const MAX_PATH: usize = 4096;
/// The ustar size field holds 11 octal digits.
const USTAR_MAX_SIZE: u64 = 0o77777777777;
const EXT4_SUPER_MAGIC: i64 = 0xEF53;

/// An output ready to be shared with the guest.
pub struct Prepared {
    pub intent: OutputIntent,
    /// The mounted staging filesystem's root (virtiofsd --shared-dir).
    pub dir: PathBuf,
}

/// The per-output result written to kubeswift.io/sandbox-outputs.
#[derive(Debug, Clone, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct Report {
    pub name: String,
    /// Packaged, Failed or Skipped.
    pub state: &'static str,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub reason: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub message: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub content_digest: String,
    pub content_bytes: u64,
    pub entries: u64,
    pub archive_bytes: u64,
}

impl Report {
    fn failed(name: &str, reason: &str, message: String) -> Self {
        Report {
            name: name.to_string(),
            state: "Failed",
            reason: reason.to_string(),
            message: truncate(&message, 512),
            content_digest: String::new(),
            content_bytes: 0,
            entries: 0,
            archive_bytes: 0,
        }
    }
    pub fn skipped(name: &str, reason: &str, message: &str) -> Self {
        Report {
            state: "Skipped",
            ..Report::failed(name, reason, message.to_string())
        }
    }
}

/// The ext4 image size for an output: room for maxBytes of content plus the
/// filesystem's own metadata.
pub fn image_size(max_bytes: u64) -> u64 {
    max_bytes + std::cmp::max(64 << 20, max_bytes / 16)
}

/// Checks an intent's outputs: tags and paths as the controller builds them.
pub fn validate(outputs: &[OutputIntent]) -> Result<(), String> {
    if outputs.len() > 4 {
        return Err(format!("{} outputs, at most 4", outputs.len()));
    }
    for (i, o) in outputs.iter().enumerate() {
        if o.tag != format!("sbxout{}", i) {
            return Err(format!(
                "output {} has tag {:?}, want sbxout{}",
                o.name, o.tag, i
            ));
        }
        if o.name.is_empty()
            || o.name.len() > 40
            || !o
                .name
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        {
            return Err(format!("output name {:?} is not a DNS label", o.name));
        }
        if o.max_bytes == 0 || o.max_bytes > 10 << 30 {
            return Err(format!(
                "output {}: maxBytes {} out of range",
                o.name, o.max_bytes
            ));
        }
        if o.max_entries == 0 || o.max_entries > 1 << 20 {
            return Err(format!(
                "output {}: maxEntries {} out of range",
                o.name, o.max_entries
            ));
        }
    }
    Ok(())
}

/// Creates, formats and mounts each output's staging filesystem. Fails closed:
/// a share that is not the expected ext4 mount is never offered to the guest.
pub fn prepare(outputs: &[OutputIntent], staging: &Path) -> Result<Vec<Prepared>, String> {
    validate(outputs)?;
    let mut out = Vec::new();
    for o in outputs {
        let img = staging.join(format!("{}.img", o.name));
        let dir = staging.join(&o.name);
        fs::create_dir_all(&dir)
            .map_err(|e| format!("output {}: {}: {}", o.name, dir.display(), e))?;
        let f =
            File::create(&img).map_err(|e| format!("output {}: create image: {}", o.name, e))?;
        f.set_len(image_size(o.max_bytes))
            .map_err(|e| format!("output {}: size image: {}", o.name, e))?;
        drop(f);
        let owner = format!("root_owner={}:{}", MAPPED_ID_BASE, MAPPED_ID_BASE);
        run(Command::new("mkfs.ext4")
            .args(["-q", "-F", "-O", "^has_journal", "-m", "0"])
            .arg("-N")
            .arg((o.max_entries + RESERVED_INODES).to_string())
            .args(["-E", &owner])
            .arg(&img))
        .map_err(|e| format!("output {}: mkfs: {}", o.name, e))?;
        mount_loop(&img, &dir).map_err(|e| format!("output {}: mount: {}", o.name, e))?;
        verify_mount(&dir, staging).map_err(|e| format!("output {}: {}", o.name, e))?;
        // mkfs makes lost+found; it would otherwise be in every export.
        let _ = fs::remove_dir(dir.join("lost+found"));
        out.push(Prepared {
            intent: o.clone(),
            dir,
        });
    }
    Ok(out)
}

/// The virtio-fs share for a prepared output. `output` selects the hardened
/// virtiofsd arguments; it is never read from the intent.
pub fn share(p: &Prepared) -> FilesystemIntent {
    FilesystemIntent {
        name: format!("out-{}", p.intent.name),
        tag: p.intent.tag.clone(),
        source_path: p.dir.to_string_lossy().into_owned(),
        read_only: false,
        optional_token: String::new(),
        output: Some(p.intent.max_entries),
    }
}

/// virtiofsd arguments for an output share, fixed here. The daemon runs in
/// its own mount, pid and net namespaces with guest uids translated to an
/// unprivileged host range, no xattrs or ACLs, no file handles, and an
/// open-file limit sized to the share.
///
/// --translate-uid/gid, not --uid-map/--gid-map: virtiofsd runs as root here
/// and refuses a user-namespace map unless it is unprivileged. Translation
/// is done by virtiofsd itself and works as root.
pub fn virtiofsd_args(max_entries: u64) -> Vec<String> {
    let map = format!("map:0:{}:{}", MAPPED_ID_BASE, MAPPED_ID_COUNT);
    vec![
        "--sandbox".into(),
        "namespace".into(),
        format!("--translate-uid={}", map),
        format!("--translate-gid={}", map),
        "--inode-file-handles=never".into(),
        "--seccomp=kill".into(),
        format!("--rlimit-nofile={}", max_entries + 4096),
    ]
}

fn mount_loop(img: &Path, dir: &Path) -> Result<(), String> {
    // A privileged container's /dev is copied at start: loop devices created
    // later are missing from it. Ask the kernel for a free index and make the
    // node here. Two launchers can race for an index; retry.
    let mut last = String::new();
    for _ in 0..8 {
        let n = free_loop_index()?;
        let dev = format!("/dev/loop{}", n);
        if !Path::new(&dev).exists() {
            let c = CString::new(dev.clone()).unwrap();
            let rc = unsafe {
                libc::mknod(
                    c.as_ptr(),
                    libc::S_IFBLK | 0o600,
                    libc::makedev(7, n as u32),
                )
            };
            if rc != 0 {
                let e = io::Error::last_os_error();
                if e.kind() != io::ErrorKind::AlreadyExists {
                    return Err(format!("mknod {}: {}", dev, e));
                }
            }
        }
        // mount sets up the loop with autoclear: it detaches when unmounted,
        // including when the launcher's mount namespace goes away.
        for opts in [
            "loop={},nodev,nosuid,noexec,noatime,nosymfollow",
            "loop={},nodev,nosuid,noexec,noatime",
        ] {
            let o = opts.replace("{}", &dev);
            match run(Command::new("mount")
                .args(["-t", "ext4", "-o", &o])
                .arg(img)
                .arg(dir))
            {
                Ok(()) => return Ok(()),
                Err(e) => last = e,
            }
        }
    }
    Err(last)
}

fn free_loop_index() -> Result<i32, String> {
    const LOOP_CTL_GET_FREE: libc::c_ulong = 0x4C82;
    let ctl = OpenOptions::new()
        .read(true)
        .write(true)
        .open("/dev/loop-control")
        .map_err(|e| format!("/dev/loop-control: {}", e))?;
    use std::os::fd::AsRawFd;
    let n = unsafe { libc::ioctl(ctl.as_raw_fd(), LOOP_CTL_GET_FREE as _) };
    if n < 0 {
        return Err(format!("LOOP_CTL_GET_FREE: {}", io::Error::last_os_error()));
    }
    Ok(n)
}

/// The share must be the ext4 filesystem just mounted, not the directory
/// under it: without the mount there would be no size or entry bound.
fn verify_mount(dir: &Path, parent: &Path) -> Result<(), String> {
    let c = CString::new(dir.as_os_str().as_bytes()).unwrap();
    let mut st: libc::statfs = unsafe { std::mem::zeroed() };
    if unsafe { libc::statfs(c.as_ptr(), &mut st) } != 0 {
        return Err(format!(
            "statfs {}: {}",
            dir.display(),
            io::Error::last_os_error()
        ));
    }
    if st.f_type as i64 != EXT4_SUPER_MAGIC {
        return Err(format!(
            "{} is not the staging ext4 filesystem",
            dir.display()
        ));
    }
    let d = fs::metadata(dir).map_err(|e| e.to_string())?;
    let p = fs::metadata(parent).map_err(|e| e.to_string())?;
    if d.dev() == p.dev() {
        return Err(format!("{} is not a mount point", dir.display()));
    }
    Ok(())
}

/// One entry of an output, in archive order.
struct Entry {
    /// Path relative to the output root, '/'-separated.
    rel: String,
    dir: bool,
    size: u64,
    exec: bool,
    dev: u64,
    ino: u64,
}

/// Collects one output after the microVM has stopped: remount read-only,
/// walk, check, package. `stop` is polled between files (sandbox deletion).
pub fn collect(p: &Prepared, spool: &Path, stop: &dyn Fn() -> bool) -> Report {
    let name = p.intent.name.as_str();
    let _ = run(Command::new("mount").args(["-o", "remount,ro"]).arg(&p.dir));
    let entries = match walk(&p.dir) {
        Ok(e) => e,
        Err((reason, msg)) => return Report::failed(name, reason, msg),
    };
    let count = entries.len() as u64;
    if count > p.intent.max_entries {
        return Report::failed(
            name,
            "OutputTooManyEntries",
            format!("{} entries, at most {}", count, p.intent.max_entries),
        );
    }
    let bytes: u64 = entries.iter().filter(|e| !e.dir).map(|e| e.size).sum();
    if bytes > p.intent.max_bytes {
        return Report::failed(
            name,
            "OutputTooLarge",
            format!("{} bytes of files, at most {}", bytes, p.intent.max_bytes),
        );
    }
    // Room for the package: content, headers and padding, and a floor.
    let need = bytes + count * 2048 + SPOOL_FLOOR;
    match free_bytes(spool) {
        Ok(free) if free >= need => {}
        Ok(free) => {
            return Report::failed(
                name,
                "OutputSpoolFull",
                format!(
                    "the node spool has {} bytes free, the package needs {}",
                    free, need
                ),
            )
        }
        Err(e) => return Report::failed(name, "OutputCollectionFailed", e),
    }
    let tmp = spool.join(format!(".{}.tar.partial", name));
    let dst = spool.join(format!("{}.tar", name));
    let res = write_archive(&p.dir, &entries, &tmp, stop);
    match res {
        Ok((digest, len)) => {
            if let Err(e) = fs::rename(&tmp, &dst) {
                let _ = fs::remove_file(&tmp);
                return Report::failed(name, "OutputCollectionFailed", format!("rename: {}", e));
            }
            Report {
                name: name.to_string(),
                state: "Packaged",
                reason: String::new(),
                message: String::new(),
                content_digest: format!("sha256:{}", digest),
                content_bytes: bytes,
                entries: count,
                archive_bytes: len,
            }
        }
        Err((reason, msg)) => {
            let _ = fs::remove_file(&tmp);
            Report::failed(name, reason, msg)
        }
    }
}

fn free_bytes(dir: &Path) -> Result<u64, String> {
    let c = CString::new(dir.as_os_str().as_bytes()).unwrap();
    let mut st: libc::statvfs = unsafe { std::mem::zeroed() };
    if unsafe { libc::statvfs(c.as_ptr(), &mut st) } != 0 {
        return Err(format!(
            "statvfs {}: {}",
            dir.display(),
            io::Error::last_os_error()
        ));
    }
    Ok(st.f_bavail as u64 * st.f_frsize as u64)
}

type Refusal = (&'static str, String);

/// Lists the tree depth first, children in byte order, links never followed.
/// Only directories and regular files are accepted. A regular file with more
/// than one link is listed for each name, as an independent file.
fn walk(root: &Path) -> Result<Vec<Entry>, Refusal> {
    let mut out = Vec::new();
    walk_dir(root, "", 0, &mut out)?;
    Ok(out)
}

fn walk_dir(dir: &Path, rel: &str, depth: usize, out: &mut Vec<Entry>) -> Result<(), Refusal> {
    if depth > MAX_DEPTH {
        return Err((
            "OutputInvalidEntry",
            format!("{:?}: deeper than {} levels", rel, MAX_DEPTH),
        ));
    }
    let rd =
        fs::read_dir(dir).map_err(|e| ("OutputCollectionFailed", format!("{:?}: {}", rel, e)))?;
    let mut names: Vec<_> = rd
        .map(|e| e.map(|e| e.file_name()))
        .collect::<Result<_, _>>()
        .map_err(|e| ("OutputCollectionFailed", format!("{:?}: {}", rel, e)))?;
    names.sort_by(|a, b| a.as_bytes().cmp(b.as_bytes()));
    for n in names {
        let s = n.to_str().ok_or_else(|| {
            (
                "OutputInvalidEntry",
                format!("{:?}: a name that is not UTF-8", rel),
            )
        })?;
        if s.starts_with(".wh.") {
            return Err((
                "OutputInvalidEntry",
                format!(
                    "{:?}: names starting .wh. are reserved (layer whiteouts)",
                    join(rel, s)
                ),
            ));
        }
        let child_rel = join(rel, s);
        if child_rel.len() > MAX_PATH {
            return Err((
                "OutputInvalidEntry",
                format!("a path longer than {} bytes", MAX_PATH),
            ));
        }
        let path = dir.join(&n);
        let md = fs::symlink_metadata(&path)
            .map_err(|e| ("OutputCollectionFailed", format!("{:?}: {}", child_rel, e)))?;
        let ft = md.file_type();
        if ft.is_dir() {
            out.push(Entry {
                rel: child_rel.clone(),
                dir: true,
                size: 0,
                exec: true,
                dev: md.dev(),
                ino: md.ino(),
            });
            walk_dir(&path, &child_rel, depth + 1, out)?;
        } else if ft.is_file() {
            out.push(Entry {
                rel: child_rel,
                dir: false,
                size: md.len(),
                exec: md.mode() & 0o111 != 0,
                dev: md.dev(),
                ino: md.ino(),
            });
        } else {
            let kind = if ft.is_symlink() {
                "a symbolic link"
            } else {
                "a special file"
            };
            return Err((
                "OutputInvalidEntry",
                format!(
                    "{:?} is {}: only regular files and directories are exported",
                    child_rel, kind
                ),
            ));
        }
    }
    Ok(())
}

fn join(rel: &str, name: &str) -> String {
    if rel.is_empty() {
        name.to_string()
    } else {
        format!("{}/{}", rel, name)
    }
}

/// Writes the canonical archive: ustar, entries in walk order, uid/gid 0,
/// no owner names, mtime 0, mode 0755 for directories and executables and
/// 0644 otherwise, a PAX record only for a path or size ustar cannot hold.
/// Returns the sha256 (hex) and length of the bytes written.
fn write_archive(
    root: &Path,
    entries: &[Entry],
    dst: &Path,
    stop: &dyn Fn() -> bool,
) -> Result<(String, u64), Refusal> {
    let io_err = |e: io::Error| ("OutputCollectionFailed", e.to_string());
    let f = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(dst)
        .map_err(io_err)?;
    let mut w = HashingWriter::new(f);
    let mut buf = vec![0u8; 1 << 20];
    for e in entries {
        if stop() {
            return Err((
                "Deleted",
                "the sandbox was deleted during collection".into(),
            ));
        }
        let name = if e.dir {
            format!("{}/", e.rel)
        } else {
            e.rel.clone()
        };
        let size = if e.dir { 0 } else { e.size };
        let mut pax = BTreeMap::new();
        if name.len() > 100 {
            pax.insert("path", name.clone());
        }
        if size > USTAR_MAX_SIZE {
            pax.insert("size", size.to_string());
        }
        if !pax.is_empty() {
            let mut body = Vec::new();
            for (k, v) in &pax {
                body.extend(pax_record(k, v));
            }
            w.write_all(&header("././@PaxHeader", b'x', 0o644, body.len() as u64))
                .map_err(io_err)?;
            w.write_all(&body).map_err(io_err)?;
            pad(&mut w, body.len() as u64).map_err(io_err)?;
        }
        let short = if name.len() > 100 {
            &name[..100]
        } else {
            name.as_str()
        };
        let mode = if e.dir || e.exec { 0o755 } else { 0o644 };
        let header_size = if size > USTAR_MAX_SIZE { 0 } else { size };
        w.write_all(&header(
            short,
            if e.dir { b'5' } else { b'0' },
            mode,
            header_size,
        ))
        .map_err(io_err)?;
        if !e.dir {
            let path = root.join(&e.rel);
            let mut src = OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_NOATIME)
                .open(&path)
                .map_err(|err| ("OutputCollectionFailed", format!("{:?}: {}", e.rel, err)))?;
            let md = src.metadata().map_err(io_err)?;
            if !md.is_file() || md.dev() != e.dev || md.ino() != e.ino || md.len() != e.size {
                return Err((
                    "OutputCollectionFailed",
                    format!("{:?} changed during collection", e.rel),
                ));
            }
            let mut left = size;
            while left > 0 {
                let want = std::cmp::min(left, buf.len() as u64) as usize;
                let n = src.read(&mut buf[..want]).map_err(io_err)?;
                if n == 0 {
                    return Err((
                        "OutputCollectionFailed",
                        format!("{:?} is shorter than listed", e.rel),
                    ));
                }
                w.write_all(&buf[..n]).map_err(io_err)?;
                left -= n as u64;
            }
            pad(&mut w, size).map_err(io_err)?;
        }
    }
    w.write_all(&[0u8; 1024]).map_err(io_err)?;
    let (f, digest, len) = w.finish();
    f.sync_all().map_err(io_err)?;
    Ok((digest, len))
}

fn header(name: &str, typeflag: u8, mode: u32, size: u64) -> [u8; 512] {
    let mut h = [0u8; 512];
    h[..name.len()].copy_from_slice(name.as_bytes());
    octal(&mut h[100..108], mode as u64);
    octal(&mut h[108..116], 0);
    octal(&mut h[116..124], 0);
    octal(&mut h[124..136], size);
    octal(&mut h[136..148], 0);
    h[148..156].copy_from_slice(b"        ");
    h[156] = typeflag;
    h[257..263].copy_from_slice(b"ustar\0");
    h[263..265].copy_from_slice(b"00");
    octal(&mut h[329..337], 0);
    octal(&mut h[337..345], 0);
    let sum: u32 = h.iter().map(|&b| b as u32).sum();
    let s = format!("{:06o}\0 ", sum);
    h[148..156].copy_from_slice(s.as_bytes());
    h
}

/// Zero-padded octal, NUL-terminated, filling the field.
fn octal(field: &mut [u8], v: u64) {
    let w = field.len() - 1;
    let s = format!("{:0width$o}", v, width = w);
    field[..w].copy_from_slice(&s.as_bytes()[s.len() - w..]);
    field[w] = 0;
}

/// A PAX record "<len> <key>=<value>\n", where <len> counts itself.
fn pax_record(key: &str, value: &str) -> Vec<u8> {
    let base = key.len() + value.len() + 3;
    let mut len = base + base.to_string().len();
    loop {
        let next = base + len.to_string().len();
        if next == len {
            break;
        }
        len = next;
    }
    format!("{} {}={}\n", len, key, value).into_bytes()
}

fn pad<W: Write>(w: &mut W, size: u64) -> io::Result<()> {
    let r = (512 - (size % 512)) % 512;
    w.write_all(&vec![0u8; r as usize])
}

struct HashingWriter {
    inner: File,
    hash: Sha256,
    len: u64,
}

impl HashingWriter {
    fn new(inner: File) -> Self {
        HashingWriter {
            inner,
            hash: Sha256::new(),
            len: 0,
        }
    }
    fn finish(self) -> (File, String, u64) {
        let d = self.hash.finalize();
        (
            self.inner,
            d.iter().map(|b| format!("{:02x}", b)).collect(),
            self.len,
        )
    }
}

impl Write for HashingWriter {
    fn write(&mut self, b: &[u8]) -> io::Result<usize> {
        let n = self.inner.write(b)?;
        self.hash.update(&b[..n]);
        self.len += n as u64;
        Ok(n)
    }
    fn flush(&mut self) -> io::Result<()> {
        self.inner.flush()
    }
}

fn run(cmd: &mut Command) -> Result<(), String> {
    let out = cmd.output().map_err(|e| e.to_string())?;
    if out.status.success() {
        Ok(())
    } else {
        Err(String::from_utf8_lossy(&out.stderr).trim().to_string())
    }
}

fn truncate(s: &str, n: usize) -> String {
    if s.len() <= n {
        return s.to_string();
    }
    let mut end = n;
    while !s.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}...", &s[..end])
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;

    fn tmp(name: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("swl-out-{}-{}", name, std::process::id()));
        let _ = fs::remove_dir_all(&d);
        fs::create_dir_all(&d).unwrap();
        d
    }

    fn intent(max_bytes: u64, max_entries: u64) -> OutputIntent {
        OutputIntent {
            name: "out".into(),
            tag: "sbxout0".into(),
            path: "/out".into(),
            max_bytes,
            max_entries,
        }
    }

    fn prepared(dir: &Path, max_bytes: u64, max_entries: u64) -> Prepared {
        Prepared {
            intent: intent(max_bytes, max_entries),
            dir: dir.to_path_buf(),
        }
    }

    fn tree(root: &Path) {
        fs::create_dir_all(root.join("b/deep")).unwrap();
        fs::write(root.join("a.txt"), b"hello").unwrap();
        fs::write(root.join("b/deep/x.bin"), vec![7u8; 1500]).unwrap();
        let long = "n".repeat(120);
        fs::write(root.join("b").join(&long), b"long name").unwrap();
    }

    #[test]
    fn archive_is_canonical_and_tar_reads_it() {
        let src = tmp("canon-src");
        let spool = tmp("canon-spool");
        tree(&src);
        fs::hard_link(src.join("a.txt"), src.join("b/a-link.txt")).unwrap();
        let r = collect(&prepared(&src, 1 << 20, 100), &spool, &|| false);
        assert_eq!(r.state, "Packaged", "{:?}", r);
        assert_eq!(r.entries, 6);
        assert_eq!(r.content_bytes, 5 + 1500 + 9 + 5);

        // The same tree, written at another time with other ownership and
        // times, gives the same bytes.
        let src2 = tmp("canon-src2");
        let spool2 = tmp("canon-spool2");
        std::thread::sleep(std::time::Duration::from_millis(20));
        tree(&src2);
        fs::hard_link(src2.join("a.txt"), src2.join("b/a-link.txt")).unwrap();
        let r2 = collect(&prepared(&src2, 1 << 20, 100), &spool2, &|| false);
        assert_eq!(r.content_digest, r2.content_digest);

        // GNU tar parses it: names (including the PAX long name), modes.
        let out = Command::new("tar")
            .arg("-tvf")
            .arg(spool.join("out.tar"))
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "{}",
            String::from_utf8_lossy(&out.stderr)
        );
        let list = String::from_utf8_lossy(&out.stdout);
        assert!(list.contains(&format!("b/{}", "n".repeat(120))), "{}", list);
        assert!(
            list.contains("b/a-link.txt"),
            "hard link not exported as a file: {}",
            list
        );
        assert!(list.contains("-rw-r--r-- 0/0"), "{}", list);
        assert!(!list.contains("lost+found"));
    }

    #[test]
    fn refuses_links_and_special_files() {
        let src = tmp("refuse-link");
        fs::write(src.join("ok"), b"x").unwrap();
        symlink("/etc/shadow", src.join("escape")).unwrap();
        let r = collect(
            &prepared(&src, 1 << 20, 100),
            &tmp("refuse-link-spool"),
            &|| false,
        );
        assert_eq!(
            (r.state, r.reason.as_str()),
            ("Failed", "OutputInvalidEntry"),
            "{:?}",
            r
        );
        assert!(r.message.contains("symbolic link"));

        let src = tmp("refuse-fifo");
        let c = CString::new(src.join("pipe").as_os_str().as_bytes()).unwrap();
        assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o644) }, 0);
        let r = collect(
            &prepared(&src, 1 << 20, 100),
            &tmp("refuse-fifo-spool"),
            &|| false,
        );
        assert_eq!(r.reason, "OutputInvalidEntry", "{:?}", r);

        let src = tmp("refuse-wh");
        fs::write(src.join(".wh.hidden"), b"x").unwrap();
        let r = collect(
            &prepared(&src, 1 << 20, 100),
            &tmp("refuse-wh-spool"),
            &|| false,
        );
        assert_eq!(r.reason, "OutputInvalidEntry", "{:?}", r);
    }

    #[test]
    fn enforces_limits_before_writing() {
        let src = tmp("limits");
        tree(&src);
        let spool = tmp("limits-spool");
        let r = collect(&prepared(&src, 100, 100), &spool, &|| false);
        assert_eq!(r.reason, "OutputTooLarge", "{:?}", r);
        let r = collect(&prepared(&src, 1 << 20, 3), &spool, &|| false);
        assert_eq!(r.reason, "OutputTooManyEntries", "{:?}", r);
        // A sparse file counts at its size.
        let sparse = tmp("sparse");
        File::create(sparse.join("big"))
            .unwrap()
            .set_len(1 << 40)
            .unwrap();
        let r = collect(&prepared(&sparse, 1 << 30, 100), &spool, &|| false);
        assert_eq!(r.reason, "OutputTooLarge", "{:?}", r);
        assert!(
            fs::read_dir(&spool).unwrap().next().is_none(),
            "something was written"
        );
    }

    #[test]
    fn empty_output_is_packaged() {
        let src = tmp("empty");
        let spool = tmp("empty-spool");
        let r = collect(&prepared(&src, 1 << 20, 100), &spool, &|| false);
        assert_eq!(
            (r.state, r.entries, r.archive_bytes),
            ("Packaged", 0, 1024),
            "{:?}",
            r
        );
    }

    #[test]
    fn deletion_stops_collection_and_leaves_nothing() {
        let src = tmp("stop");
        tree(&src);
        let spool = tmp("stop-spool");
        let r = collect(&prepared(&src, 1 << 20, 100), &spool, &|| true);
        assert_eq!(r.reason, "Deleted", "{:?}", r);
        assert!(fs::read_dir(&spool).unwrap().next().is_none());
    }

    #[test]
    fn pax_record_counts_itself() {
        for v in ["x", &"y".repeat(90), &"z".repeat(995)] {
            let r = pax_record("path", v);
            let s = String::from_utf8(r.clone()).unwrap();
            let n: usize = s.split(' ').next().unwrap().parse().unwrap();
            assert_eq!(n, r.len(), "{}", s);
        }
    }

    #[test]
    fn validate_checks_tags_and_limits() {
        assert!(validate(&[intent(1, 1)]).is_ok());
        let mut bad = intent(1, 1);
        bad.tag = "sbxart0".into();
        assert!(validate(&[bad]).is_err());
        assert!(validate(&[intent(0, 1)]).is_err());
        assert!(validate(&[intent(11 << 30, 1)]).is_err());
    }
}

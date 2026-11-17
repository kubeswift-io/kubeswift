//! Read-only lookups in a sandbox's OCI rootfs, without mounting it: the
//! cached ext4 image through `debugfs` (opened read-only), or the virtio-fs
//! tree directly. Symlinks are resolved here, component by component and
//! confined to the rootfs, the way the guest would see them; nothing follows
//! a link on the host. Used to find the image's CA bundle (egress_secrets.rs).
use std::collections::VecDeque;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::OnceLock;

const MAX_HOPS: usize = 16;
const MAX_READ: usize = 8 << 20;

/// One path in the rootfs, not following a final symlink.
#[derive(Debug, Clone, PartialEq)]
pub enum Entry {
    Missing,
    File,
    Dir,
    Symlink(String),
}

pub trait RootfsReader: Send + Sync {
    fn lstat(&self, path: &str) -> Entry;
    fn read(&self, path: &str) -> Option<Vec<u8>>;
    /// Copies the directory `dir` (one level: regular files, and symlinks as
    /// symlinks) to `dest`, which must not exist. Refuses a directory with a
    /// subdirectory: a copy bound over the original would hide it.
    fn copy_dir(&self, dir: &str, dest: &Path) -> Result<(), String>;

    /// The path with every symlink resolved inside the rootfs, when it ends
    /// at a regular file.
    fn resolve(&self, path: &str) -> Option<String> {
        let mut todo: VecDeque<String> = split(path);
        let mut cur: Vec<String> = Vec::new();
        let mut hops = 0;
        while let Some(part) = todo.pop_front() {
            match part.as_str() {
                "" | "." => continue,
                ".." => {
                    cur.pop();
                    continue;
                }
                _ => {}
            }
            cur.push(part);
            let now = format!("/{}", cur.join("/"));
            match self.lstat(&now) {
                Entry::Missing => return None,
                Entry::Symlink(target) => {
                    hops += 1;
                    if hops > MAX_HOPS {
                        return None;
                    }
                    cur.pop();
                    if target.starts_with('/') {
                        cur.clear();
                    }
                    let mut next = split(&target);
                    next.extend(todo.drain(..));
                    todo = next;
                }
                Entry::File | Entry::Dir => {}
            }
        }
        let p = format!("/{}", cur.join("/"));
        (self.lstat(&p) == Entry::File).then_some(p)
    }
}

fn split(p: &str) -> VecDeque<String> {
    p.split('/')
        .filter(|s| !s.is_empty())
        .map(str::to_string)
        .collect()
}

/// A path safe to put in a debugfs request: no quoting or command syntax.
fn plain(p: &str) -> bool {
    !p.is_empty()
        && p.len() < 4096
        && p.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"/._-+@".contains(&b))
}

/// The cached ext4 image, through debugfs (read-only by default).
pub struct Ext4 {
    pub image: PathBuf,
}

impl Ext4 {
    fn debugfs(&self, request: &str) -> Option<Vec<u8>> {
        let out = Command::new("debugfs")
            .arg("-R")
            .arg(request)
            .arg(&self.image)
            .env("DEBUGFS_PAGER", "__none__")
            .env("PAGER", "__none__")
            .output()
            .ok()?;
        out.status.success().then_some(out.stdout)
    }
}

/// Parses `debugfs stat` output.
fn parse_stat(out: &str) -> Entry {
    let ty = out
        .split_whitespace()
        .skip_while(|w| *w != "Type:")
        .nth(1)
        .unwrap_or("");
    match ty {
        "regular" => Entry::File,
        "directory" => Entry::Dir,
        "symlink" => match out
            .split("Fast link dest: \"")
            .nth(1)
            .and_then(|r| r.split('"').next())
        {
            Some(t) if !t.is_empty() => Entry::Symlink(t.to_string()),
            // A slow symlink (target over 60 bytes): not followed.
            _ => Entry::Missing,
        },
        _ => Entry::Missing,
    }
}

/// Refuses a copied directory that holds a subdirectory.
fn flat(dest: &Path) -> Result<(), String> {
    let rd = std::fs::read_dir(dest).map_err(|e| format!("{}: {}", dest.display(), e))?;
    for e in rd.flatten() {
        if e.file_type().map(|t| t.is_dir()).unwrap_or(false) {
            return Err(format!(
                "{} has a subdirectory ({})",
                dest.display(),
                e.file_name().to_string_lossy()
            ));
        }
    }
    Ok(())
}

impl RootfsReader for Ext4 {
    fn copy_dir(&self, dir: &str, dest: &Path) -> Result<(), String> {
        if !plain(dir) || dir == "/" {
            return Err(format!("{} cannot be copied", dir));
        }
        let parent = dest.with_extension("dump");
        let _ = std::fs::remove_dir_all(&parent);
        std::fs::create_dir_all(&parent).map_err(|e| format!("{}: {}", parent.display(), e))?;
        let request = format!("rdump {} {}", dir, parent.display());
        let ok = Command::new("debugfs")
            .arg("-R")
            .arg(&request)
            .arg(&self.image)
            .env("DEBUGFS_PAGER", "__none__")
            .output()
            .map(|o| o.status.success())
            .unwrap_or(false);
        let base = Path::new(dir).file_name().map(|n| parent.join(n));
        let result = match base {
            Some(b) if ok && b.is_dir() => {
                std::fs::rename(&b, dest).map_err(|e| format!("{}: {}", dest.display(), e))
            }
            _ => Err(format!("debugfs could not copy {}", dir)),
        };
        let _ = std::fs::remove_dir_all(&parent);
        result?;
        flat(dest)
    }

    fn lstat(&self, path: &str) -> Entry {
        if !plain(path) {
            return Entry::Missing;
        }
        match self.debugfs(&format!("stat {}", path)) {
            Some(out) => match parse_stat(&String::from_utf8_lossy(&out)) {
                Entry::Symlink(t) if !plain(&t) => Entry::Missing,
                e => e,
            },
            None => Entry::Missing,
        }
    }
    fn read(&self, path: &str) -> Option<Vec<u8>> {
        if !plain(path) {
            return None;
        }
        self.debugfs(&format!("cat {}", path))
            .filter(|b| b.len() <= MAX_READ)
    }
}

/// The virtio-fs rootfs tree.
pub struct Tree {
    pub root: PathBuf,
}

impl Tree {
    fn host(&self, path: &str) -> PathBuf {
        self.root.join(path.trim_start_matches('/'))
    }
}

impl RootfsReader for Tree {
    fn copy_dir(&self, dir: &str, dest: &Path) -> Result<(), String> {
        if self.lstat(dir) != Entry::Dir {
            return Err(format!("{} is not a directory", dir));
        }
        std::fs::create_dir(dest).map_err(|e| format!("{}: {}", dest.display(), e))?;
        let rd = std::fs::read_dir(self.host(dir)).map_err(|e| format!("{}: {}", dir, e))?;
        for e in rd.flatten() {
            let t = e.file_type().map_err(|e| e.to_string())?;
            let to = dest.join(e.file_name());
            if t.is_symlink() {
                let target = std::fs::read_link(e.path()).map_err(|e| e.to_string())?;
                std::os::unix::fs::symlink(target, &to).map_err(|e| e.to_string())?;
            } else if t.is_file() {
                std::fs::copy(e.path(), &to).map_err(|e| e.to_string())?;
            } else if t.is_dir() {
                return Err(format!(
                    "{} has a subdirectory ({})",
                    dir,
                    e.file_name().to_string_lossy()
                ));
            }
        }
        Ok(())
    }

    fn lstat(&self, path: &str) -> Entry {
        match std::fs::symlink_metadata(self.host(path)) {
            Ok(m) if m.file_type().is_symlink() => std::fs::read_link(self.host(path))
                .ok()
                .and_then(|t| t.to_str().map(str::to_string))
                .map(Entry::Symlink)
                .unwrap_or(Entry::Missing),
            Ok(m) if m.is_dir() => Entry::Dir,
            Ok(m) if m.is_file() => Entry::File,
            _ => Entry::Missing,
        }
    }
    fn read(&self, path: &str) -> Option<Vec<u8>> {
        // resolve() has checked every component is not a link.
        let data = std::fs::read(self.host(path)).ok()?;
        (data.len() <= MAX_READ).then_some(data)
    }
}

/// A rootfs with nothing in it (no rootfs known): no image bundle.
pub struct Empty;

impl RootfsReader for Empty {
    fn copy_dir(&self, dir: &str, _: &Path) -> Result<(), String> {
        Err(format!("no rootfs to copy {} from", dir))
    }
    fn lstat(&self, _: &str) -> Entry {
        Entry::Missing
    }
    fn read(&self, _: &str) -> Option<Vec<u8>> {
        None
    }
}

static ROOTFS: OnceLock<Box<dyn RootfsReader>> = OnceLock::new();

/// Records this launcher's rootfs (call once at start).
pub fn set_from_intent(intent: &crate::intent::RuntimeIntent) {
    let reader: Box<dyn RootfsReader> = if let Some(p) = intent.sandbox_rootfs_path() {
        Box::new(Ext4 {
            image: PathBuf::from(p),
        })
    } else if let Some(fs) = intent
        .filesystems
        .as_ref()
        .and_then(|v| v.iter().find(|f| f.tag == "sandboxroot"))
    {
        Box::new(Tree {
            root: PathBuf::from(&fs.source_path),
        })
    } else {
        Box::new(Empty)
    };
    let _ = ROOTFS.set(reader);
}

/// This launcher's rootfs.
pub fn get() -> &'static dyn RootfsReader {
    static EMPTY: Empty = Empty;
    ROOTFS.get().map(|b| b.as_ref()).unwrap_or(&EMPTY)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_debugfs_stat() {
        assert_eq!(
            parse_stat("Inode: 12   Type: regular    Mode:  0644"),
            Entry::File
        );
        assert_eq!(
            parse_stat("Inode: 2   Type: directory    Mode:  0755"),
            Entry::Dir
        );
        assert_eq!(
            parse_stat("Inode: 172   Type: symlink    Mode:  0777\nFast link dest: \"certs/ca-certificates.crt\"\n"),
            Entry::Symlink("certs/ca-certificates.crt".into())
        );
        assert_eq!(
            parse_stat("Inode: 9   Type: symlink    Mode:  0777\n"),
            Entry::Missing
        );
        assert_eq!(
            parse_stat("stat: File not found by ext2_lookup"),
            Entry::Missing
        );
    }

    #[test]
    fn refuses_debugfs_syntax() {
        assert!(plain("/etc/ssl/certs/ca-certificates.crt"));
        assert!(!plain("/etc/x\" ; write"));
        assert!(!plain("/etc/a b"));
    }

    #[test]
    fn tree_resolves_inside_the_root() {
        let d = tempfile::tempdir().unwrap();
        let r = d.path();
        std::fs::create_dir_all(r.join("etc/pki/tls/certs")).unwrap();
        std::fs::create_dir_all(r.join("etc/ssl")).unwrap();
        std::fs::write(r.join("etc/pki/tls/certs/ca-bundle.crt"), b"ROOTS").unwrap();
        // Absolute directory symlink, as on Fedora: /etc/ssl/certs -> /etc/pki/tls/certs.
        std::os::unix::fs::symlink("/etc/pki/tls/certs", r.join("etc/ssl/certs")).unwrap();
        // A link that tries to leave the rootfs stays inside it.
        std::os::unix::fs::symlink("../../../../../../etc/hostname", r.join("etc/ssl/escape"))
            .unwrap();
        let t = Tree {
            root: r.to_path_buf(),
        };
        assert_eq!(
            t.resolve("/etc/ssl/certs/ca-bundle.crt").as_deref(),
            Some("/etc/pki/tls/certs/ca-bundle.crt")
        );
        assert_eq!(
            t.read("/etc/pki/tls/certs/ca-bundle.crt").unwrap(),
            b"ROOTS"
        );
        assert_eq!(t.resolve("/etc/ssl/escape"), None);
        assert_eq!(t.resolve("/etc/ssl/certs/missing.crt"), None);
        let dest = d.path().join("copy");
        std::os::unix::fs::symlink("ca-bundle.crt", r.join("etc/pki/tls/certs/abcd.0")).unwrap();
        t.copy_dir("/etc/pki/tls/certs", &dest).unwrap();
        assert_eq!(
            std::fs::read_link(dest.join("abcd.0")).unwrap(),
            PathBuf::from("ca-bundle.crt")
        );
        assert!(t.copy_dir("/etc/pki", &d.path().join("copy2")).is_err());
    }

    #[test]
    fn ext4_reads_through_debugfs() {
        // Needs mke2fs and debugfs (e2fsprogs); skipped where absent.
        if Command::new("mke2fs").arg("-V").output().is_err()
            || Command::new("debugfs").arg("-V").output().is_err()
        {
            return;
        }
        let d = tempfile::tempdir().unwrap();
        let root = d.path().join("root");
        std::fs::create_dir_all(root.join("etc/ssl/certs")).unwrap();
        std::fs::write(root.join("etc/ssl/certs/ca-certificates.crt"), b"ROOTS\n").unwrap();
        std::os::unix::fs::symlink("certs/ca-certificates.crt", root.join("etc/ssl/cert.pem"))
            .unwrap();
        let img = d.path().join("img.ext4");
        let ok = Command::new("mke2fs")
            .args(["-q", "-t", "ext4", "-d"])
            .arg(&root)
            .arg(&img)
            .arg("8M")
            .status()
            .map(|s| s.success())
            .unwrap_or(false);
        if !ok {
            return;
        }
        let e = Ext4 { image: img };
        assert_eq!(
            e.resolve("/etc/ssl/cert.pem").as_deref(),
            Some("/etc/ssl/certs/ca-certificates.crt")
        );
        assert_eq!(
            e.read("/etc/ssl/certs/ca-certificates.crt").unwrap(),
            b"ROOTS\n"
        );
        assert_eq!(e.resolve("/etc/pki/tls/certs/ca-bundle.crt"), None);
        let dest = d.path().join("copy");
        e.copy_dir("/etc/ssl/certs", &dest).unwrap();
        assert_eq!(
            std::fs::read(dest.join("ca-certificates.crt")).unwrap(),
            b"ROOTS\n"
        );
        // A directory with a subdirectory is refused.
        assert!(e.copy_dir("/etc/ssl", &d.path().join("copy2")).is_err());
    }
}

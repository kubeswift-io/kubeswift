//! Checkout-time artifacts for a warm-pool slot (bridge feature `warm-mounts`).
//!
//! A warm slot boots with an empty, read-only virtio-fs staging share. When a
//! sandbox with `spec.artifacts` checks the slot out, the controller lists each
//! artifact's digest in the exec action. Here each one is checked against the
//! node's artifact cache (present, sealed read-only by the materializer, and
//! verified with the sandbox's key when it has one) and bound read-only into
//! the staging directory under the artifact's name. The guest agent then binds
//! those directories at their paths in the sandbox root.
//!
//! Nothing is fetched here: the launcher mounts the cache read-only and has no
//! registry credentials. A missing entry fails the dispatch with
//! `ArtifactMissing: <names>`, and the controller fetches it on this node with
//! the sandbox's own credentials and key, then dispatches again.
//!
//! Every input is validated before it becomes a path: digests are
//! `sha256:<64 hex>`, names are DNS labels, key fingerprints are 64 hex.

use std::path::{Path, PathBuf};

/// The node artifact cache, as the launcher mounts it (read-only).
pub const CACHE_DIR: &str = "/var/lib/kubeswift/sandbox-artifacts";

/// The staging share's virtio-fs tag (the controller's `warmStageTag`).
pub const STAGE_TAG: &str = "sbxstage";

/// The staging share's source in the launcher: an emptyDir the controller
/// mounts here on every warm slot (the controller's `warmStageDir`).
pub const STAGE_DIR: &str = "/var/lib/kubeswift/sandbox-stage";

/// The mode the materializer gives an entry's top directory last, after it is
/// complete: an entry without it is not ready for a checkout.
const SEALED_MODE: u32 = 0o555;

/// One artifact in a sandbox-exec action.
#[derive(Debug, Clone, Default, serde::Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct WarmArtifact {
    pub name: String,
    pub digest: String,
    /// `oci` (an OCI image layout) or `unpacked` (a tree).
    #[serde(default)]
    pub layout: String,
    pub mount_path: String,
    /// sha256 of the sandbox's cosign public key, when it has one: the entry
    /// must carry the materializer's verified marker for exactly this key.
    #[serde(default)]
    pub key_fingerprint: String,
}

fn is_hex64(s: &str) -> bool {
    s.len() == 64
        && s.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

fn valid_name(s: &str) -> bool {
    let b = s.as_bytes();
    !b.is_empty()
        && b.len() <= 40
        && b.iter()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || *c == b'-')
        && b[0] != b'-'
        && b[b.len() - 1] != b'-'
}

impl WarmArtifact {
    fn validate(&self) -> Result<(), String> {
        if !valid_name(&self.name) {
            return Err(format!("artifact name {:?} is not a DNS label", self.name));
        }
        match self.digest.strip_prefix("sha256:") {
            Some(hex) if is_hex64(hex) => {}
            _ => {
                return Err(format!(
                    "artifact {}: digest {:?} is not sha256",
                    self.name, self.digest
                ))
            }
        }
        if !matches!(self.layout.as_str(), "" | "oci" | "unpacked") {
            return Err(format!("artifact {}: layout {:?}", self.name, self.layout));
        }
        if !self.key_fingerprint.is_empty() && !is_hex64(&self.key_fingerprint) {
            return Err(format!(
                "artifact {}: key fingerprint is not sha256 hex",
                self.name
            ));
        }
        if !self.mount_path.starts_with('/') {
            return Err(format!(
                "artifact {}: mount path is not absolute",
                self.name
            ));
        }
        Ok(())
    }

    /// The cache entry, as the materializer names it (`CachePathFor`).
    fn entry(&self, cache: &Path) -> PathBuf {
        let name = self.digest.replace(':', "-");
        if self.layout == "unpacked" {
            cache.join(name)
        } else {
            cache.join(format!("{}.oci", name))
        }
    }

    /// The materializer's marker that this digest verified with this key.
    fn verified_marker(&self, cache: &Path) -> PathBuf {
        cache
            .join(".verified")
            .join(self.digest.replace(':', "-"))
            .join(&self.key_fingerprint)
    }

    /// Ready to project: present, sealed, and verified for its key.
    fn ready(&self, cache: &Path) -> bool {
        use std::os::unix::fs::PermissionsExt;
        let sealed = std::fs::metadata(self.entry(cache))
            .map(|m| m.is_dir() && m.permissions().mode() & 0o7777 == SEALED_MODE)
            .unwrap_or(false);
        sealed && (self.key_fingerprint.is_empty() || self.verified_marker(cache).is_file())
    }
}

/// How a projection is made; replaced in tests (mounting needs root).
pub trait Binder {
    fn bind_read_only(&self, source: &Path, target: &Path) -> Result<(), String>;
    fn is_mounted(&self, target: &Path) -> bool;
}

/// Bind mounts in the launcher's mount namespace, which virtiofsd shares.
pub struct MountBinder;

impl Binder for MountBinder {
    fn bind_read_only(&self, source: &Path, target: &Path) -> Result<(), String> {
        use std::ffi::CString;
        use std::os::unix::ffi::OsStrExt;
        let src = CString::new(source.as_os_str().as_bytes()).map_err(|e| e.to_string())?;
        let dst = CString::new(target.as_os_str().as_bytes()).map_err(|e| e.to_string())?;
        // SAFETY: valid NUL-terminated paths; null fstype and data are allowed
        // for a bind and a remount.
        let rc = unsafe {
            libc::mount(
                src.as_ptr(),
                dst.as_ptr(),
                std::ptr::null(),
                libc::MS_BIND,
                std::ptr::null(),
            )
        };
        if rc != 0 {
            return Err(format!(
                "bind {}: {}",
                target.display(),
                std::io::Error::last_os_error()
            ));
        }
        let flags =
            libc::MS_REMOUNT | libc::MS_BIND | libc::MS_RDONLY | libc::MS_NOSUID | libc::MS_NODEV;
        let rc = unsafe {
            libc::mount(
                std::ptr::null(),
                dst.as_ptr(),
                std::ptr::null(),
                flags,
                std::ptr::null(),
            )
        };
        if rc != 0 {
            let err = std::io::Error::last_os_error();
            unsafe { libc::umount2(dst.as_ptr(), libc::MNT_DETACH) };
            return Err(format!("remount {} read-only: {}", target.display(), err));
        }
        Ok(())
    }

    fn is_mounted(&self, target: &Path) -> bool {
        // A mount point sits on a different device than its parent, or is the
        // same directory as another mount (bind of the same filesystem): read
        // the mount table instead of guessing from st_dev.
        let t = target.to_string_lossy();
        std::fs::read_to_string("/proc/self/mountinfo")
            .map(|m| m.lines().any(|l| l.split(' ').nth(4) == Some(&t)))
            .unwrap_or(false)
    }
}

/// Projects `artifacts` into `stage` from `cache` and returns the guest mounts.
/// Checks every artifact first: if any is not ready, nothing is mounted and the
/// error is `ArtifactMissing: <names>`. Idempotent for a repeated dispatch.
pub fn project(
    artifacts: &[WarmArtifact],
    cache: &Path,
    stage: &Path,
    binder: &dyn Binder,
) -> Result<Vec<swift_vsock_client::ExecMount>, String> {
    for a in artifacts {
        a.validate()?;
    }
    let missing: Vec<&str> = artifacts
        .iter()
        .filter(|a| !a.ready(cache))
        .map(|a| a.name.as_str())
        .collect();
    if !missing.is_empty() {
        return Err(format!("ArtifactMissing: {}", missing.join(",")));
    }
    let mut mounts = Vec::with_capacity(artifacts.len());
    for a in artifacts {
        let target = stage.join(&a.name);
        if !binder.is_mounted(&target) {
            std::fs::create_dir_all(&target).map_err(|e| format!("stage {}: {}", a.name, e))?;
            binder.bind_read_only(&a.entry(cache), &target)?;
        }
        mounts.push(swift_vsock_client::ExecMount {
            name: a.name.clone(),
            path: a.mount_path.clone(),
        });
    }
    Ok(mounts)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::os::unix::fs::PermissionsExt;

    #[derive(Default)]
    struct Recorder(RefCell<Vec<(PathBuf, PathBuf)>>);
    impl Binder for Recorder {
        fn bind_read_only(&self, s: &Path, t: &Path) -> Result<(), String> {
            self.0.borrow_mut().push((s.to_path_buf(), t.to_path_buf()));
            Ok(())
        }
        fn is_mounted(&self, t: &Path) -> bool {
            self.0.borrow().iter().any(|(_, x)| x == t)
        }
    }

    const D1: &str = "sha256:1111111111111111111111111111111111111111111111111111111111111111";
    const D2: &str = "sha256:2222222222222222222222222222222222222222222222222222222222222222";
    const FP: &str = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff";

    fn tmp() -> PathBuf {
        let d = std::env::temp_dir().join(format!(
            "warm-art-{}-{:?}",
            std::process::id(),
            std::time::SystemTime::now()
        ));
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn entry(cache: &Path, name: &str, mode: u32) {
        let p = cache.join(name);
        std::fs::create_dir_all(&p).unwrap();
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(mode)).unwrap();
    }

    fn art(name: &str, digest: &str, layout: &str, fp: &str) -> WarmArtifact {
        WarmArtifact {
            name: name.into(),
            digest: digest.into(),
            layout: layout.into(),
            mount_path: format!("/run/{}", name),
            key_fingerprint: fp.into(),
        }
    }

    #[test]
    fn projects_sealed_entries_under_their_names() {
        let (cache, stage) = (tmp(), tmp());
        entry(&cache, &format!("{}.oci", D1.replace(':', "-")), 0o555);
        entry(&cache, &D2.replace(':', "-"), 0o555);
        let b = Recorder::default();
        let arts = [art("app", D1, "oci", ""), art("data", D2, "unpacked", "")];
        let m = project(&arts, &cache, &stage, &b).unwrap();
        assert_eq!(m.len(), 2);
        assert_eq!(m[0].name, "app");
        assert_eq!(m[0].path, "/run/app");
        let binds = b.0.borrow();
        assert_eq!(
            binds[0].0,
            cache.join(format!("{}.oci", D1.replace(':', "-")))
        );
        assert_eq!(binds[0].1, stage.join("app"));
        assert_eq!(binds[1].0, cache.join(D2.replace(':', "-")));
        // A repeated dispatch binds nothing twice.
        drop(binds);
        project(&arts, &cache, &stage, &b).unwrap();
        assert_eq!(b.0.borrow().len(), 2);
        // Leave the dirs removable.
        for d in [
            cache.join(format!("{}.oci", D1.replace(':', "-"))),
            cache.join(D2.replace(':', "-")),
        ] {
            std::fs::set_permissions(&d, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
    }

    #[test]
    fn missing_unsealed_or_unverified_mounts_nothing() {
        let (cache, stage) = (tmp(), tmp());
        // app: present and sealed but needs a key marker that is absent.
        entry(&cache, &format!("{}.oci", D1.replace(':', "-")), 0o555);
        // data: present but not sealed (an older release, or mid-publish).
        entry(&cache, &format!("{}.oci", D2.replace(':', "-")), 0o755);
        let b = Recorder::default();
        let arts = [
            art("app", D1, "oci", FP),
            art("data", D2, "oci", ""),
            art("gone", &D1.replace('1', "3"), "oci", ""),
        ];
        let err = project(&arts, &cache, &stage, &b).unwrap_err();
        assert_eq!(err, "ArtifactMissing: app,data,gone");
        assert!(b.0.borrow().is_empty());
        // With the marker for this key, app is ready.
        let marker = cache.join(".verified").join(D1.replace(':', "-"));
        std::fs::create_dir_all(&marker).unwrap();
        std::fs::write(marker.join(FP), b"").unwrap();
        project(&arts[..1], &cache, &stage, &b).unwrap();
        // A marker for another key does not count.
        let other = art("app", D1, "oci", &FP.replace('f', "e"));
        assert!(project(&[other], &cache, &stage, &Recorder::default()).is_err());
        std::fs::set_permissions(
            cache.join(format!("{}.oci", D1.replace(':', "-"))),
            std::fs::Permissions::from_mode(0o755),
        )
        .unwrap();
    }

    #[test]
    fn hostile_inputs_never_become_paths() {
        let (cache, stage) = (tmp(), tmp());
        let b = Recorder::default();
        for a in [
            art("../x", D1, "oci", ""),
            art("app", "sha256:../../etc", "oci", ""),
            art("app", "sha512:aa", "oci", ""),
            art("app", D1, "ext4", ""),
            art("app", D1, "oci", "../../x"),
            art("-app", D1, "oci", ""),
            WarmArtifact {
                mount_path: "relative".into(),
                ..art("app", D1, "oci", "")
            },
        ] {
            assert!(
                project(&[a.clone()], &cache, &stage, &b).is_err(),
                "accepted {:?}",
                a
            );
        }
        assert!(b.0.borrow().is_empty());
    }
}

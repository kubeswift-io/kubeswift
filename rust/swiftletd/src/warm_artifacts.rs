//! Checkout-time artifacts for a warm-pool slot (bridge feature `warm-mounts`).
//!
//! A warm slot boots with an empty, read-only virtio-fs staging share. When a
//! sandbox with `spec.artifacts` checks the slot out, the controller lists each
//! artifact's digest in the exec action. Here each one is checked against the
//! node's artifact cache: sealed by the materializer, its content read from
//! the sandbox's own repository (an origin marker: a copy of the manifest
//! hosted elsewhere does not unlock it), and verified with the sandbox's key
//! when it has one. Ready, it is bound read-only into the staging directory
//! under the artifact's name. The guest agent then binds
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
    /// sha256 of the repository the sandbox resolved the artifact from: the
    /// entry's content must have been read from exactly that repository.
    #[serde(default)]
    pub repository_fingerprint: String,
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

    /// The materializer's marker that this digest's content was read from
    /// this repository.
    fn origin_marker(&self, cache: &Path) -> PathBuf {
        cache
            .join(".origin")
            .join(self.digest.replace(':', "-"))
            .join(&self.repository_fingerprint)
    }

    /// The materializer's marker that the entry is complete.
    fn sealed_marker(&self, cache: &Path) -> PathBuf {
        let entry = self.entry(cache);
        cache
            .join(".sealed")
            .join(entry.file_name().unwrap_or_default())
    }

    /// Ready to project: sealed, from this repository, and verified for its key.
    fn ready(&self, cache: &Path) -> bool {
        self.entry(cache).is_dir()
            && self.sealed_marker(cache).is_file()
            && self.origin_marker(cache).is_file()
            && (self.key_fingerprint.is_empty() || self.verified_marker(cache).is_file())
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
    const REPO: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

    fn tmp() -> PathBuf {
        let d = std::env::temp_dir().join(format!(
            "warm-art-{}-{:?}",
            std::process::id(),
            std::time::SystemTime::now()
        ));
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn touch(p: PathBuf) {
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(p, b"").unwrap();
    }

    fn art(name: &str, digest: &str, layout: &str, fp: &str) -> WarmArtifact {
        WarmArtifact {
            name: name.into(),
            digest: digest.into(),
            layout: layout.into(),
            mount_path: format!("/run/{}", name),
            key_fingerprint: fp.into(),
            repository_fingerprint: REPO.into(),
        }
    }

    /// What the materializer leaves for a complete entry read from REPO.
    fn cached(cache: &Path, a: &WarmArtifact) {
        std::fs::create_dir_all(a.entry(cache)).unwrap();
        touch(a.sealed_marker(cache));
        touch(a.origin_marker(cache));
    }

    #[test]
    fn projects_ready_entries_under_their_names() {
        let (cache, stage) = (tmp(), tmp());
        let arts = [art("app", D1, "oci", ""), art("data", D2, "unpacked", "")];
        arts.iter().for_each(|a| cached(&cache, a));
        let b = Recorder::default();
        let m = project(&arts, &cache, &stage, &b).unwrap();
        assert_eq!(m.len(), 2);
        assert_eq!(m[0].name, "app");
        assert_eq!(m[0].path, "/run/app");
        {
            let binds = b.0.borrow();
            assert_eq!(
                binds[0].0,
                cache.join(format!("{}.oci", D1.replace(':', "-")))
            );
            assert_eq!(binds[0].1, stage.join("app"));
            assert_eq!(binds[1].0, cache.join(D2.replace(':', "-")));
        }
        // A repeated dispatch binds nothing twice.
        project(&arts, &cache, &stage, &b).unwrap();
        assert_eq!(b.0.borrow().len(), 2);
    }

    #[test]
    fn anything_not_ready_mounts_nothing() {
        let cache = tmp();
        let stage = tmp();
        let ok = art("ok", D2, "oci", "");
        cached(&cache, &ok);
        let cases: Vec<(&str, WarmArtifact, Box<dyn Fn(&Path, &WarmArtifact)>)> = vec![
            (
                "absent",
                art("app", &D1.replace('1', "3"), "oci", ""),
                Box::new(|_, _| {}),
            ),
            (
                "not sealed",
                art("app", D1, "oci", ""),
                Box::new(|c, a| {
                    std::fs::create_dir_all(a.entry(c)).unwrap();
                    touch(a.origin_marker(c));
                }),
            ),
            (
                "from another repository",
                art("app", &D1.replace('1', "5"), "unpacked", ""),
                Box::new(|c, a| {
                    std::fs::create_dir_all(a.entry(c)).unwrap();
                    touch(a.sealed_marker(c));
                    let mut other = a.clone();
                    other.repository_fingerprint = REPO.replace('a', "b");
                    touch(other.origin_marker(c));
                }),
            ),
            (
                "unverified for this key",
                art("app", &D1.replace('1', "4"), "oci", FP),
                Box::new(|c, a| {
                    cached(c, a);
                    let mut other = a.clone();
                    other.key_fingerprint = FP.replace('f', "e");
                    touch(other.verified_marker(c));
                }),
            ),
        ];
        for (what, a, setup) in cases {
            setup(&cache, &a);
            let b = Recorder::default();
            let err = project(&[ok.clone(), a.clone()], &cache, &stage, &b).unwrap_err();
            assert_eq!(err, "ArtifactMissing: app", "{}", what);
            assert!(b.0.borrow().is_empty(), "{}: mounted before refusing", what);
        }
        // With the marker for its key, the signed one is ready.
        let signed = art("app", &D1.replace('1', "4"), "oci", FP);
        touch(signed.verified_marker(&cache));
        project(&[signed], &cache, &stage, &Recorder::default()).unwrap();
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
                repository_fingerprint: "../../x".into(),
                ..art("app", D1, "oci", "")
            },
            WarmArtifact {
                repository_fingerprint: String::new(),
                ..art("app", D1, "oci", "")
            },
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

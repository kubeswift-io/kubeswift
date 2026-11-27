//! Sandbox suspend (SwiftSandbox `spec.operatingMode: Suspended`).
//!
//! The controller sends the `suspend` verb on the snapshot action namespace
//! with a destination under [`SUSPEND_ROOT`]. This pauses the microVM, has
//! Cloud Hypervisor write its state there, and records what a later restore
//! needs that the state does not hold ([`SuspendState`], also returned to the
//! controller as the status detail). The microVM stays paused: the launcher
//! marks itself suspended, and the SIGTERM that follows when the controller
//! deletes the pod stops the hypervisor at once, with no drain
//! (shutdown.rs). A failure resumes the microVM, so the sandbox keeps running.

use std::io::Write;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::Path;

use crate::action::{
    dest_dir_under, probe_ch_version, ActionOutcome, PendingAction, DEFAULT_ACTION_TIMEOUT_SECS,
};

/// Node directory every sandbox suspend lands under (one directory per
/// sandbox, named by its UID). Root-only: it holds guest memory.
pub const SUSPEND_ROOT: &str = "/var/lib/kubeswift/sandbox-suspend/";

/// What the launcher records next to Cloud Hypervisor's files.
pub const STATE_FILE: &str = "kubeswift.json";

#[cfg(not(test))]
fn suspend_root() -> String {
    SUSPEND_ROOT.to_string()
}

#[cfg(test)]
thread_local! {
    static TEST_SUSPEND_ROOT: std::cell::RefCell<Option<String>> =
        const { std::cell::RefCell::new(None) };
}

#[cfg(test)]
fn suspend_root() -> String {
    TEST_SUSPEND_ROOT
        .with(|r| r.borrow().clone())
        .unwrap_or_else(|| SUSPEND_ROOT.to_string())
}

#[derive(Debug, serde::Deserialize)]
struct SuspendArgs {
    /// `file://` + [`SUSPEND_ROOT`] + one segment.
    destination_url: String,
    #[serde(default)]
    timeout_seconds: Option<u64>,
}

/// What a restore needs that Cloud Hypervisor's state does not carry.
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SuspendState {
    pub version: u32,
    /// The guest's gateway MAC (br0); empty without a NIC.
    pub bridge_mac: String,
    /// The guest's address from the launcher's lease; empty without one.
    pub guest_ip: String,
    /// The intent the launcher ran (KUBESWIFT_INTENT_SHA256).
    pub intent_sha256: String,
    /// Size of the config disk the guest read at boot; a restore recreates a
    /// placeholder of this size at the same path.
    pub config_disk_bytes: u64,
    pub hypervisor_version: String,
    pub taken_at_unix: i64,
    /// Bytes on disk of the whole directory (memory-ranges is sparse).
    pub size_bytes: u64,
    /// The read-only rootfs image the guest has open: a restore refuses a
    /// file that is not this one (the node cache rebuilt or replaced it).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rootfs: Option<FileId>,
}

/// Identity of a file on the node.
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FileId {
    pub path: String,
    pub size: u64,
    pub mtime_ns: i64,
    pub inode: u64,
}

impl FileId {
    fn of(path: &str) -> std::io::Result<Self> {
        let m = std::fs::metadata(path)?;
        Ok(FileId {
            path: path.to_string(),
            size: m.len(),
            mtime_ns: m.mtime() * 1_000_000_000 + m.mtime_nsec(),
            inode: m.ino(),
        })
    }
}

/// The node's rootfs image cache (sandbox-materialize --cache-dir).
const ROOTFS_CACHE: &str = "/var/lib/kubeswift/sandbox-rootfs/";

/// The block rootfs in a sandbox's Cloud Hypervisor config: the disk from
/// the node's rootfs cache. None for a virtio-fs rootfs, whose only disk is
/// the config disk.
fn rootfs_path(info: &serde_json::Value) -> Option<String> {
    info.pointer("/config/disks")?
        .as_array()?
        .iter()
        .filter_map(|d| d.get("path").and_then(|v| v.as_str()))
        .find(|p| p.starts_with(ROOTFS_CACHE))
        .map(str::to_string)
}

/// Handle the `suspend` verb.
pub fn dispatch(action: &PendingAction, api_socket: &Path) -> Result<ActionOutcome, String> {
    let args: SuspendArgs = serde_json::from_value(action.args.clone())
        .map_err(|e| format!("parse suspend args: {}", e))?;
    // From a pod annotation: checked here, as for a capture (action.rs).
    let dir = dest_dir_under(&suspend_root(), &args.destination_url)?.to_string();
    let runtime_dir = api_socket
        .parent()
        .ok_or_else(|| "api socket has no runtime directory".to_string())?
        .to_path_buf();
    log::info!("dispatch_sandbox_suspend id={} dest={}", action.id, dir);

    let timeout =
        std::time::Duration::from_secs(args.timeout_seconds.unwrap_or(DEFAULT_ACTION_TIMEOUT_SECS));
    let client = swift_ch_client::ApiClient::new(api_socket).with_timeout(timeout);
    let info = client
        .vm_info_value()
        .map_err(|e| format!("vm.info before suspend: {}", e))?;
    if let Some(reason) =
        crate::vfio_guard::snapshot_refusal(&info, crate::vfio_guard::sysfs_driver)
    {
        log::warn!("suspend_refused_vfio id={}: {}", action.id, reason);
        return Err(reason);
    }
    let version = probe_ch_version(&client)
        .map(|v| v.build_version)
        .unwrap_or_default();

    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).map_err(|e| format!("create {}: {}", dir, e))?;
    let _ = std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700));

    client.pause().map_err(|e| format!("pause: {}", e))?;
    let paused = std::time::Instant::now();
    let fail = |msg: String| -> Result<ActionOutcome, String> {
        log::warn!(
            "suspend_failed id={}: {}; resuming the microVM",
            action.id,
            msg
        );
        if let Err(e) = client.resume() {
            log::error!(
                "resume_after_suspend_failure_failed id={}: {}",
                action.id,
                e
            );
        }
        let _ = std::fs::remove_dir_all(&dir);
        Err(msg)
    };
    if let Err(e) = client.snapshot(&args.destination_url) {
        return fail(format!("snapshot: {}", e));
    }
    let mut state = SuspendState {
        version: 1,
        bridge_mac: read_trimmed("/sys/class/net/br0/address"),
        guest_ip: lease_ip(&runtime_dir.join("dnsmasq.leases")),
        intent_sha256: std::env::var("KUBESWIFT_INTENT_SHA256").unwrap_or_default(),
        config_disk_bytes: std::fs::metadata(runtime_dir.join("exec.config"))
            .map(|m| m.len())
            .unwrap_or(0),
        hypervisor_version: version,
        taken_at_unix: std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or(0),
        size_bytes: 0,
        rootfs: rootfs_path(&info).and_then(|p| FileId::of(&p).ok()),
    };
    if let Err(e) = write_state(Path::new(&dir), &mut state) {
        return fail(format!("record state: {}", e));
    }
    let pause_ms = paused.elapsed().as_millis().min(u64::MAX as u128) as u64;
    // From here SIGTERM stops the paused microVM at once.
    crate::shutdown::set_suspended();
    log::info!(
        "sandbox_suspended id={} dir={} size_bytes={} pause_ms={}",
        action.id,
        dir,
        state.size_bytes,
        pause_ms
    );
    let detail = serde_json::to_string(&state).map_err(|e| format!("encode state: {}", e))?;
    Ok(ActionOutcome {
        detail: Some(detail),
        pause_window_ms: Some(pause_ms),
        success_status: None,
        suppress_status: false,
    })
}

// ─── Resume ──────────────────────────────────────────────────────────────────

/// Reads the state a suspend recorded and checks the node still has what the
/// guest expects: the same rootfs file, and the guest's gateway MAC on br0
/// (set when it differs). The error names what is wrong; the restore must
/// not go on.
pub fn prepare_restore(snapshot_dir: &str, runtime_dir: &Path) -> Result<SuspendState, String> {
    let path = Path::new(snapshot_dir).join(STATE_FILE);
    let raw = std::fs::read(&path).map_err(|e| format!("read {}: {}", path.display(), e))?;
    let state: SuspendState =
        serde_json::from_slice(&raw).map_err(|e| format!("parse {}: {}", path.display(), e))?;
    if let Some(want) = &state.rootfs {
        let have =
            FileId::of(&want.path).map_err(|e| format!("rootfs {} is gone: {}", want.path, e))?;
        if &have != want {
            return Err(format!(
                "rootfs {} is not the file the guest was suspended with (rebuilt or replaced)",
                want.path
            ));
        }
    }
    if !state.bridge_mac.is_empty() {
        let now = read_trimmed("/sys/class/net/br0/address");
        if !now.is_empty() && now != state.bridge_mac {
            log::info!(
                "restore: br0 {} -> {} (the guest's gateway MAC)",
                now,
                state.bridge_mac
            );
            let out = std::process::Command::new("ip")
                .args(["link", "set", "br0", "address", &state.bridge_mac])
                .output()
                .map_err(|e| format!("ip link set br0: {}", e))?;
            if !out.status.success() {
                return Err(format!(
                    "set br0 address {}: {}",
                    state.bridge_mac,
                    String::from_utf8_lossy(&out.stderr).trim()
                ));
            }
        }
    }
    if !state.guest_ip.is_empty() {
        seed_lease(
            snapshot_dir,
            &runtime_dir.join("dnsmasq.leases"),
            &state.guest_ip,
        )?;
    }
    Ok(state)
}

/// Gives the launcher's lease file the restored guest's address. The guest
/// keeps the address it had (the kernel's DHCP client never renews), so no
/// lease would ever appear, and the lease poller, the probes and
/// status.network all read the guest's address from this file. dnsmasq only
/// rewrites it on DHCP traffic, which a restored guest does not send.
fn seed_lease(snapshot_dir: &str, leases: &Path, ip: &str) -> Result<(), String> {
    let cfg = Path::new(snapshot_dir).join("config.json");
    let mac = std::fs::read(&cfg)
        .ok()
        .and_then(|b| serde_json::from_slice::<serde_json::Value>(&b).ok())
        .and_then(|v| {
            v.pointer("/net/0/mac")
                .and_then(|m| m.as_str())
                .map(str::to_string)
        })
        .unwrap_or_default();
    if mac.is_empty() {
        return Ok(()); // no NIC
    }
    let expiry = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() + 3600)
        .unwrap_or(0);
    let mut f = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(leases)
        .map_err(|e| format!("open {}: {}", leases.display(), e))?;
    writeln!(f, "{} {} {} * *", expiry, mac, ip)
        .map_err(|e| format!("write {}: {}", leases.display(), e))
}

/// A placeholder for the config disk the guest read at boot: zeros of the
/// recorded size. The guest does not read it again, so no Secret is read on
/// resume.
pub fn write_placeholder_config(path: &Path, size: u64) -> std::io::Result<()> {
    let f = std::fs::File::create(path)?;
    f.set_len(size)
}

/// How long the restored guest's agent has to answer resume-sync.
const RESUME_SYNC_WINDOW: std::time::Duration = std::time::Duration::from_secs(60);

/// After a restore: sets the guest's clock and adds host entropy through its
/// agent (resume-sync), reports the result on the pod, and removes the
/// snapshot, so it is restored only once.
pub fn spawn_resume_sync(vsock: std::path::PathBuf, snapshot_dir: String) {
    let spawned = std::thread::Builder::new()
        .name("resume-sync".into())
        .spawn(move || {
            let until = std::time::Instant::now() + RESUME_SYNC_WINDOW;
            let mut last_err = String::new();
            let result = loop {
                let mut entropy = [0u8; 64];
                if let Err(e) = getrandom(&mut entropy) {
                    log::warn!("resume_sync: no host entropy: {}", e);
                }
                let now_ns = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map(|d| d.as_nanos() as i64)
                    .unwrap_or(0);
                match swift_vsock_client::resume_sync(
                    &vsock,
                    now_ns,
                    &entropy,
                    std::time::Duration::from_secs(5),
                ) {
                    Ok(r) => break r,
                    Err(e) => last_err = e.to_string(),
                }
                if std::time::Instant::now() >= until {
                    break swift_vsock_client::ResumeSyncResponse {
                        ok: false,
                        error: Some(format!("the guest agent did not answer: {}", last_err)),
                        ..Default::default()
                    };
                }
                std::thread::sleep(std::time::Duration::from_millis(100));
            };
            log::info!(
                "resume_sync ok={} synced={:?} clock_offset_ms={:?} error={:?}",
                result.ok,
                result.synced,
                result.clock_offset_ms,
                result.error
            );
            // Cloud Hypervisor restored the guest in copy mode: its memory is
            // in the hypervisor now, so the saved state is done with, synced
            // or not (the controller drops its cleanup finalizer either way).
            if let Err(e) = std::fs::remove_dir_all(&snapshot_dir) {
                log::warn!("resume_sync: remove {}: {}", snapshot_dir, e);
            }
            let json = serde_json::to_string(&result).unwrap_or_default();
            if let (Ok(ns), Ok(n)) = (std::env::var("POD_NAMESPACE"), std::env::var("POD_NAME")) {
                if let Ok(rt) = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                {
                    if let Err(e) = rt.block_on(crate::report::report_resume_sync(&ns, &n, &json)) {
                        log::error!("report_resume_sync_failed: {}", e);
                    }
                }
            }
        });
    if let Err(e) = spawned {
        log::error!("resume_sync: thread spawn failed: {}", e);
    }
}

fn getrandom(buf: &mut [u8]) -> std::io::Result<()> {
    use std::io::Read;
    std::fs::File::open("/dev/urandom")?.read_exact(buf)
}

/// Writes [`STATE_FILE`] with the directory's size, then flushes every file
/// and the directory to disk: the launcher is stopped right after.
fn write_state(dir: &Path, state: &mut SuspendState) -> std::io::Result<()> {
    state.size_bytes = dir_size(dir)?;
    let json = serde_json::to_vec_pretty(state).map_err(std::io::Error::other)?;
    let mut f = std::fs::OpenOptions::new()
        .create(true)
        .truncate(true)
        .write(true)
        .mode_0600()
        .open(dir.join(STATE_FILE))?;
    f.write_all(&json)?;
    for entry in std::fs::read_dir(dir)? {
        let p = entry?.path();
        if p.is_file() {
            std::fs::File::open(&p)?.sync_all()?;
        }
    }
    std::fs::File::open(dir)?.sync_all()
}

trait Mode0600 {
    fn mode_0600(&mut self) -> &mut Self;
}

impl Mode0600 for std::fs::OpenOptions {
    fn mode_0600(&mut self) -> &mut Self {
        use std::os::unix::fs::OpenOptionsExt;
        self.mode(0o600)
    }
}

/// Bytes allocated on disk by the files directly in `dir`.
fn dir_size(dir: &Path) -> std::io::Result<u64> {
    let mut total = 0;
    for entry in std::fs::read_dir(dir)? {
        let m = entry?.metadata()?;
        if m.is_file() {
            total += m.blocks() * 512;
        }
    }
    Ok(total)
}

fn read_trimmed(path: &str) -> String {
    std::fs::read_to_string(path)
        .map(|s| s.trim().to_string())
        .unwrap_or_default()
}

/// The address in the launcher's dnsmasq lease file (`expiry mac ip ...`).
fn lease_ip(path: &Path) -> String {
    std::fs::read_to_string(path)
        .ok()
        .and_then(|s| {
            s.lines()
                .next()
                .and_then(|l| l.split_whitespace().nth(2).map(str::to_string))
        })
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn action(url: &str) -> PendingAction {
        PendingAction {
            kind: crate::action::ActionKind::SandboxSuspend,
            id: "s-1".into(),
            args: serde_json::json!({ "destination_url": url }),
        }
    }

    #[test]
    fn refuses_a_destination_outside_the_suspend_root() {
        let root = tempfile::tempdir().unwrap();
        let r = format!("{}/", root.path().display());
        TEST_SUSPEND_ROOT.with(|t| *t.borrow_mut() = Some(r.clone()));
        let sock = root.path().join("run/ch.sock");
        for bad in [
            format!("file://{}", r),
            format!("file://{}a/b/", r),
            format!("file://{}../x/", r),
            "file:///var/lib/kubeswift/snapshots/x/".to_string(),
            format!("http://{}x/", r),
        ] {
            let err = dispatch(&action(&bad), &sock).unwrap_err();
            assert!(err.contains("destination_url"), "{bad}: {err}");
        }
        TEST_SUSPEND_ROOT.with(|t| *t.borrow_mut() = None);
    }

    #[test]
    fn reads_the_lease_ip() {
        let d = tempfile::tempdir().unwrap();
        let p = d.path().join("dnsmasq.leases");
        std::fs::write(&p, "1795739129 2e:f2:7c:58:06:21 192.168.99.19 * *\n").unwrap();
        assert_eq!(lease_ip(&p), "192.168.99.19");
        assert_eq!(lease_ip(&d.path().join("missing")), "");
    }

    #[test]
    fn writes_the_state_file_with_the_size() {
        let d = tempfile::tempdir().unwrap();
        std::fs::write(d.path().join("state.json"), vec![1u8; 8192]).unwrap();
        let mut st = SuspendState {
            version: 1,
            bridge_mac: "52:54:00:aa:bb:cc".into(),
            guest_ip: "192.168.99.10".into(),
            intent_sha256: "abc".into(),
            config_disk_bytes: 1048576,
            hypervisor_version: "v54.0".into(),
            taken_at_unix: 1,
            size_bytes: 0,
            rootfs: None,
        };
        write_state(d.path(), &mut st).unwrap();
        assert!(st.size_bytes >= 8192, "size {}", st.size_bytes);
        let back: SuspendState =
            serde_json::from_slice(&std::fs::read(d.path().join(STATE_FILE)).unwrap()).unwrap();
        assert_eq!(back, st);
        let mode = std::fs::metadata(d.path().join(STATE_FILE))
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o600);
    }

    #[test]
    fn finds_the_block_rootfs_in_the_vm_config() {
        let block = serde_json::json!({"config": {"disks": [
            {"path": "/var/lib/kubeswift/sandbox-rootfs/sha256-ab.ext4", "readonly": true},
            {"path": "/run/exec.config", "readonly": true}]}});
        assert_eq!(
            rootfs_path(&block).as_deref(),
            Some("/var/lib/kubeswift/sandbox-rootfs/sha256-ab.ext4")
        );
        // A virtio-fs rootfs: the config disk comes first and is read-only too.
        let virtiofs = serde_json::json!({"config": {"disks": [{"path": "/run/exec.config", "readonly": true}]}});
        assert_eq!(rootfs_path(&virtiofs), None);
        assert_eq!(rootfs_path(&serde_json::json!({})), None);
    }

    fn state_with(rootfs: Option<FileId>) -> SuspendState {
        SuspendState {
            version: 1,
            bridge_mac: String::new(),
            guest_ip: String::new(),
            intent_sha256: String::new(),
            config_disk_bytes: 4096,
            hypervisor_version: String::new(),
            taken_at_unix: 0,
            size_bytes: 0,
            rootfs,
        }
    }

    #[test]
    fn restore_refuses_a_replaced_rootfs() {
        let d = tempfile::tempdir().unwrap();
        let img = d.path().join("root.ext4");
        std::fs::write(&img, b"one").unwrap();
        let id = FileId::of(img.to_str().unwrap()).unwrap();
        let snap = d.path().join("snap");
        std::fs::create_dir(&snap).unwrap();
        std::fs::write(
            snap.join(STATE_FILE),
            serde_json::to_vec(&state_with(Some(id))).unwrap(),
        )
        .unwrap();
        assert!(prepare_restore(snap.to_str().unwrap(), d.path()).is_ok());
        // Rebuilt: a new file at the same path.
        std::fs::remove_file(&img).unwrap();
        std::fs::write(&img, b"rebuilt").unwrap();
        let err = prepare_restore(snap.to_str().unwrap(), d.path()).unwrap_err();
        assert!(err.contains("not the file"), "{err}");
        std::fs::remove_file(&img).unwrap();
        let err = prepare_restore(snap.to_str().unwrap(), d.path()).unwrap_err();
        assert!(err.contains("is gone"), "{err}");
    }

    #[test]
    fn restore_needs_the_state_file() {
        let d = tempfile::tempdir().unwrap();
        let err = prepare_restore(d.path().to_str().unwrap(), d.path()).unwrap_err();
        assert!(err.contains(STATE_FILE), "{err}");
    }

    #[test]
    fn placeholder_config_has_the_recorded_size() {
        let d = tempfile::tempdir().unwrap();
        let p = d.path().join("exec.config");
        std::fs::write(&p, b"secret=value").unwrap();
        write_placeholder_config(&p, 1048576).unwrap();
        let data = std::fs::read(&p).unwrap();
        assert_eq!(data.len(), 1048576);
        assert!(
            data.iter().all(|b| *b == 0),
            "old content left in the placeholder"
        );
    }

    #[test]
    fn seeds_the_lease_of_the_restored_guest() {
        let d = tempfile::tempdir().unwrap();
        std::fs::write(
            d.path().join("config.json"),
            r#"{"net":[{"mac":"2e:fe:48:8b:2f:cf"}]}"#,
        )
        .unwrap();
        let leases = d.path().join("dnsmasq.leases");
        seed_lease(d.path().to_str().unwrap(), &leases, "192.168.99.12").unwrap();
        let text = std::fs::read_to_string(&leases).unwrap();
        assert_eq!(
            crate::lease::parse_first_lease(&text).as_deref(),
            Some("192.168.99.12")
        );
        assert!(text.contains(" 2e:fe:48:8b:2f:cf 192.168.99.12 "), "{text}");
        // No NIC in the config: nothing written.
        let e = tempfile::tempdir().unwrap();
        std::fs::write(e.path().join("config.json"), r#"{"net":null}"#).unwrap();
        seed_lease(
            e.path().to_str().unwrap(),
            &e.path().join("l"),
            "192.168.99.12",
        )
        .unwrap();
        assert!(!e.path().join("l").exists());
    }
}

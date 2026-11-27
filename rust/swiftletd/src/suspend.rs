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
}

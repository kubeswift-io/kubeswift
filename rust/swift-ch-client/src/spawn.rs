//! Spawn Cloud Hypervisor and connect to socket.

use std::path::Path;
use std::process::Child;
use std::time::Duration;

use crate::config::DEFAULT_CH_BINARY;
use crate::VmConfig;

/// Pre-flight: remove any stale API socket file before spawning CH.
///
/// CH does NOT clean up its `--api-socket` file on SIGKILL exit (a
/// Linux process cannot run cleanup hooks under SIGKILL). If a prior
/// CH instance was killed (e.g., the dst-kill cancel primitive used
/// during live migration), the socket file persists. The next CH
/// invocation fails with `Address in use` and exits immediately —
/// silent failure with confusing downstream symptoms.
///
/// This is the W2 walkthrough finding from
/// `docs/design/live-migration-phase-2-spike.md` — the most-replicated
/// failure mode in the spike (recurred in Q1, Q1d, Q1e, Q2, Q4, and the
/// walkthrough run #1). The cleanup is co-located with each spawn call
/// site so future spawn variants inherit the protection automatically.
///
/// The `let _ =` is intentional: a missing socket is the normal startup
/// case; a stale socket is the post-SIGKILL case. Both lead to the
/// same desired post-condition (no file exists at the path), so any
/// error from `remove_file` is non-actionable.
fn rm_stale_api_socket(api_socket: &Path) {
    let _ = std::fs::remove_file(api_socket);
}

/// Spawns the Cloud Hypervisor process with the given config.
/// Returns the child process. The API socket will be created by CH when ready.
pub fn spawn_ch(config: &VmConfig) -> Result<Child, std::io::Error> {
    let binary =
        std::env::var("KUBESWIFT_CH_BINARY").unwrap_or_else(|_| DEFAULT_CH_BINARY.to_string());

    rm_stale_api_socket(Path::new(config.api_socket()));

    let args = config.to_args();

    std::process::Command::new(&binary).args(&args).spawn()
}

/// Spawn Cloud Hypervisor in restore-receive mode.
///
/// Unlike [`spawn_ch`], `spawn_ch_restore` does NOT pass disks, networks,
/// kernel, memory, or cpu arguments — CH reads all of those from the
/// snapshot's `config.json`. The host-side resources the snapshot
/// references (tap devices, hostPath disk files, etc.) MUST already
/// exist in the new pod's network/mount namespace before this call;
/// otherwise CH will fail to wire the restored devices and exit
/// non-zero shortly after start.
///
/// Arguments:
///   - `api_socket`: Unix socket where CH will listen for the REST API.
///     The caller can then drive resume/shutdown through [`crate::ApiClient`].
///   - `source_url`: snapshot location, e.g.
///     `file:///var/lib/kubeswift/snapshots/<ns>-<name>/`. CH reads
///     `config.json`, `state.json`, and `memory-ranges` from this dir.
///
/// CH brings the VM up in **Paused** state — the action handler must
/// issue a [`crate::ApiClient::resume`] to start CPUs.
pub fn spawn_ch_restore(api_socket: &Path, source_url: &str) -> Result<Child, std::io::Error> {
    let binary =
        std::env::var("KUBESWIFT_CH_BINARY").unwrap_or_else(|_| DEFAULT_CH_BINARY.to_string());
    rm_stale_api_socket(api_socket);
    let args = restore_args(api_socket, source_url);
    std::process::Command::new(&binary).args(&args).spawn()
}

/// Spawn Cloud Hypervisor in receive-migration mode (live-migration Phase 2).
///
/// Unlike [`spawn_ch`] and [`spawn_ch_restore`], `spawn_ch_receive` passes
/// **only** `--api-socket=<path>`. CH starts as an empty VMM and waits for
/// the action handler to issue `vm.receive-migration` over the API socket.
/// All VM configuration (CPUs, memory, payload, network, disks) arrives
/// over the migration wire from the source CH; nothing on this side
/// describes the guest shape.
///
/// Host-side resources referenced by the migrated config (tap interfaces,
/// PVC mount points, etc.) MUST already exist in the destination pod's
/// network/mount namespace before this call. CH attaches them at
/// receive-migration completion; a missing tap or unattached PVC manifests
/// as a `receive-migration` failure on the API call, NOT here at spawn.
///
/// The receive listener is opened only when the action handler invokes
/// [`crate::ApiClient::receive_migration`]; spawning CH alone does not
/// bind the migration TCP listener.
///
/// See `docs/design/live-migration-phase-2.md` §4.3.2 for the full
/// destination-pod startup sequence and §4.1 for the rationale on
/// keeping `spawn_ch_receive` as a sibling of `spawn_ch_restore`
/// (rather than special-casing `spawn_ch`).
pub fn spawn_ch_receive(api_socket: &Path) -> Result<Child, std::io::Error> {
    let binary =
        std::env::var("KUBESWIFT_CH_BINARY").unwrap_or_else(|_| DEFAULT_CH_BINARY.to_string());
    rm_stale_api_socket(api_socket);
    let args = receive_args(api_socket);
    std::process::Command::new(&binary).args(&args).spawn()
}

/// Build the argv for a receive-migration CH invocation. Split out from
/// [`spawn_ch_receive`] so the argv shape is unit-testable without
/// actually spawning CH.
fn receive_args(api_socket: &Path) -> Vec<String> {
    vec![format!("--api-socket={}", api_socket.display())]
}

/// Build the argv for a restore-receive CH invocation. Split out from
/// [`spawn_ch_restore`] so the argv shape is unit-testable without
/// actually spawning CH.
fn restore_args(api_socket: &Path, source_url: &str) -> Vec<String> {
    vec![
        format!("--api-socket={}", api_socket.display()),
        // CH expects --restore as a separate flag, with `source_url=...`
        // as its single value (no equals after the flag itself).
        "--restore".to_string(),
        format!("source_url={}", source_url),
    ]
}

/// Polls until the socket file exists, then returns.
/// Returns Err if timeout is exceeded.
pub fn wait_for_socket(socket_path: &Path, timeout: Duration) -> Result<(), String> {
    let start = std::time::Instant::now();
    while start.elapsed() < timeout {
        if socket_path.exists() {
            return Ok(());
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    Err(format!(
        "timeout waiting for socket at {}",
        socket_path.display()
    ))
}

/// Connects to the Cloud Hypervisor API socket.
///
/// Deprecated: superseded by [`crate::ApiClient`] which provides the
/// full HTTP-over-UDS surface. Kept as a no-op stub so existing callers
/// (only [`crate::spawn_ch`] consumers) continue to compile during the
/// Phase 2 migration. Will be removed in a follow-up cleanup.
pub fn connect_socket(_socket_path: &Path) -> Result<(), String> {
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;
    use std::path::PathBuf;
    use std::sync::Mutex;

    /// Serializes the three `*_honors_binary_env_var` tests + the
    /// `spawn_ch_receive_cleans_up_stale_socket` test.
    ///
    /// `KUBESWIFT_CH_BINARY` is process-wide; cargo test runs tests in
    /// parallel by default. Without this lock, two tests that both
    /// `set_var` race: test A sets path-A, test B sets path-B
    /// (clobbering A), test A's `spawn_ch_*` reads the env var and
    /// gets path-B, test A's argv ends up in test B's `argv.log`,
    /// test A's `read_to_string(&argv_log)` returns ENOENT.
    ///
    /// This race went undetected when `spawn_ch_restore_honors_binary_env_var`
    /// was the only test of its kind (PR-A doubled the surface by
    /// adding two more `spawn_ch_receive_*` tests). CI's GitHub
    /// Actions runner triggered it on the second post-PR-A run.
    ///
    /// Use `.lock().unwrap()` even though we don't carry shared
    /// data across the lock — the side effect we're guarding is the
    /// process-wide env var, not a Rust value.
    fn env_lock() -> &'static Mutex<()> {
        static LOCK: std::sync::OnceLock<Mutex<()>> = std::sync::OnceLock::new();
        LOCK.get_or_init(|| Mutex::new(()))
    }

    #[test]
    fn restore_args_includes_api_socket_and_source_url() {
        let args = restore_args(
            Path::new("/run/foo/ch.sock"),
            "file:///var/lib/kubeswift/snapshots/default-snap1/",
        );
        assert_eq!(args.len(), 3);
        assert_eq!(args[0], "--api-socket=/run/foo/ch.sock");
        assert_eq!(args[1], "--restore");
        assert_eq!(
            args[2],
            "source_url=file:///var/lib/kubeswift/snapshots/default-snap1/"
        );
    }

    #[test]
    fn restore_args_relative_socket_path() {
        let args = restore_args(Path::new("ch.sock"), "file:///snap");
        assert_eq!(args[0], "--api-socket=ch.sock");
    }

    #[test]
    fn restore_args_does_not_include_disk_or_network_flags() {
        // The whole point of restore-receive mode: CH gets the VM
        // shape from config.json in the snapshot directory, not from
        // CLI flags. Asserting the absence here protects against a
        // future regression where someone tries to "helpfully" pass
        // disks or networks through.
        let args = restore_args(Path::new("/run/ch.sock"), "file:///snap");
        let joined = args.join(" ");
        assert!(!joined.contains("--disk"), "argv leaked --disk: {}", joined);
        assert!(!joined.contains("--net"), "argv leaked --net: {}", joined);
        assert!(
            !joined.contains("--kernel"),
            "argv leaked --kernel: {}",
            joined
        );
        assert!(
            !joined.contains("--memory"),
            "argv leaked --memory: {}",
            joined
        );
        assert!(!joined.contains("--cpus"), "argv leaked --cpus: {}", joined);
    }

    #[test]
    fn spawn_ch_restore_honors_binary_env_var() {
        // Use a tempfile-backed shell script as the fake CH binary;
        // it records its argv to a sentinel file then exits 0. We
        // can then assert the spawn invoked the right binary with
        // the right args.
        let _guard = env_lock().lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        let argv_log = tmp.path().join("argv.log");
        let script_path = tmp.path().join("fake-ch.sh");
        let script = format!(
            "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\" >> {}; done\nexit 0\n",
            argv_log.display()
        );
        std::fs::write(&script_path, script).unwrap();
        let mut perms = std::fs::metadata(&script_path).unwrap().permissions();
        perms.set_mode(0o755);
        std::fs::set_permissions(&script_path, perms).unwrap();

        // SAFETY: tests in this module are run serially on the same
        // env var by cargo test default. We restore the original
        // value after.
        let prev = std::env::var("KUBESWIFT_CH_BINARY").ok();
        // SAFETY: Rust 1.86 marks env::set_var unsafe in multi-threaded
        // contexts; cargo test's default test runner is multi-threaded
        // but this test reads/writes only this single var serially via
        // the prev/restore guard above.
        unsafe {
            std::env::set_var("KUBESWIFT_CH_BINARY", &script_path);
        }
        let mut child = spawn_ch_restore(
            Path::new("/run/foo/ch.sock"),
            "file:///var/lib/kubeswift/snapshots/x/",
        )
        .expect("spawn fake CH");
        let status = child.wait().expect("wait fake CH");
        assert!(status.success());
        // SAFETY: same as above.
        unsafe {
            match prev {
                Some(v) => std::env::set_var("KUBESWIFT_CH_BINARY", v),
                None => std::env::remove_var("KUBESWIFT_CH_BINARY"),
            }
        }

        let logged = std::fs::read_to_string(&argv_log).unwrap();
        let lines: Vec<&str> = logged.lines().collect();
        assert_eq!(lines.len(), 3, "argv had {:?}", lines);
        assert_eq!(lines[0], "--api-socket=/run/foo/ch.sock");
        assert_eq!(lines[1], "--restore");
        assert_eq!(
            lines[2],
            "source_url=file:///var/lib/kubeswift/snapshots/x/"
        );
    }

    #[test]
    fn receive_args_only_includes_api_socket() {
        // Receive-migration mode is the leanest of the three spawn
        // variants — `--api-socket=<path>` and nothing else. CH's
        // entire VM shape arrives over the migration wire from the
        // source, so emitting any --cpus/--memory/--disk/--net here
        // would either be ignored (best case) or conflict with the
        // restored config (worst case).
        let args = receive_args(Path::new("/run/foo/ch.sock"));
        assert_eq!(args.len(), 1);
        assert_eq!(args[0], "--api-socket=/run/foo/ch.sock");
    }

    #[test]
    fn receive_args_does_not_include_disk_network_or_payload_flags() {
        // Same shape as restore_args_does_not_include_disk_or_network_flags.
        // Protects against a future regression where someone tries to
        // "helpfully" pre-configure the destination CH with the source's
        // disk paths or network shape.
        let args = receive_args(Path::new("/run/ch.sock"));
        let joined = args.join(" ");
        assert!(!joined.contains("--disk"), "argv leaked --disk: {}", joined);
        assert!(!joined.contains("--net"), "argv leaked --net: {}", joined);
        assert!(
            !joined.contains("--kernel"),
            "argv leaked --kernel: {}",
            joined
        );
        assert!(
            !joined.contains("--memory"),
            "argv leaked --memory: {}",
            joined
        );
        assert!(!joined.contains("--cpus"), "argv leaked --cpus: {}", joined);
        assert!(
            !joined.contains("--restore"),
            "argv leaked --restore: {}",
            joined
        );
    }

    #[test]
    fn rm_stale_api_socket_is_idempotent_when_path_missing() {
        // The cleanup must succeed silently when the socket file does
        // not exist (the normal startup case). Otherwise every fresh
        // pod would log a misleading "remove failed" warning.
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("not-a-socket.sock");
        assert!(!p.exists());
        rm_stale_api_socket(&p);
        assert!(!p.exists()); // still missing, no error path
    }

    #[test]
    fn rm_stale_api_socket_removes_existing_file() {
        // The W2 walkthrough finding: a prior CH instance SIGKILL'd by
        // dst-kill leaves its API socket file behind, blocking the
        // next CH startup with "Address in use". The cleanup must
        // actually remove the file when it exists.
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("stale.sock");
        std::fs::write(&p, b"stale socket bytes").unwrap();
        assert!(p.exists());
        rm_stale_api_socket(&p);
        assert!(!p.exists(), "stale socket file was not removed");
    }

    #[test]
    fn spawn_ch_receive_honors_binary_env_var() {
        // Mirrors spawn_ch_restore_honors_binary_env_var but for the
        // new receive variant — asserts the spawned binary gets
        // exactly --api-socket=<path> and nothing else.
        let _guard = env_lock().lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        let argv_log = tmp.path().join("argv.log");
        let script_path = tmp.path().join("fake-ch.sh");
        let script = format!(
            "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\" >> {}; done\nexit 0\n",
            argv_log.display()
        );
        std::fs::write(&script_path, script).unwrap();
        let mut perms = std::fs::metadata(&script_path).unwrap().permissions();
        perms.set_mode(0o755);
        std::fs::set_permissions(&script_path, perms).unwrap();

        let prev = std::env::var("KUBESWIFT_CH_BINARY").ok();
        // SAFETY: cargo test's default test runner is multi-threaded but
        // this test reads/writes only this single var serially via
        // the prev/restore guard.
        unsafe {
            std::env::set_var("KUBESWIFT_CH_BINARY", &script_path);
        }
        let mut child = spawn_ch_receive(Path::new("/run/foo/ch.sock")).expect("spawn fake CH");
        let status = child.wait().expect("wait fake CH");
        assert!(status.success());
        // SAFETY: same as above.
        unsafe {
            match prev {
                Some(v) => std::env::set_var("KUBESWIFT_CH_BINARY", v),
                None => std::env::remove_var("KUBESWIFT_CH_BINARY"),
            }
        }

        let logged = std::fs::read_to_string(&argv_log).unwrap();
        let lines: Vec<&str> = logged.lines().collect();
        assert_eq!(lines.len(), 1, "argv had {:?}", lines);
        assert_eq!(lines[0], "--api-socket=/run/foo/ch.sock");
    }

    #[test]
    fn spawn_ch_receive_cleans_up_stale_socket() {
        // End-to-end W2 protection: an existing socket file at the
        // target path must be removed before CH spawn fires, even if
        // the file content is non-empty (a real CH socket left by a
        // prior SIGKILL'd process).
        let _guard = env_lock().lock().unwrap();
        let tmp = tempfile::tempdir().unwrap();
        let argv_log = tmp.path().join("argv.log");
        let socket_path = tmp.path().join("ch.sock");
        // Pre-seed a stale "socket" file (just bytes; we only care
        // about file existence for the cleanup check).
        std::fs::write(&socket_path, b"stale").unwrap();
        assert!(socket_path.exists());

        let script_path = tmp.path().join("fake-ch.sh");
        // Fake CH records its argv and exits without touching the
        // socket path — the test asserts that the cleanup step
        // (not the fake CH) is what removed the stale file.
        let script = format!(
            "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\" >> {}; done\nexit 0\n",
            argv_log.display()
        );
        std::fs::write(&script_path, script).unwrap();
        let mut perms = std::fs::metadata(&script_path).unwrap().permissions();
        perms.set_mode(0o755);
        std::fs::set_permissions(&script_path, perms).unwrap();

        let prev = std::env::var("KUBESWIFT_CH_BINARY").ok();
        unsafe {
            std::env::set_var("KUBESWIFT_CH_BINARY", &script_path);
        }
        let mut child = spawn_ch_receive(&socket_path).expect("spawn fake CH");
        let _ = child.wait();
        unsafe {
            match prev {
                Some(v) => std::env::set_var("KUBESWIFT_CH_BINARY", v),
                None => std::env::remove_var("KUBESWIFT_CH_BINARY"),
            }
        }

        // The stale file must be gone (cleanup ran before fake-ch).
        assert!(
            !socket_path.exists(),
            "stale socket file persisted past cleanup"
        );
    }

    #[test]
    fn wait_for_socket_returns_immediately_when_present() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("present.sock");
        std::fs::write(&p, b"").unwrap();
        wait_for_socket(&p, Duration::from_millis(100)).unwrap();
    }

    #[test]
    fn wait_for_socket_times_out_when_absent() {
        let path = PathBuf::from("/does/not/exist/socket");
        let err = wait_for_socket(&path, Duration::from_millis(100)).unwrap_err();
        assert!(err.contains("timeout"));
    }
}

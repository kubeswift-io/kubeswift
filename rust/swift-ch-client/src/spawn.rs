//! Spawn Cloud Hypervisor and connect to socket.

use std::path::Path;
use std::process::Child;
use std::time::Duration;

use crate::config::DEFAULT_CH_BINARY;
use crate::VmConfig;

/// Spawns the Cloud Hypervisor process with the given config.
/// Returns the child process. The API socket will be created by CH when ready.
pub fn spawn_ch(config: &VmConfig) -> Result<Child, std::io::Error> {
    let binary =
        std::env::var("KUBESWIFT_CH_BINARY").unwrap_or_else(|_| DEFAULT_CH_BINARY.to_string());

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
    let args = restore_args(api_socket, source_url);
    std::process::Command::new(&binary).args(&args).spawn()
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

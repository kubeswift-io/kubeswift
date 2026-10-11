//! Warm-slot readiness: report when the guest agent first answers.
//!
//! A warm slot boots with no workload; a checkout later hands its workload to
//! the guest agent over vsock. The bridge starts the agent only after the
//! guest kernel has booted (DHCP included) and the root filesystem, model and
//! staging share are mounted, which is seconds after this launcher started.
//! Until the agent answers, a checkout's exec finds nothing listening and
//! fails, so the controller hands a slot to no checkout before this module has
//! written `kubeswift.io/guest-agent-ready`.

use std::path::PathBuf;
use std::thread;
use std::time::{Duration, Instant};

use k8s_openapi::api::core::v1::Pod;
use kube::api::{Api, Patch, PatchParams};
use serde_json::json;

/// Written once, the first time the guest agent answers. Must match the
/// controller's GuestAgentReadyAnnotation (internal/controller/swiftsandbox).
pub const ANNOTATION: &str = "kubeswift.io/guest-agent-ready";

/// One ping's bound: a guest whose vsock device is not up yet leaves the
/// connection pending rather than refusing it.
const PING_TIMEOUT: Duration = Duration::from_secs(1);
/// Asked this often while a guest is booting...
const FAST: Duration = Duration::from_millis(100);
/// ...for this long; after that a guest that has not answered is unusual, and
/// asking once a second is enough.
const FAST_FOR: Duration = Duration::from_secs(120);
const SLOW: Duration = Duration::from_secs(1);

const REPORT_ATTEMPTS: u32 = 30;
const REPORT_BACKOFF: Duration = Duration::from_secs(1);

/// How long to wait before the next ping, `elapsed` after the first.
fn interval(elapsed: Duration) -> Duration {
    if elapsed < FAST_FOR {
        FAST
    } else {
        SLOW
    }
}

/// Pings the guest agent behind `vsock_socket` until it answers ok. Returns
/// the number of pings sent. Runs until the agent answers: the process ends
/// with the microVM, and with it this wait.
fn wait_for_agent(vsock_socket: &std::path::Path) -> u32 {
    let start = Instant::now();
    let mut attempts = 0u32;
    loop {
        attempts += 1;
        match swift_vsock_client::ping(vsock_socket, PING_TIMEOUT) {
            Ok(r) if r.ok => return attempts,
            Ok(r) => log::debug!("guest_agent_ping_not_ok error={:?}", r.error),
            Err(e) => log::debug!("guest_agent_ping_failed {}", e),
        }
        thread::sleep(interval(start.elapsed()));
    }
}

/// Starts the wait on its own thread; when the agent answers, writes
/// [`ANNOTATION`] on this launcher pod.
pub fn spawn(vsock_socket: PathBuf, namespace: String, pod: String) {
    thread::spawn(move || {
        let start = Instant::now();
        let attempts = wait_for_agent(&vsock_socket);
        log::info!(
            "guest_agent_ready elapsed_ms={} pings={}",
            start.elapsed().as_millis(),
            attempts
        );
        report(&namespace, &pod);
    });
}

/// Writes the annotation, retrying: a missed write leaves the slot unusable
/// (never handed to a checkout), so a give-up logs at ERROR.
fn report(namespace: &str, pod: &str) {
    let Ok(rt) = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    else {
        log::error!("guest_agent_ready_report_failed: no runtime");
        return;
    };
    let now = chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true);
    let patch = json!({"metadata": {"annotations": {ANNOTATION: now}}});
    for attempt in 1..=REPORT_ATTEMPTS {
        let res = rt.block_on(async {
            let client = crate::kube_client::create_client()
                .await
                .map_err(|e| e.to_string())?;
            let api: Api<Pod> = Api::namespaced(client, namespace);
            api.patch(pod, &PatchParams::default(), &Patch::Merge(&patch))
                .await
                .map(|_| ())
                .map_err(|e| e.to_string())
        });
        match res {
            Ok(()) => {
                log::info!("pod_annotation_patched {}={}", ANNOTATION, now);
                return;
            }
            Err(e) if attempt < REPORT_ATTEMPTS => {
                log::warn!("guest_agent_ready_report_failed attempt={} (will retry): {}", attempt, e);
                thread::sleep(REPORT_BACKOFF);
            }
            Err(e) => log::error!(
                "guest_agent_ready_report_failed after {} attempts: {}; this slot will not be checked out",
                REPORT_ATTEMPTS,
                e
            ),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader, Write};
    use std::os::unix::net::UnixListener;

    #[test]
    fn interval_is_fast_while_booting_then_slow() {
        assert_eq!(interval(Duration::ZERO), FAST);
        assert_eq!(interval(FAST_FOR - Duration::from_millis(1)), FAST);
        assert_eq!(interval(FAST_FOR), SLOW);
    }

    // The guest boots: no socket at first, then a hypervisor socket whose
    // guest has no listener yet (handshake refused), then the agent answers.
    #[test]
    fn waits_through_boot_until_the_agent_answers() {
        let dir = tempfile::tempdir().unwrap();
        let sock = dir.path().join("vsock.sock");
        let path = sock.clone();
        let h = thread::spawn(move || {
            thread::sleep(Duration::from_millis(250));
            let listener = UnixListener::bind(&path).unwrap();
            for refuse in [true, true, false] {
                let (stream, _) = listener.accept().unwrap();
                let mut w = stream.try_clone().unwrap();
                let mut r = BufReader::new(stream);
                let mut line = String::new();
                r.read_line(&mut line).unwrap();
                if refuse {
                    // what CH does when the guest resets the connection
                    drop(w);
                    continue;
                }
                w.write_all(b"OK 1024\n").unwrap();
                line.clear();
                r.read_line(&mut line).unwrap();
                assert!(line.contains("\"op\":\"ping\""), "request={}", line);
                w.write_all(b"{\"v\":1,\"ok\":true,\"op\":\"ping\"}\n")
                    .unwrap();
            }
        });
        let pings = wait_for_agent(&sock);
        h.join().unwrap();
        assert!(pings >= 4, "answered after {} pings", pings);
    }

    // An agent that answers but says not ok is not ready.
    #[test]
    fn a_not_ok_answer_is_not_ready() {
        let dir = tempfile::tempdir().unwrap();
        let sock = dir.path().join("vsock.sock");
        let listener = UnixListener::bind(&sock).unwrap();
        let h = thread::spawn(move || {
            for ok in [false, true] {
                let (stream, _) = listener.accept().unwrap();
                let mut w = stream.try_clone().unwrap();
                let mut r = BufReader::new(stream);
                let mut line = String::new();
                r.read_line(&mut line).unwrap();
                w.write_all(b"OK 1024\n").unwrap();
                r.read_line(&mut line).unwrap();
                w.write_all(format!("{{\"v\":1,\"ok\":{}}}\n", ok).as_bytes())
                    .unwrap();
            }
        });
        assert_eq!(wait_for_agent(&sock), 2);
        h.join().unwrap();
    }
}

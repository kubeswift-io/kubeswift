//! Workload probes for a SwiftSandbox (spec.readinessProbe / spec.livenessProbe).
//!
//! The guest's address is private to the launcher pod, so the kubelet cannot
//! probe it and a warm slot's probes cannot be fixed at pod creation. swiftletd
//! runs them instead, from inside the launcher, against the guest's address:
//! an HTTP GET (any 2xx or 3xx passes, as for the kubelet) or a TCP connect.
//!
//! Results go to the pod as annotations, written only when a result changes:
//! - `kubeswift.io/sandbox-workload-ready` = "true" / "false", with the probe's
//!   last message in `kubeswift.io/sandbox-workload-ready-detail`;
//! - `kubeswift.io/sandbox-liveness-failed` = the failing probe's message, once
//!   liveness reaches its failure threshold. Probing then stops; the controller
//!   fails the sandbox (LivenessProbeFailed) and deletes the pod, as it does
//!   for spec.timeout.
//!
//! The thresholds follow the kubelet: successThreshold passes in a row make
//! the workload ready, failureThreshold failures in a row make it not ready
//! (or, for liveness, failed). initialDelaySeconds counts from the moment the
//! guest has an address.

use std::io::{Read, Write};
use std::net::{IpAddr, SocketAddr, TcpStream};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use kube::api::{Api, Patch, PatchParams};
use serde_json::json;

use crate::intent::{SandboxProbe, SandboxProbes};

pub const ANN_WORKLOAD_READY: &str = "kubeswift.io/sandbox-workload-ready";
pub const ANN_WORKLOAD_READY_DETAIL: &str = "kubeswift.io/sandbox-workload-ready-detail";
pub const ANN_LIVENESS_FAILED: &str = "kubeswift.io/sandbox-liveness-failed";

/// How long the runner sleeps between checks for due probes and cancellation.
const TICK: Duration = Duration::from_millis(200);
/// Longest status line read from an HTTP probe response.
const MAX_STATUS_LINE: usize = 1024;

/// Applies the kubelet's threshold rules to a stream of probe results.
#[derive(Debug)]
pub struct Tracker {
    success_threshold: u32,
    failure_threshold: u32,
    successes: u32,
    failures: u32,
    state: Option<bool>,
}

impl Tracker {
    pub fn new(p: &SandboxProbe) -> Self {
        Tracker {
            success_threshold: p.success_threshold.max(1),
            failure_threshold: p.failure_threshold.max(1),
            successes: 0,
            failures: 0,
            state: None,
        }
    }

    /// Records one result. Returns the new state when it changes: true after
    /// success_threshold passes in a row, false after failure_threshold
    /// failures in a row. The first decisive result always counts as a change.
    pub fn observe(&mut self, ok: bool) -> Option<bool> {
        if ok {
            self.successes += 1;
            self.failures = 0;
            if self.successes >= self.success_threshold && self.state != Some(true) {
                self.state = Some(true);
                return Some(true);
            }
        } else {
            self.failures += 1;
            self.successes = 0;
            if self.failures >= self.failure_threshold && self.state != Some(false) {
                self.state = Some(false);
                return Some(false);
            }
        }
        None
    }
}

/// Runs one probe against the guest. Ok and Err both carry a short message.
pub fn probe_once(p: &SandboxProbe, ip: IpAddr) -> Result<String, String> {
    let addr = SocketAddr::new(ip, p.port);
    let timeout = Duration::from_secs(u64::from(p.timeout_seconds.max(1)));
    match p.kind.as_str() {
        "tcp" => TcpStream::connect_timeout(&addr, timeout)
            .map(|_| format!("tcp connect {} succeeded", addr))
            .map_err(|e| format!("tcp connect {}: {}", addr, e)),
        "http" => http_get(p, addr, timeout),
        other => Err(format!("unknown probe kind {:?}", other)),
    }
}

fn http_get(p: &SandboxProbe, addr: SocketAddr, timeout: Duration) -> Result<String, String> {
    let deadline = Instant::now() + timeout;
    let path = if p.path.is_empty() {
        "/"
    } else {
        p.path.as_str()
    };
    let what = format!("http GET {}{}", addr, path);
    let mut stream =
        TcpStream::connect_timeout(&addr, timeout).map_err(|e| format!("{}: {}", what, e))?;
    let remaining = |what: &str| {
        deadline
            .checked_duration_since(Instant::now())
            .filter(|d| !d.is_zero())
            .ok_or_else(|| format!("{}: timed out after {}s", what, timeout.as_secs()))
    };
    stream
        .set_write_timeout(Some(remaining(&what)?))
        .map_err(|e| format!("{}: {}", what, e))?;
    stream
        .write_all(http_request(p, addr).as_bytes())
        .map_err(|e| format!("{}: {}", what, e))?;

    let mut buf = Vec::with_capacity(256);
    let mut chunk = [0u8; 256];
    while !buf.contains(&b'\n') && buf.len() < MAX_STATUS_LINE {
        stream
            .set_read_timeout(Some(remaining(&what)?))
            .map_err(|e| format!("{}: {}", what, e))?;
        match stream.read(&mut chunk) {
            Ok(0) => break,
            Ok(n) => buf.extend_from_slice(&chunk[..n]),
            Err(e) => return Err(format!("{}: {}", what, e)),
        }
    }
    let line = String::from_utf8_lossy(&buf);
    let line = line.lines().next().unwrap_or("");
    match parse_status(line) {
        Some(code) if (200..400).contains(&code) => Ok(format!("{}: {}", what, code)),
        Some(code) => Err(format!("{}: {}", what, code)),
        None => Err(format!("{}: no HTTP status line in the response", what)),
    }
}

/// The request an http probe sends. The controller validated the path and
/// headers (no whitespace in the path, no line breaks in header values).
fn http_request(p: &SandboxProbe, addr: SocketAddr) -> String {
    let path = if p.path.is_empty() {
        "/"
    } else {
        p.path.as_str()
    };
    let mut req = format!("GET {} HTTP/1.1\r\n", path);
    let mut has_host = false;
    let mut has_agent = false;
    for h in &p.headers {
        has_host |= h.name.eq_ignore_ascii_case("host");
        has_agent |= h.name.eq_ignore_ascii_case("user-agent");
        req.push_str(&format!("{}: {}\r\n", h.name, h.value));
    }
    if !has_host {
        req.push_str(&format!("Host: {}\r\n", addr));
    }
    if !has_agent {
        req.push_str("User-Agent: kubeswift-probe/1.0\r\n");
    }
    req.push_str("Accept: */*\r\nConnection: close\r\n\r\n");
    req
}

/// The status code of an HTTP/1.x status line.
fn parse_status(line: &str) -> Option<u16> {
    let mut parts = line.split_whitespace();
    if !parts.next()?.starts_with("HTTP/") {
        return None;
    }
    parts.next()?.parse().ok()
}

/// Stops the runner when dropped (the checkout's workload has ended).
pub struct ProbeGuard {
    cancel: Arc<AtomicBool>,
}

impl Drop for ProbeGuard {
    fn drop(&mut self) {
        self.cancel.store(true, Ordering::SeqCst);
    }
}

/// One probe and when it next runs.
struct Scheduled {
    probe: SandboxProbe,
    tracker: Tracker,
    next: Instant,
}

/// Starts probing on its own thread and returns a guard that stops it. The
/// guest's address is read from the dnsmasq lease file.
pub fn spawn(
    probes: SandboxProbes,
    namespace: String,
    pod: String,
    lease_path: PathBuf,
) -> ProbeGuard {
    let cancel = Arc::new(AtomicBool::new(false));
    let stop = cancel.clone();
    std::thread::Builder::new()
        .name("swiftletd-probe".to_string())
        .spawn(move || run(probes, namespace, pod, lease_path, stop))
        .map_err(|e| log::error!("probe_runner_not_started: {}", e))
        .ok();
    ProbeGuard { cancel }
}

fn run(
    probes: SandboxProbes,
    namespace: String,
    pod: String,
    lease_path: PathBuf,
    stop: Arc<AtomicBool>,
) {
    let rt = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            log::error!("probe_runner_not_started: runtime: {}", e);
            return;
        }
    };

    // Wait for the guest's address.
    let ip = loop {
        if stop.load(Ordering::SeqCst) {
            return;
        }
        if let Some(ip) = std::fs::read_to_string(&lease_path)
            .ok()
            .and_then(|c| crate::lease::parse_first_lease(&c))
            .and_then(|s| s.parse::<IpAddr>().ok())
        {
            break ip;
        }
        std::thread::sleep(Duration::from_secs(1));
    };
    log::info!(
        "probe_runner_started guest={} readiness={} liveness={}",
        ip,
        probes.readiness.is_some(),
        probes.liveness.is_some()
    );

    let start = Instant::now();
    let schedule = |p: SandboxProbe| Scheduled {
        tracker: Tracker::new(&p),
        next: start + Duration::from_secs(u64::from(p.initial_delay_seconds)),
        probe: p,
    };
    let mut readiness = probes.readiness.map(schedule);
    let mut liveness = probes.liveness.map(schedule);
    let mut reporter = Reporter::new(namespace, pod);

    while !stop.load(Ordering::SeqCst) {
        let now = Instant::now();
        if let Some(r) = readiness.as_mut().filter(|r| r.next <= now) {
            let result = probe_once(&r.probe, ip);
            log::debug!("readiness_probe {:?}", result);
            r.next = now + Duration::from_secs(u64::from(r.probe.period_seconds.max(1)));
            if let Some(ready) = r.tracker.observe(result.is_ok()) {
                let msg = result.unwrap_or_else(|e| e);
                log::info!("workload_ready={} {}", ready, msg);
                reporter.queue(&[
                    (ANN_WORKLOAD_READY, ready.to_string()),
                    (ANN_WORKLOAD_READY_DETAIL, msg),
                ]);
            }
        }
        if let Some(l) = liveness.as_mut().filter(|l| l.next <= now) {
            let result = probe_once(&l.probe, ip);
            log::debug!("liveness_probe {:?}", result);
            l.next = now + Duration::from_secs(u64::from(l.probe.period_seconds.max(1)));
            if l.tracker.observe(result.is_ok()) == Some(false) {
                let msg = result.err().unwrap_or_default();
                log::warn!("liveness_failed {}", msg);
                reporter.queue(&[(ANN_LIVENESS_FAILED, msg)]);
                // The controller ends the sandbox; nothing more to probe.
                readiness = None;
                liveness = None;
            }
        }
        reporter.flush(&rt);
        if readiness.is_none() && liveness.is_none() && reporter.is_idle() {
            return;
        }
        std::thread::sleep(TICK);
    }
}

/// Writes queued annotations, retrying on the next tick when a write fails so
/// a transient API error never loses a result.
struct Reporter {
    namespace: String,
    pod: String,
    pending: std::collections::BTreeMap<String, String>,
    client: Option<kube::Client>,
    next_attempt: Instant,
}

impl Reporter {
    fn new(namespace: String, pod: String) -> Self {
        Reporter {
            namespace,
            pod,
            pending: Default::default(),
            client: None,
            next_attempt: Instant::now(),
        }
    }

    fn queue(&mut self, kv: &[(&str, String)]) {
        for (k, v) in kv {
            self.pending.insert((*k).to_string(), v.clone());
        }
    }

    fn is_idle(&self) -> bool {
        self.pending.is_empty()
    }

    fn flush(&mut self, rt: &tokio::runtime::Runtime) {
        if self.pending.is_empty() || Instant::now() < self.next_attempt {
            return;
        }
        let patch = json!({"metadata": {"annotations": &self.pending}});
        let result: Result<(), String> = rt.block_on(async {
            if self.client.is_none() {
                self.client = Some(
                    crate::kube_client::create_client()
                        .await
                        .map_err(|e| format!("kube client: {}", e))?,
                );
            }
            let api: Api<k8s_openapi::api::core::v1::Pod> =
                Api::namespaced(self.client.clone().unwrap(), &self.namespace);
            api.patch(&self.pod, &PatchParams::default(), &Patch::Merge(&patch))
                .await
                .map(|_| ())
                .map_err(|e| e.to_string())
        });
        match result {
            Ok(()) => self.pending.clear(),
            Err(e) => {
                log::warn!("probe_report_failed (will retry): {}", e);
                self.next_attempt = Instant::now() + Duration::from_secs(2);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::intent::ProbeHeader;
    use std::net::TcpListener;

    fn probe(kind: &str, port: u16) -> SandboxProbe {
        SandboxProbe {
            kind: kind.to_string(),
            port,
            path: "/healthz".to_string(),
            headers: vec![],
            initial_delay_seconds: 0,
            period_seconds: 10,
            timeout_seconds: 1,
            success_threshold: 1,
            failure_threshold: 3,
        }
    }

    #[test]
    fn tracker_follows_the_kubelet_thresholds() {
        let mut p = probe("tcp", 1);
        p.success_threshold = 2;
        p.failure_threshold = 3;
        let mut t = Tracker::new(&p);
        assert_eq!(
            t.observe(true),
            None,
            "one pass is below successThreshold 2"
        );
        assert_eq!(t.observe(true), Some(true));
        assert_eq!(t.observe(true), None, "no change, no report");
        assert_eq!(t.observe(false), None);
        assert_eq!(t.observe(false), None);
        assert_eq!(t.observe(true), None, "a pass resets the failure streak");
        assert_eq!(t.observe(false), None);
        assert_eq!(t.observe(false), None);
        assert_eq!(t.observe(false), Some(false), "three failures in a row");
        assert_eq!(t.observe(false), None);
    }

    #[test]
    fn tracker_reports_the_first_decisive_failure() {
        let mut t = Tracker::new(&probe("tcp", 1));
        assert_eq!(t.observe(false), None);
        assert_eq!(t.observe(false), None);
        assert_eq!(
            t.observe(false),
            Some(false),
            "not ready, with a reason, before any pass"
        );
    }

    #[test]
    fn parses_status_lines() {
        assert_eq!(parse_status("HTTP/1.1 200 OK"), Some(200));
        assert_eq!(parse_status("HTTP/1.0 503 Service Unavailable"), Some(503));
        assert_eq!(parse_status("SSH-2.0-OpenSSH"), None);
        assert_eq!(parse_status(""), None);
    }

    #[test]
    fn request_carries_path_and_headers() {
        let mut p = probe("http", 80);
        p.headers = vec![ProbeHeader {
            name: "X-Probe".into(),
            value: "yes".into(),
        }];
        let req = http_request(&p, "10.0.0.1:80".parse().unwrap());
        assert!(req.starts_with("GET /healthz HTTP/1.1\r\n"), "{}", req);
        assert!(req.contains("X-Probe: yes\r\n"));
        assert!(req.contains("Host: 10.0.0.1:80\r\n"));
        assert!(req.ends_with("Connection: close\r\n\r\n"));
        p.headers = vec![ProbeHeader {
            name: "host".into(),
            value: "app.local".into(),
        }];
        let req = http_request(&p, "10.0.0.1:80".parse().unwrap());
        assert!(
            !req.contains("Host: 10.0.0.1"),
            "a Host header from the spec wins"
        );
    }

    /// Serves one connection with the given response.
    fn serve_once(response: &'static str) -> u16 {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        std::thread::spawn(move || {
            if let Ok((mut s, _)) = l.accept() {
                let mut buf = [0u8; 1024];
                let _ = s.read(&mut buf);
                let _ = s.write_all(response.as_bytes());
            }
        });
        port
    }

    #[test]
    fn http_probe_passes_on_2xx_3xx_and_fails_otherwise() {
        let lo: IpAddr = "127.0.0.1".parse().unwrap();
        for (resp, ok) in [
            ("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", true),
            ("HTTP/1.1 302 Found\r\nLocation: /x\r\n\r\n", true),
            ("HTTP/1.1 503 Service Unavailable\r\n\r\n", false),
            ("garbage\r\n", false),
        ] {
            let port = serve_once(resp);
            let got = probe_once(&probe("http", port), lo);
            assert_eq!(got.is_ok(), ok, "{:?} for {:?}", got, resp);
        }
    }

    #[test]
    fn http_probe_times_out_on_a_silent_server() {
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        std::thread::spawn(move || {
            let _conn = l.accept();
            std::thread::sleep(Duration::from_secs(3));
        });
        let started = Instant::now();
        let got = probe_once(&probe("http", port), "127.0.0.1".parse().unwrap());
        assert!(got.is_err(), "{:?}", got);
        assert!(
            started.elapsed() < Duration::from_secs(3),
            "the 1s timeout bounds the whole probe"
        );
    }

    #[test]
    fn tcp_probe_passes_only_when_something_listens() {
        let lo: IpAddr = "127.0.0.1".parse().unwrap();
        let l = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        assert!(probe_once(&probe("tcp", port), lo).is_ok());
        drop(l);
        assert!(probe_once(&probe("tcp", port), lo).is_err());
    }
}

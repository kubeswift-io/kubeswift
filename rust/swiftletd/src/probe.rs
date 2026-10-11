//! Workload probes for a SwiftSandbox (spec.readinessProbe / spec.livenessProbe).
//!
//! The guest's address is private to the launcher pod, so the kubelet cannot
//! probe it and a warm slot's probes cannot be fixed at pod creation. swiftletd
//! runs them instead, from inside the launcher, against the guest's address:
//! an HTTP GET (any 2xx or 3xx passes, as for the kubelet) or a TCP connect.
//!
//! Results are written only when they change:
//! - readiness, on a pod with the workload readiness gate, to that pod
//!   condition through pods/status: True or False, the message prefixed with
//!   the workload it belongs to. It is the only pod write between a warm
//!   slot's claim and the workload being ready: every other pod write in that
//!   window makes the kubelet wait for its next PLEG relist (up to 1 s) before
//!   it recomputes the pod's Ready condition. A pod without the gate gets
//!   `kubeswift.io/sandbox-workload-ready` = "true" / "false" and the message
//!   in `kubeswift.io/sandbox-workload-ready-detail`, as before;
//! - `kubeswift.io/sandbox-liveness-failed` = the failing probe's message, once
//!   liveness reaches its failure threshold. Probing then stops; the controller
//!   fails the sandbox (LivenessProbeFailed) and deletes the pod, as it does
//!   for spec.timeout.
//!
//! The thresholds follow the kubelet: successThreshold passes in a row make
//! the workload ready, failureThreshold failures in a row make it not ready
//! (or, for liveness, failed). initialDelaySeconds counts from the moment the
//! guest has an address. With readinessStartup, the readiness probe instead
//! starts at once and is retried every interval until it first passes, for at
//! most the window; failures in that phase do not count, and afterwards the
//! probe runs every periodSeconds.

use std::io::{Read, Write};
use std::net::{IpAddr, SocketAddr, TcpStream};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use kube::api::{Api, Patch, PatchParams};
use serde_json::json;

use crate::intent::{ReadinessStartup, SandboxProbe, SandboxProbes};

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

    /// Records one result of a readiness probe's fast start: passes count
    /// toward successThreshold, failures only break the streak. The workload
    /// is not ready yet either way, so a failure reports nothing.
    pub fn observe_startup(&mut self, ok: bool) -> Option<bool> {
        if !ok {
            self.successes = 0;
            return None;
        }
        self.observe(true)
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
    /// readinessStartup: until this instant, and until the probe first
    /// passes, a failed attempt is retried after `fast_interval` and does not
    /// count against failureThreshold.
    fast_until: Option<Instant>,
    fast_interval: Duration,
    /// The probe has passed once; the fast start never comes back.
    passed: bool,
}

impl Scheduled {
    fn new(probe: SandboxProbe, start: Instant, startup: Option<&ReadinessStartup>) -> Self {
        let (next, fast_until, fast_interval) = match startup {
            // The fast start replaces initialDelaySeconds: attempts begin when
            // the workload does, and an early failure costs nothing.
            Some(s) => (
                start,
                Some(start + Duration::from_millis(s.window_ms)),
                Duration::from_millis(s.interval_ms.max(1)),
            ),
            None => (
                start + Duration::from_secs(u64::from(probe.initial_delay_seconds)),
                None,
                Duration::ZERO,
            ),
        };
        Scheduled {
            tracker: Tracker::new(&probe),
            probe,
            next,
            fast_until,
            fast_interval,
            passed: false,
        }
    }

    /// Whether `now` is in the fast start: before the window ends and before
    /// the first pass.
    fn fast(&self, now: Instant) -> bool {
        !self.passed && self.fast_until.is_some_and(|u| now < u)
    }

    /// Records one attempt made at `now` and schedules the next. Returns the
    /// new state when it changes, as `Tracker::observe` does.
    fn record(&mut self, ok: bool, now: Instant) -> Option<bool> {
        let fast = self.fast(now);
        let change = if fast {
            self.tracker.observe_startup(ok)
        } else {
            self.tracker.observe(ok)
        };
        if change == Some(true) {
            self.passed = true;
        }
        let period = Duration::from_secs(u64::from(self.probe.period_seconds.max(1)));
        self.next = if fast && change != Some(true) {
            now + self.fast_interval
        } else {
            now + period
        };
        change
    }
}

/// Starts probing on its own thread and returns a guard that stops it. The
/// guest's address is read from the dnsmasq lease file. `workload` names the
/// workload the probes belong to (a warm slot's action id; empty for a cold
/// sandbox, whose pod runs one workload); it goes into every readiness report.
pub fn spawn(
    probes: SandboxProbes,
    namespace: String,
    pod: String,
    workload: String,
    lease_path: PathBuf,
) -> ProbeGuard {
    let cancel = Arc::new(AtomicBool::new(false));
    let stop = cancel.clone();
    std::thread::Builder::new()
        .name("swiftletd-probe".to_string())
        .spawn(move || run(probes, namespace, pod, workload, lease_path, stop))
        .map_err(|e| log::error!("probe_runner_not_started: {}", e))
        .ok();
    ProbeGuard { cancel }
}

fn run(
    probes: SandboxProbes,
    namespace: String,
    pod: String,
    workload: String,
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
    let mut reporter = Reporter::new(namespace, pod, workload, probes.readiness_gate.clone());
    // Connect now, not at the first result: the first report is the one a
    // Service waits for.
    reporter.connect(&rt);

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
        std::thread::sleep(Duration::from_millis(100));
    };
    log::info!(
        "probe_runner_started guest={} readiness={} liveness={} startup={:?} gate={:?}",
        ip,
        probes.readiness.is_some(),
        probes.liveness.is_some(),
        probes.readiness_startup,
        probes.readiness_gate
    );

    let start = Instant::now();
    let startup = probes.readiness_startup.clone();
    let mut readiness = probes
        .readiness
        .map(|p| Scheduled::new(p, start, startup.as_ref()));
    let mut liveness = probes.liveness.map(|p| Scheduled::new(p, start, None));
    let mut reported_ready = false;

    let mut terminating = false;
    while !stop.load(Ordering::SeqCst) {
        // SIGTERM: the sandbox is being deleted. Not ready from now on,
        // whatever the probe says; the drain runs out of Service endpoints.
        if crate::shutdown::terminating() {
            terminating = true;
            break;
        }
        let now = Instant::now();
        if let Some(r) = readiness.as_mut().filter(|r| r.next <= now) {
            let result = probe_once(&r.probe, ip);
            log::debug!("readiness_probe {:?}", result);
            if let Some(ready) = r.record(result.is_ok(), Instant::now()) {
                let msg = result.unwrap_or_else(|e| e);
                log::info!("workload_ready={} {}", ready, msg);
                reporter.queue_ready(ready, msg);
                reported_ready = ready;
            }
        }
        if let Some(l) = liveness.as_mut().filter(|l| l.next <= now) {
            let result = probe_once(&l.probe, ip);
            log::debug!("liveness_probe {:?}", result);
            if l.record(result.is_ok(), Instant::now()) == Some(false) {
                let msg = result.err().unwrap_or_default();
                log::warn!("liveness_failed {}", msg);
                reporter.queue_annotations(&[(ANN_LIVENESS_FAILED, msg)]);
                // The controller ends the sandbox; nothing more to probe.
                readiness = None;
                liveness = None;
            }
        }
        reporter.flush(&rt);
        if readiness.is_none() && liveness.is_none() && reporter.is_idle() {
            return;
        }
        // Sleep until the next probe is due, at most TICK.
        let due = [readiness.as_ref(), liveness.as_ref()]
            .into_iter()
            .flatten()
            .map(|s| s.next)
            .min();
        let wait = due
            .map(|d| d.saturating_duration_since(Instant::now()))
            .unwrap_or(TICK)
            .clamp(Duration::from_millis(1), TICK);
        std::thread::sleep(wait);
    }
    // The workload has ended, or the sandbox is terminating: a pod condition
    // it set must not outlive it. A few prompt attempts; the pod itself ends
    // soon after. On termination the annotation is cleared too.
    if reported_ready && (reporter.gated() || terminating) {
        let why = if terminating {
            "the sandbox is terminating"
        } else {
            "the workload has ended"
        };
        log::info!("workload_ready=false {}", why);
        reporter.queue_ready(false, why.to_string());
        for _ in 0..5 {
            reporter.flush_now(&rt);
            if reporter.is_idle() {
                break;
            }
            std::thread::sleep(RETRY);
        }
    }
}

/// Writes queued results, retrying on the next tick when a write fails so a
/// transient API error never loses a result. Readiness goes to the pod's
/// readiness-gate condition when the pod has one (`gate`), else to
/// annotations; liveness always goes to an annotation.
struct Reporter {
    namespace: String,
    pod: String,
    workload: String,
    gate: String,
    annotations: std::collections::BTreeMap<String, String>,
    condition: Option<(bool, String)>,
    client: Option<kube::Client>,
    // Each kind of write retries on its own: a refused status write must not
    // hold back a liveness annotation, nor the other way round.
    next_condition: Instant,
    next_annotations: Instant,
}

impl Reporter {
    fn new(namespace: String, pod: String, workload: String, gate: String) -> Self {
        Reporter {
            namespace,
            pod,
            workload,
            gate,
            annotations: Default::default(),
            condition: None,
            client: None,
            next_condition: Instant::now(),
            next_annotations: Instant::now(),
        }
    }

    fn gated(&self) -> bool {
        !self.gate.is_empty()
    }

    fn connect(&mut self, rt: &tokio::runtime::Runtime) {
        if self.client.is_none() {
            match rt.block_on(crate::kube_client::create_client()) {
                Ok(c) => self.client = Some(c),
                Err(e) => log::warn!("probe_report_client (will retry): {}", e),
            }
        }
    }

    fn queue_ready(&mut self, ready: bool, msg: String) {
        if self.gated() {
            self.condition = Some((ready, msg));
        } else {
            self.queue_annotations(&[
                (ANN_WORKLOAD_READY, ready.to_string()),
                (ANN_WORKLOAD_READY_DETAIL, msg),
            ]);
        }
    }

    fn queue_annotations(&mut self, kv: &[(&str, String)]) {
        for (k, v) in kv {
            self.annotations.insert((*k).to_string(), v.clone());
        }
    }

    fn is_idle(&self) -> bool {
        self.annotations.is_empty() && self.condition.is_none()
    }

    fn flush(&mut self, rt: &tokio::runtime::Runtime) {
        self.flush_at(rt, Instant::now());
    }

    /// Flushes regardless of the retry times.
    fn flush_now(&mut self, rt: &tokio::runtime::Runtime) {
        self.next_condition = Instant::now();
        self.next_annotations = Instant::now();
        self.flush_at(rt, Instant::now());
    }

    fn flush_at(&mut self, rt: &tokio::runtime::Runtime, now: Instant) {
        let due_condition = self.condition.is_some() && now >= self.next_condition;
        let due_annotations = !self.annotations.is_empty() && now >= self.next_annotations;
        if !due_condition && !due_annotations {
            return;
        }
        self.connect(rt);
        let Some(client) = self.client.clone() else {
            self.next_condition = now + RETRY;
            self.next_annotations = now + RETRY;
            return;
        };
        let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client, &self.namespace);
        if due_condition {
            if let Some((ready, msg)) = self.condition.clone() {
                let body = gate_patch(
                    &self.gate,
                    ready,
                    &self.workload,
                    &msg,
                    &chrono::Utc::now().to_rfc3339(),
                );
                match rt.block_on(api.patch_status(
                    &self.pod,
                    &PatchParams::default(),
                    &Patch::Strategic(&body),
                )) {
                    // A newer verdict queued meanwhile is written next time.
                    Ok(_) if self.condition.as_ref() == Some(&(ready, msg)) => {
                        self.condition = None
                    }
                    Ok(_) => {}
                    Err(e) => {
                        log::warn!("probe_report_failed (will retry): {}", e);
                        self.next_condition = Instant::now() + RETRY;
                    }
                }
            }
        }
        if due_annotations {
            let patch = json!({"metadata": {"annotations": &self.annotations}});
            match rt.block_on(api.patch(&self.pod, &PatchParams::default(), &Patch::Merge(&patch)))
            {
                Ok(_) => self.annotations.clear(),
                Err(e) => {
                    log::warn!("probe_report_failed (will retry): {}", e);
                    self.next_annotations = Instant::now() + RETRY;
                }
            }
        }
    }
}

/// How long a failed report waits before it is tried again.
const RETRY: Duration = Duration::from_millis(250);

/// The pods/status strategic merge patch that sets the readiness-gate
/// condition (merge key: type), leaving every other condition to the kubelet.
/// The message starts with the workload it belongs to, so the controller can
/// tell a report for this workload from any other.
pub fn gate_patch(
    gate: &str,
    ready: bool,
    workload: &str,
    msg: &str,
    now: &str,
) -> serde_json::Value {
    let (status, reason) = if ready {
        ("True", "ProbeSucceeded")
    } else {
        ("False", "ProbeFailed")
    };
    json!({"status": {"conditions": [{
        "type": gate,
        "status": status,
        "reason": reason,
        "message": format!("{}{}", workload_prefix(workload), msg),
        "lastTransitionTime": now,
    }]}})
}

/// "workload <id>: " for a warm slot's workload; nothing for a cold sandbox.
pub fn workload_prefix(workload: &str) -> String {
    if workload.is_empty() {
        String::new()
    } else {
        format!("workload {}: ", workload)
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

    fn startup(interval_ms: u64, window_ms: u64) -> ReadinessStartup {
        ReadinessStartup {
            interval_ms,
            window_ms,
        }
    }

    // readinessStartup: attempts start at once, a failure is retried after the
    // interval and reports nothing, and the first pass ends the fast start.
    #[test]
    fn startup_retries_fast_until_the_first_pass() {
        let mut p = probe("tcp", 1);
        p.initial_delay_seconds = 5;
        p.period_seconds = 10;
        let t0 = Instant::now();
        let mut s = Scheduled::new(p, t0, Some(&startup(100, 30_000)));
        assert_eq!(s.next, t0, "initialDelaySeconds does not apply");
        for i in 0..5 {
            let now = t0 + Duration::from_millis(100 * i);
            assert_eq!(
                s.record(false, now),
                None,
                "a fast-start failure reports nothing"
            );
            assert_eq!(s.next, now + Duration::from_millis(100));
        }
        let now = t0 + Duration::from_millis(500);
        assert_eq!(s.record(true, now), Some(true));
        assert_eq!(
            s.next,
            now + Duration::from_secs(10),
            "then every periodSeconds"
        );
        // Ready now: the kubelet's thresholds apply again.
        assert_eq!(s.record(false, now), None);
        assert_eq!(s.record(false, now), None);
        assert_eq!(s.record(false, now), Some(false), "failureThreshold 3");
    }

    // Once the probe has passed, the fast start is over for good: a later
    // failure inside the window counts and waits periodSeconds.
    #[test]
    fn startup_does_not_come_back_after_a_pass() {
        let p = probe("tcp", 1);
        let t0 = Instant::now();
        let mut s = Scheduled::new(p, t0, Some(&startup(100, 30_000)));
        assert_eq!(s.record(true, t0), Some(true));
        for _ in 0..2 {
            assert_eq!(s.record(false, t0), None);
            assert_eq!(s.next, t0 + Duration::from_secs(10));
        }
        assert_eq!(
            s.record(false, t0),
            Some(false),
            "counted toward failureThreshold"
        );
        assert!(!s.fast(t0));
    }

    // A workload that never becomes ready is probed fast only for the window,
    // then at periodSeconds, and its failures count from then on.
    #[test]
    fn startup_ends_with_the_window() {
        let p = probe("tcp", 1);
        let t0 = Instant::now();
        let mut s = Scheduled::new(p, t0, Some(&startup(200, 1_000)));
        let mut now = t0;
        let mut attempts = 0;
        while s.fast(now) {
            assert_eq!(s.record(false, now), None);
            attempts += 1;
            now = s.next;
        }
        assert_eq!(attempts, 5, "1 s at 200 ms");
        assert_eq!(s.record(false, now), None);
        assert_eq!(s.next, now + Duration::from_secs(10));
        assert_eq!(s.record(false, now), None);
        assert_eq!(
            s.record(false, now),
            Some(false),
            "three failures after the window"
        );
    }

    // successThreshold still holds during the fast start, and a failure
    // breaks the streak.
    #[test]
    fn startup_keeps_the_success_threshold() {
        let mut p = probe("tcp", 1);
        p.success_threshold = 2;
        let t0 = Instant::now();
        let mut s = Scheduled::new(p, t0, Some(&startup(50, 10_000)));
        assert_eq!(s.record(true, t0), None);
        assert_eq!(
            s.next,
            t0 + Duration::from_millis(50),
            "still fast before the threshold"
        );
        assert_eq!(s.record(false, t0), None);
        assert_eq!(s.record(true, t0), None);
        assert_eq!(s.record(true, t0), Some(true));
    }

    // Without readinessStartup nothing changes: initialDelaySeconds, then
    // periodSeconds, failures counted from the first attempt.
    #[test]
    fn without_startup_the_kubelet_schedule_holds() {
        let mut p = probe("tcp", 1);
        p.initial_delay_seconds = 1;
        p.period_seconds = 2;
        let t0 = Instant::now();
        let mut s = Scheduled::new(p, t0, None);
        assert_eq!(s.next, t0 + Duration::from_secs(1));
        assert_eq!(s.record(false, s.next), None);
        assert_eq!(s.next, t0 + Duration::from_secs(3));
    }

    // The readiness report is one condition, keyed by type for a strategic
    // merge, and names the workload it belongs to.
    #[test]
    fn gate_patch_sets_only_the_gate_condition() {
        let body = gate_patch(
            "sandbox.kubeswift.io/workload-ready",
            true,
            "uid-1",
            "http GET 10.0.0.2:80/healthz: 200",
            "2026-10-10T00:00:00Z",
        );
        assert_eq!(
            body,
            json!({"status": {"conditions": [{
                "type": "sandbox.kubeswift.io/workload-ready",
                "status": "True",
                "reason": "ProbeSucceeded",
                "message": "workload uid-1: http GET 10.0.0.2:80/healthz: 200",
                "lastTransitionTime": "2026-10-10T00:00:00Z",
            }]}})
        );
        let off = gate_patch("g", false, "", "the workload has ended", "t");
        assert_eq!(off["status"]["conditions"][0]["status"], "False");
        assert_eq!(
            off["status"]["conditions"][0]["message"],
            "the workload has ended"
        );
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

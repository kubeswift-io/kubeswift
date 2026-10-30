//! Cloud Hypervisor's migration events, and cancelling a send.
//!
//! Every Cloud Hypervisor this launcher spawns gets an event pipe
//! (`--event-monitor fd=`, see `swift_ch_client::events`). A send then waits
//! on what Cloud Hypervisor says happened -- `migration-finished`,
//! `-cancelled`, `-failed` -- instead of inferring it: before v54's events
//! the source API had no failure signal, and a failed transfer was told apart
//! from a running one by its TCP connection disappearing from
//! `/proc/net/tcp` (migconn.rs).
//!
//! Events are an accelerator, never the only evidence. They can be lost (a
//! full pipe drops them; the last ones before Cloud Hypervisor exits may
//! never be written), so [`wait_send`] keeps the earlier observations behind
//! them:
//!
//! - the source Cloud Hypervisor exiting is a completed send unless an event
//!   said the migration failed first -- the same rule as before events;
//! - when the stream shows nothing for this send (no `migration-starting`
//!   soon after the request, or events were dropped unread), the connection
//!   watch takes over, as before;
//! - the deadline still ends the wait.
//!
//! A failed or cancelled send is reported only once `vm.info` shows the
//! source guest running again: the report means "the source is still the
//! guest", and Cloud Hypervisor resumes it after the event.

use std::os::fd::OwnedFd;
use std::sync::atomic::{AtomicU8, Ordering};
use std::sync::OnceLock;
use std::time::{Duration, Instant};

use swift_ch_client::{ApiError, EventStream, SendEnd, SendMigrationOutcome, SendTracker, VmInfo};

/// This launcher's event stream: its Cloud Hypervisor's.
static STREAM: OnceLock<EventStream> = OnceLock::new();

/// Create the event pipe for the Cloud Hypervisor about to be spawned and
/// return the end to give it. None when the pipe could not be made, or one
/// already exists (a launcher runs one Cloud Hypervisor): the launch goes on
/// without events.
pub fn pipe_for_spawn() -> Option<OwnedFd> {
    if STREAM.get().is_some() {
        return None;
    }
    match EventStream::pipe() {
        Ok((stream, write)) => {
            let _ = STREAM.set(stream);
            Some(write)
        }
        Err(e) => {
            log::warn!(
                "ch_events_unavailable err={} (migrations fall back to polling)",
                e
            );
            None
        }
    }
}

pub fn stream() -> Option<&'static EventStream> {
    STREAM.get()
}

/// Launcher features beyond the base set, for `kubeswift.io/launcher-features`.
///
/// - `migration-events`: this launcher's Cloud Hypervisor reports migration
///   events and its sends wait on them.
/// - `migration-cancel`: a `cancel-send` action cancels this launcher's
///   send in Cloud Hypervisor (`vm.cancel-migration`, v54+) and the send
///   reports how it ended. The controller cancels such a source this way
///   and leaves older launchers on the destination-kill cancel.
pub fn features(ch_major: Option<u32>) -> Vec<&'static str> {
    if stream().is_none() {
        return Vec::new();
    }
    let mut f = vec!["migration-events"];
    if ch_major.is_some_and(|m| m >= 54) {
        f.push("migration-cancel");
    }
    f
}

/// Where this launcher's send is, for a `cancel-send`.
const SEND_IDLE: u8 = 0;
const SEND_REQUESTING: u8 = 1;
const SEND_ACCEPTED: u8 = 2;
static SEND_STATE: AtomicU8 = AtomicU8::new(SEND_IDLE);

/// Marks the send in progress for as long as it is held.
pub struct SendInProgress(());

/// The action loop accepted a send: a cancel from now on waits for its
/// request (the send's dispatch thread may not have started yet).
pub fn send_accepted_by_loop() {
    // The action-loop tests run sends without this module's state; its own
    // tests cover the state.
    #[cfg(not(test))]
    SEND_STATE.store(SEND_REQUESTING, Ordering::SeqCst);
}

/// The loop settled a send: none is in progress, even if its dispatch never
/// ran (a thread that could not start).
pub fn send_settled_by_loop() {
    #[cfg(not(test))]
    SEND_STATE.store(SEND_IDLE, Ordering::SeqCst);
}

impl SendInProgress {
    /// The send request is about to go to Cloud Hypervisor.
    pub fn requesting() -> Self {
        SEND_STATE.store(SEND_REQUESTING, Ordering::SeqCst);
        SendInProgress(())
    }

    /// Cloud Hypervisor accepted it: a migration is now in progress there.
    pub fn accepted(&self) {
        SEND_STATE.store(SEND_ACCEPTED, Ordering::SeqCst);
    }
}

impl Drop for SendInProgress {
    fn drop(&mut self) {
        SEND_STATE.store(SEND_IDLE, Ordering::SeqCst);
    }
}

/// The result of a `cancel-send`, written to the source pod. Not an outcome
/// of the migration: the send itself reports that.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CancelResult {
    /// Cloud Hypervisor took the cancel. The send ends `cancelled`, or
    /// `complete` if it was past its last check.
    Requested,
    /// No send is in progress here: there is nothing to cancel.
    NotActive,
}

impl CancelResult {
    pub fn as_str(self) -> &'static str {
        match self {
            CancelResult::Requested => "requested",
            CancelResult::NotActive => "not-active",
        }
    }
}

/// How long a cancel waits for a send request still on its way to Cloud
/// Hypervisor: it answers within milliseconds.
const CANCEL_WAIT_FOR_REQUEST: Duration = Duration::from_secs(10);
const CANCEL_ATTEMPTS: u32 = 3;
const CANCEL_RETRY: Duration = Duration::from_secs(1);

/// Cancel this launcher's send. `cancel` makes one `vm.cancel-migration`
/// call. Errors that could mean the request never arrived are retried; a
/// Cloud Hypervisor that is gone has no migration left to cancel.
pub fn cancel_send(
    cancel: impl FnMut() -> Result<swift_ch_client::CancelMigration, ApiError>,
) -> Result<CancelResult, String> {
    cancel_send_on(&SEND_STATE, cancel)
}

fn cancel_send_on(
    send_state: &AtomicU8,
    mut cancel: impl FnMut() -> Result<swift_ch_client::CancelMigration, ApiError>,
) -> Result<CancelResult, String> {
    let waited = Instant::now();
    while send_state.load(Ordering::SeqCst) == SEND_REQUESTING
        && waited.elapsed() < CANCEL_WAIT_FOR_REQUEST
    {
        std::thread::sleep(Duration::from_millis(20));
    }
    if send_state.load(Ordering::SeqCst) != SEND_ACCEPTED {
        return Ok(CancelResult::NotActive);
    }
    let mut last = String::new();
    for attempt in 0..CANCEL_ATTEMPTS {
        if attempt > 0 {
            std::thread::sleep(CANCEL_RETRY);
        }
        match cancel() {
            Ok(swift_ch_client::CancelMigration::Requested) => return Ok(CancelResult::Requested),
            Ok(swift_ch_client::CancelMigration::NotActive) => return Ok(CancelResult::NotActive),
            Err(ApiError::Connect { .. }) => return Ok(CancelResult::NotActive),
            Err(e) => last = crate::action::sanitize_ch_error(&format!("{:?}", e)).to_string(),
        }
    }
    Err(format!("cancel_migration: {}", last))
}

/// What ended a send's wait.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Evidence {
    /// A Cloud Hypervisor event (with `vm.info` confirming a failure).
    Event,
    /// The source Cloud Hypervisor exited, with no failure event.
    SourceExit,
    /// The connection watch, after the event stream showed nothing.
    ConnectionWatch,
    /// The deadline.
    Deadline,
}

impl Evidence {
    pub fn as_str(self) -> &'static str {
        match self {
            Evidence::Event => "event",
            Evidence::SourceExit => "source-exit",
            Evidence::ConnectionWatch => "connection-watch",
            Evidence::Deadline => "deadline",
        }
    }
}

#[derive(Debug)]
pub struct SendWait {
    pub outcome: SendMigrationOutcome,
    pub evidence: Evidence,
    /// Memory iterations Cloud Hypervisor reported for this send.
    pub iterations: u64,
    /// `vm.info` calls made while waiting.
    pub info_calls: u32,
}

#[derive(Debug, Clone)]
pub struct Timing {
    /// Longest wait for an event before looking again.
    pub event_wait: Duration,
    /// How often `vm.info` is asked while nothing has ended.
    pub info_every: Duration,
    /// How soon this send's `migration-starting` must show before the
    /// stream is treated as unavailable for it.
    pub starting_grace: Duration,
}

impl Default for Timing {
    fn default() -> Self {
        Timing {
            event_wait: Duration::from_millis(500),
            info_every: Duration::from_secs(2),
            starting_grace: Duration::from_secs(10),
        }
    }
}

/// The status detail of a send that did not complete.
pub fn failure_detail(end: SendEnd) -> &'static str {
    match end {
        SendEnd::Cancelled => {
            "cancelled: Cloud Hypervisor stopped the migration on request; the source guest is running"
        }
        SendEnd::Failed { established: false } => {
            "the migration failed before its connection to the destination was established; \
             the source guest is running"
        }
        SendEnd::Failed { established: true } | SendEnd::Finished => {
            "the migration failed during the transfer; Cloud Hypervisor resumed the source guest, \
             which is running"
        }
    }
}

/// Wait for an accepted send to end. `cursor` is the stream's cursor taken
/// before the request; `info` asks the source's `vm.info`; `watch` is the
/// connection watch, consulted only once the stream is unavailable for this
/// send.
pub fn wait_send(
    stream: &EventStream,
    cursor: u64,
    deadline: Instant,
    timing: &Timing,
    mut info: impl FnMut() -> Result<VmInfo, ApiError>,
    mut watch: impl FnMut(Instant, bool) -> Option<String>,
) -> SendWait {
    let accepted = Instant::now();
    let mut tracker = SendTracker::default();
    let mut cur = cursor;
    let mut degraded = false;
    let mut next_info = accepted + timing.info_every;
    let mut info_calls = 0u32;
    // Iterations before this send, which runs one migration at a time.
    let iterations_before = stream.iterations();
    let mut iterations = 0u64;
    let done = |outcome, evidence, iterations, info_calls| SendWait {
        outcome,
        evidence,
        iterations,
        info_calls,
    };
    loop {
        let batch = stream.wait_since(cur, timing.event_wait);
        cur = batch.cursor;
        for e in &batch.events {
            tracker.observe(e);
        }
        if tracker.started() {
            iterations = batch.iterations.saturating_sub(iterations_before);
        }
        let now = Instant::now();
        let end = tracker.end();
        if end == Some(SendEnd::Finished) {
            log::info!("migration_send_end source=event end=finished");
            return done(
                SendMigrationOutcome::Completed,
                Evidence::Event,
                iterations,
                info_calls,
            );
        }
        if end.is_none()
            && !degraded
            && (batch.gap || (!tracker.started() && now >= accepted + timing.starting_grace))
        {
            degraded = true;
            log::warn!(
                "migration_send_events_unavailable gap={} started={} (using the connection watch)",
                batch.gap,
                tracker.started()
            );
        }
        let due = now >= next_info || batch.closed || end.is_some();
        if due {
            next_info = now + timing.info_every;
            info_calls += 1;
            let sample = info();
            match (&sample, end) {
                (Err(ApiError::Connect { .. }), Some(end)) => {
                    log::warn!(
                        "migration_send_end source=event end={:?} then source exited",
                        end
                    );
                    return done(
                        SendMigrationOutcome::Failed(format!(
                            "Cloud Hypervisor exited after the migration {}",
                            if end == SendEnd::Cancelled {
                                "was cancelled"
                            } else {
                                "failed"
                            }
                        )),
                        Evidence::Event,
                        iterations,
                        info_calls,
                    );
                }
                (Err(ApiError::Connect { .. }), None) => {
                    log::info!("migration_send_end source=source-exit");
                    return done(
                        SendMigrationOutcome::Completed,
                        Evidence::SourceExit,
                        iterations,
                        info_calls,
                    );
                }
                (Ok(i), Some(end)) if i.state == "Running" => {
                    log::info!(
                        "migration_send_end source=event end={:?} source=Running",
                        end
                    );
                    return done(
                        SendMigrationOutcome::Failed(failure_detail(end).to_string()),
                        Evidence::Event,
                        iterations,
                        info_calls,
                    );
                }
                _ => {}
            }
            if end.is_none() && degraded {
                let running = matches!(&sample, Ok(i) if i.state == "Running");
                if let Some(reason) = watch(now, running) {
                    log::warn!("migration_send_end source=connection-watch");
                    return done(
                        SendMigrationOutcome::Failed(reason),
                        Evidence::ConnectionWatch,
                        iterations,
                        info_calls,
                    );
                }
            }
        }
        if now >= deadline {
            log::warn!("migration_send_end source=deadline end={:?}", end);
            return done(
                SendMigrationOutcome::Failed(
                    "source guest still Running past the migration deadline \
                     (v53 auto-resumes the source on a failed transfer)"
                        .to_string(),
                ),
                Evidence::Deadline,
                iterations,
                info_calls,
            );
        }
        if batch.closed {
            // The stream has ended: nothing to wait on but time.
            std::thread::sleep(timing.event_wait);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;
    use std::path::PathBuf;
    use std::sync::{Arc, Mutex};
    use swift_ch_client::events::Event;

    fn ev(event: &str) -> Event {
        Event {
            since_start: Duration::ZERO,
            source: "vm".to_string(),
            event: event.to_string(),
            properties: BTreeMap::new(),
        }
    }

    fn fast() -> Timing {
        Timing {
            event_wait: Duration::from_millis(5),
            info_every: Duration::from_millis(20),
            starting_grace: Duration::from_millis(60),
        }
    }

    fn running() -> Result<VmInfo, ApiError> {
        Ok(VmInfo {
            state: "Running".to_string(),
        })
    }

    fn state(s: &str) -> Result<VmInfo, ApiError> {
        Ok(VmInfo {
            state: s.to_string(),
        })
    }

    fn gone() -> Result<VmInfo, ApiError> {
        Err(ApiError::Connect {
            path: PathBuf::from("/run/ch.sock"),
            source: std::io::Error::from(std::io::ErrorKind::ConnectionRefused),
        })
    }

    fn no_watch(_: Instant, _: bool) -> Option<String> {
        None
    }

    fn far() -> Instant {
        Instant::now() + Duration::from_secs(30)
    }

    /// Feed `events` to a stream from another thread after `delay`.
    fn feed(stream: &EventStream, delay: Duration, events: &[&str]) -> std::thread::JoinHandle<()> {
        let s = stream.clone();
        let events: Vec<Event> = events.iter().map(|e| ev(e)).collect();
        std::thread::spawn(move || {
            std::thread::sleep(delay);
            s.record(events, 0);
        })
    }

    // 1. The normal sequence: finished is completion, at once, without
    // waiting for the source to exit.
    #[test]
    fn finished_event_completes_the_send() {
        let s = EventStream::detached();
        // An earlier migration's iterations are not this send's.
        s.record([ev("migration-memory-iteration")], 0);
        let c = s.cursor();
        let t = feed(
            &s,
            Duration::from_millis(10),
            &[
                "migration-starting",
                "migration-started",
                "migration-memory-iteration",
                "migration-memory-iteration",
                "pausing",
                "paused",
                "migration-finished",
            ],
        );
        let w = wait_send(&s, c, far(), &fast(), running, no_watch);
        t.join().unwrap();
        assert_eq!(w.outcome, SendMigrationOutcome::Completed);
        assert_eq!(w.evidence, Evidence::Event);
        assert_eq!(w.iterations, 2);
        assert_eq!(w.info_calls, 0);
    }

    // 2. Stream unavailable (nothing ever arrives): the connection watch
    // decides failure, and source exit still means completion.
    #[test]
    fn silent_stream_falls_back_to_the_connection_watch() {
        let s = EventStream::detached();
        let c = s.cursor();
        let armed = Arc::new(Mutex::new(0));
        let a = armed.clone();
        let w = wait_send(&s, c, far(), &fast(), running, move |_, run| {
            assert!(run);
            *a.lock().unwrap() += 1;
            Some("the migration connection closed".to_string())
        });
        assert_eq!(
            w.outcome,
            SendMigrationOutcome::Failed("the migration connection closed".to_string())
        );
        assert_eq!(w.evidence, Evidence::ConnectionWatch);
        assert!(*armed.lock().unwrap() >= 1);

        let s = EventStream::detached();
        let c = s.cursor();
        let w = wait_send(&s, c, far(), &fast(), gone, no_watch);
        assert_eq!(w.outcome, SendMigrationOutcome::Completed);
        assert_eq!(w.evidence, Evidence::SourceExit);
    }

    // The watch is not consulted while the stream works.
    #[test]
    fn watch_is_not_used_while_events_flow() {
        let s = EventStream::detached();
        let c = s.cursor();
        s.record([ev("migration-starting"), ev("migration-started")], 0);
        let t = feed(&s, Duration::from_millis(150), &["migration-finished"]);
        let w = wait_send(&s, c, far(), &fast(), running, |_, _| {
            panic!("watch consulted while events flow")
        });
        t.join().unwrap();
        assert_eq!(w.outcome, SendMigrationOutcome::Completed);
    }

    // 3. Connection drops / final events lost at exit: the source exiting
    // with no failure event is completion, as it was before events.
    #[test]
    fn exit_without_an_end_event_is_completion() {
        let s = EventStream::detached();
        let c = s.cursor();
        s.record(
            [
                ev("migration-starting"),
                ev("migration-started"),
                ev("paused"),
            ],
            0,
        );
        s.close();
        let w = wait_send(&s, c, far(), &fast(), gone, no_watch);
        assert_eq!(w.outcome, SendMigrationOutcome::Completed);
        assert_eq!(w.evidence, Evidence::SourceExit);
    }

    // A failure event then exit is a crash, never a completion.
    #[test]
    fn exit_after_a_failure_event_is_failure() {
        let s = EventStream::detached();
        let c = s.cursor();
        s.record(
            [
                ev("migration-starting"),
                ev("migration-started"),
                ev("migration-failed"),
            ],
            0,
        );
        let w = wait_send(&s, c, far(), &fast(), gone, no_watch);
        assert!(
            matches!(w.outcome, SendMigrationOutcome::Failed(ref d) if d.contains("exited after the migration failed"))
        );
    }

    // 4. Duplicates, 5. out-of-order: the first end after this send's start
    // decides; repeats and late contradictions change nothing.
    #[test]
    fn duplicates_and_late_events_do_not_change_the_end() {
        let s = EventStream::detached();
        let c = s.cursor();
        s.record(
            [
                "migration-starting",
                "migration-starting",
                "migration-started",
                "migration-cancelled",
                "migration-cancelled",
                "migration-finished",
            ]
            .map(ev),
            0,
        );
        let w = wait_send(&s, c, far(), &fast(), running, no_watch);
        assert_eq!(
            w.outcome,
            SendMigrationOutcome::Failed(failure_detail(SendEnd::Cancelled).to_string())
        );
    }

    // 6. A stale end from an earlier attempt (before this one's start, and
    // before the cursor) does not end this send.
    #[test]
    fn earlier_attempts_end_does_not_end_this_send() {
        let s = EventStream::detached();
        // Attempt A, entirely before B's cursor.
        s.record(
            ["migration-starting", "migration-failed", "resumed"].map(ev),
            0,
        );
        let c = s.cursor();
        // Attempt B: its own start, then completion.
        let t = feed(
            &s,
            Duration::from_millis(30),
            &[
                "migration-starting",
                "migration-started",
                "migration-finished",
            ],
        );
        let w = wait_send(&s, c, far(), &fast(), running, no_watch);
        t.join().unwrap();
        assert_eq!(w.outcome, SendMigrationOutcome::Completed);

        // A's end delivered after B's cursor but before B's start.
        let s = EventStream::detached();
        let c = s.cursor();
        s.record(["migration-failed", "resumed"].map(ev), 0);
        let t = feed(
            &s,
            Duration::from_millis(30),
            &[
                "migration-starting",
                "migration-started",
                "migration-finished",
            ],
        );
        let w = wait_send(&s, c, far(), &fast(), running, no_watch);
        t.join().unwrap();
        assert_eq!(w.outcome, SendMigrationOutcome::Completed);
    }

    // A failure is reported only once the source guest runs again.
    #[test]
    fn failure_waits_for_the_source_to_run() {
        let s = EventStream::detached();
        let c = s.cursor();
        s.record(
            [
                "migration-starting",
                "migration-started",
                "paused",
                "migration-failed",
            ]
            .map(ev),
            0,
        );
        let n = Arc::new(Mutex::new(0));
        let n2 = n.clone();
        let w = wait_send(
            &s,
            c,
            far(),
            &fast(),
            move || {
                let mut k = n2.lock().unwrap();
                *k += 1;
                if *k < 3 {
                    state("Paused")
                } else {
                    running()
                }
            },
            no_watch,
        );
        assert_eq!(
            w.outcome,
            SendMigrationOutcome::Failed(
                failure_detail(SendEnd::Failed { established: true }).to_string()
            )
        );
        assert!(*n.lock().unwrap() >= 3);
    }

    #[test]
    fn a_failure_before_the_connection_says_so() {
        let s = EventStream::detached();
        let c = s.cursor();
        s.record(["migration-starting", "migration-failed"].map(ev), 0);
        let w = wait_send(&s, c, far(), &fast(), running, no_watch);
        assert!(matches!(w.outcome, SendMigrationOutcome::Failed(ref d)
            if d.contains("before its connection to the destination was established")));
    }

    #[test]
    fn deadline_still_ends_the_wait() {
        let s = EventStream::detached();
        let c = s.cursor();
        s.record([ev("migration-starting"), ev("migration-started")], 0);
        let w = wait_send(
            &s,
            c,
            Instant::now() + Duration::from_millis(80),
            &fast(),
            running,
            no_watch,
        );
        assert_eq!(w.evidence, Evidence::Deadline);
    }

    // Polling: with events flowing, vm.info is asked every info_every, not
    // every event wait.
    #[test]
    fn vm_info_is_polled_at_the_slow_cadence() {
        let s = EventStream::detached();
        let c = s.cursor();
        s.record([ev("migration-starting"), ev("migration-started")], 0);
        let t = feed(&s, Duration::from_millis(200), &["migration-finished"]);
        let w = wait_send(&s, c, far(), &fast(), running, no_watch);
        t.join().unwrap();
        // ~200 ms at a 20 ms cadence: about 10, never one per 5 ms wait.
        assert!(w.info_calls <= 12, "info_calls={}", w.info_calls);
    }

    fn cancel_answers(
        answers: Vec<Result<swift_ch_client::CancelMigration, ApiError>>,
    ) -> impl FnMut() -> Result<swift_ch_client::CancelMigration, ApiError> {
        let mut it = answers.into_iter();
        move || it.next().expect("no more answers")
    }

    fn api_500() -> ApiError {
        ApiError::Status(swift_ch_client::ApiResponse {
            status: 500,
            body: Vec::new(),
        })
    }

    #[test]
    fn cancel_send_cases() {
        use swift_ch_client::CancelMigration::*;
        let st = AtomicU8::new(SEND_IDLE);
        // Cancel before any send: nothing to cancel, Cloud Hypervisor not asked.
        assert_eq!(
            cancel_send_on(&st, || panic!("asked")),
            Ok(CancelResult::NotActive)
        );

        // Active send: delivered.
        st.store(SEND_ACCEPTED, Ordering::SeqCst);
        assert_eq!(
            cancel_send_on(&st, cancel_answers(vec![Ok(Requested)])),
            Ok(CancelResult::Requested)
        );
        // After completion Cloud Hypervisor says no migration: not active.
        assert_eq!(
            cancel_send_on(&st, cancel_answers(vec![Ok(NotActive)])),
            Ok(CancelResult::NotActive)
        );
        // The request failed before delivery: retried.
        assert_eq!(
            cancel_send_on(&st, cancel_answers(vec![Err(api_500()), Ok(Requested)])),
            Ok(CancelResult::Requested)
        );
        // Never delivered: an error the controller falls back on.
        let r = cancel_send_on(
            &st,
            cancel_answers(vec![Err(api_500()), Err(api_500()), Err(api_500())]),
        );
        assert!(
            matches!(r, Err(ref e) if e.starts_with("cancel_migration:")),
            "{r:?}"
        );
        // The source Cloud Hypervisor is gone: its migration has ended.
        assert_eq!(
            cancel_send_on(&st, cancel_answers(vec![Err(gone().unwrap_err())])),
            Ok(CancelResult::NotActive)
        );
        // Send over: not active.
        st.store(SEND_IDLE, Ordering::SeqCst);
        assert_eq!(
            cancel_send_on(&st, || panic!("asked")),
            Ok(CancelResult::NotActive)
        );

        // A cancel racing the send request waits for Cloud Hypervisor's answer.
        let st = std::sync::Arc::new(AtomicU8::new(SEND_REQUESTING));
        let st2 = st.clone();
        let t = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(50));
            st2.store(SEND_ACCEPTED, Ordering::SeqCst);
        });
        assert_eq!(
            cancel_send_on(&st, cancel_answers(vec![Ok(Requested)])),
            Ok(CancelResult::Requested)
        );
        t.join().unwrap();
    }
}

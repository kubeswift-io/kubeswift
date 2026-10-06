//! The action loop's view of its own launcher pod.
//!
//! The controller drives an action by writing annotations on the launcher
//! pod. [`PodFeed`] hands the action loop a snapshot of the pod's annotations
//! every time they may have changed, from a watch on the pod's metadata, so an
//! action is seen as soon as the API server delivers the change.
//!
//! The feed starts with a GET, which reads the pod from etcd: the first
//! snapshot, and with it the launcher's own UID, is the pod's current state.
//! Every watch then opens without a resourceVersion: the API server starts it
//! with the pod as its watch cache has it (an `Added`), then sends each change.
//! Nothing is kept that can expire (410 Gone).
//!
//! Snapshots are handed out in resourceVersion order. A watch cache can lag
//! etcd, and with more than one API server a new watch can land on another
//! server, so a new watch can start with an older state than one already
//! seen. A pod's resourceVersions are positive integers that compare as
//! numbers (apimachinery's `CompareResourceVersion`). The feed drops a
//! snapshot older than the newest it handed out, and one equal to it unless
//! [`PodFeed::refresh`] asked for a pass. A GET reads etcd and is never older.
//! If a resourceVersion is not such an integer, the feed cannot order the
//! watch's events: it stops watching and polls, as swiftletd always did.
//!
//! The pod's current state is the source of truth. An event only says when to
//! look again: the loop decides from the whole snapshot, and its decisions are
//! idempotent by action id, so a repeated or a missed event changes nothing a
//! later snapshot does not correct.
//!
//! A watch is replaced, starting again from the current state:
//!   - when the server ends it (its timeout, or an error);
//!   - every [`RESYNC_INTERVAL`] or a little sooner, in case an event was lost
//!     without the watch reporting it;
//!   - on [`PodFeed::refresh`], after the loop changed state of its own.
//!
//! A watch works once it delivers an event. One that cannot be opened (a
//! launcher Role from before swiftletd watched its pod grants no `watch`), is
//! refused (kube-rs reports a refused watch as the stream's first item), sends
//! nothing within [`FIRST_EVENT_TIMEOUT`], ends before it delivers, or ends
//! within [`MIN_STREAM_LIFETIME`] is a failure: the feed polls every
//! [`POLL_INTERVAL`], as swiftletd always did, and opens the watch again after
//! a backoff of up to [`WATCH_RETRY_MAX`]. The resync interval and the backoff
//! are shortened by a random 0 to 20%, so that launchers whose watches ended
//! together (an API server restart) do not stay in step.

use std::cmp::Ordering;
use std::collections::BTreeMap;
use std::time::Duration;

use futures_util::stream::{LocalBoxStream, StreamExt};
use k8s_openapi::api::core::v1::Pod;
use kube::api::{Api, WatchEvent, WatchParams};
use kube::core::PartialObjectMeta;
use tokio::time::Instant;

/// Polling cadence when no watch works. The action loop's only cadence
/// before it watched its pod.
pub const POLL_INTERVAL: Duration = Duration::from_secs(2);

/// How often the watch is replaced by a new one, which starts from the pod's
/// current state.
pub const RESYNC_INTERVAL: Duration = Duration::from_secs(30);

/// Longest wait between attempts to open the watch.
pub const WATCH_RETRY_MAX: Duration = Duration::from_secs(60);

/// A watch the server ends sooner than this after it opened counts as a
/// failure, even if it delivered: reopening it at once could repeat without
/// bound.
pub const MIN_STREAM_LIFETIME: Duration = Duration::from_secs(5);

/// A watch that sends nothing for this long after it opened is a failure.
/// The API server starts every watch with the pod's state at once.
pub const FIRST_EVENT_TIMEOUT: Duration = Duration::from_secs(5);

/// Longest wait for a GET, or for a watch to open.
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(5);

/// Server-side timeout of one watch request.
const WATCH_TIMEOUT_SECS: u32 = 290;

/// The part of the launcher pod the action loop reads.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct PodSnapshot {
    pub uid: Option<String>,
    pub annotations: BTreeMap<String, String>,
}

impl PodSnapshot {
    fn of(meta: &PartialObjectMeta<Pod>) -> Self {
        PodSnapshot {
            uid: meta.metadata.uid.clone(),
            annotations: meta
                .metadata
                .annotations
                .clone()
                .unwrap_or_default()
                .into_iter()
                .collect(),
        }
    }
}

pub type MetaStream =
    LocalBoxStream<'static, Result<WatchEvent<PartialObjectMeta<Pod>>, kube::Error>>;

/// Reads one pod: a GET of its metadata, and a watch of its metadata from its
/// current state. The API server, or a script in tests.
#[allow(async_fn_in_trait)]
pub trait PodSource {
    async fn get(&self) -> Result<PartialObjectMeta<Pod>, kube::Error>;
    async fn watch(&self) -> Result<MetaStream, kube::Error>;
}

/// The launcher's own pod on the API server. The watch is narrowed to the
/// pod's name, which is what the launcher's per-pod Role grants `watch` on.
pub struct KubePodSource {
    api: Api<Pod>,
    name: String,
    params: WatchParams,
}

impl KubePodSource {
    pub fn new(client: kube::Client, namespace: &str, name: &str) -> Self {
        KubePodSource {
            api: Api::namespaced(client, namespace),
            name: name.to_string(),
            params: WatchParams::default()
                .fields(&format!("metadata.name={}", name))
                .timeout(WATCH_TIMEOUT_SECS),
        }
    }
}

impl PodSource for KubePodSource {
    async fn get(&self) -> Result<PartialObjectMeta<Pod>, kube::Error> {
        self.api.get_metadata(&self.name).await
    }

    async fn watch(&self) -> Result<MetaStream, kube::Error> {
        // An empty resourceVersion is "unset": start from the current state.
        Ok(self
            .api
            .watch_metadata(&self.params, "")
            .await?
            .boxed_local())
    }
}

/// An open watch.
struct Open {
    stream: MetaStream,
    opened_at: Instant,
    /// It delivered an event: the server accepted it.
    delivered: bool,
}

impl Open {
    /// It delivered and stayed open long enough to have worked.
    fn worked(&self) -> bool {
        self.delivered && self.opened_at.elapsed() >= MIN_STREAM_LIFETIME
    }
}

/// Hands out pod snapshots: see the module doc.
pub struct PodFeed<S: PodSource> {
    source: S,
    watch: Option<Open>,
    /// When the watch may be opened again after a failure.
    watch_retry_at: Instant,
    watch_failures: u32,
    /// Next GET while no watch works.
    next_poll: Instant,
    /// A GET is due now: at start, and on [`PodFeed::refresh`] while no watch
    /// is open.
    get_now: bool,
    /// When the open watch is replaced.
    next_resync: Instant,
    /// The resourceVersion of the newest snapshot handed out.
    newest: Option<String>,
    /// The next snapshot is wanted even if the pod has not changed.
    want_pass: bool,
    /// The watch's events can be ordered: false once one carried a
    /// resourceVersion that is not an integer.
    ordered: bool,
    /// Jitter state (xorshift64); 0 turns jitter off.
    rng: u64,
}

impl<S: PodSource> PodFeed<S> {
    pub fn new(source: S) -> Self {
        let now = Instant::now();
        PodFeed {
            source,
            watch: None,
            watch_retry_at: now,
            watch_failures: 0,
            next_poll: now,
            get_now: true,
            next_resync: now + RESYNC_INTERVAL,
            newest: None,
            want_pass: false,
            ordered: true,
            rng: jitter_seed(),
        }
    }

    /// Exact intervals, for tests that assert them.
    #[cfg(test)]
    pub fn without_jitter(mut self) -> Self {
        self.rng = 0;
        self
    }

    /// Whether the open watch has delivered: the loop is event-driven rather
    /// than polling.
    #[cfg(test)]
    pub fn watching(&self) -> bool {
        open_delivered(&self.watch)
    }

    /// Make the next snapshot one of the pod as it is now, even if it has not
    /// changed. The loop changed state of its own (a dispatch finished) and
    /// must decide again.
    pub fn refresh(&mut self) {
        self.want_pass = true;
        match &self.watch {
            // A watch that has not delivered yet starts with the pod's state,
            // then sends each change, so it brings the current state.
            Some(open) if !open.delivered => {}
            // A new watch starts with the current state.
            Some(_) => self.drop_watch(),
            None => self.get_now = true,
        }
    }

    /// The next snapshot of the pod. Cancel-safe: every await either resumes
    /// from state kept in `self` or is repeated by the next call, so the loop
    /// may drop this future to handle something else.
    pub async fn next(&mut self) -> PodSnapshot {
        loop {
            if self.watch.is_none() {
                let now = Instant::now();
                let polling = !self.ordered || now < self.watch_retry_at;
                if self.get_now || (polling && now >= self.next_poll) {
                    let got = tokio::time::timeout(REQUEST_TIMEOUT, self.source.get()).await;
                    self.get_now = false;
                    self.next_poll = Instant::now() + POLL_INTERVAL;
                    match got {
                        Ok(Ok(meta)) => {
                            if let Some(snapshot) = self.offer(&meta, true) {
                                return snapshot;
                            }
                        }
                        Ok(Err(e)) => log::warn!("action_loop_get_pod_err: {}", e),
                        Err(_) => log::warn!(
                            "action_loop_get_pod_err: no response within {:?}",
                            REQUEST_TIMEOUT
                        ),
                    }
                    continue;
                }
                if !polling {
                    match tokio::time::timeout(REQUEST_TIMEOUT, self.source.watch()).await {
                        Ok(Ok(stream)) => {
                            let now = Instant::now();
                            log::debug!("action_loop_watch_opened");
                            self.watch = Some(Open {
                                stream,
                                opened_at: now,
                                delivered: false,
                            });
                            self.next_resync = now + self.jittered(RESYNC_INTERVAL);
                        }
                        Ok(Err(e)) => self.watch_failed(&e.to_string()),
                        Err(_) => {
                            self.watch_failed(&format!("no response within {:?}", REQUEST_TIMEOUT))
                        }
                    }
                    continue;
                }
            }
            if let Some(open) = self.watch.as_mut() {
                let deadline = if open.delivered {
                    self.next_resync
                } else {
                    self.next_resync.min(open.opened_at + FIRST_EVENT_TIMEOUT)
                };
                tokio::select! {
                    item = open.stream.next() => match item {
                        Some(Ok(WatchEvent::Added(meta))) | Some(Ok(WatchEvent::Modified(meta))) => {
                            self.delivered();
                            if let Some(snapshot) = self.offer(&meta, false) {
                                return snapshot;
                            }
                        }
                        Some(Ok(WatchEvent::Deleted(_))) => {
                            // The kubelet ends this process with its pod. A pod
                            // created under the same name later arrives as Added.
                            self.delivered();
                            log::debug!("action_loop_pod_deleted");
                        }
                        Some(Ok(WatchEvent::Bookmark(_))) => self.delivered(),
                        Some(Ok(WatchEvent::Error(status))) => {
                            self.watch_ended(&format!("watch error {}: {}", status.code, status.message));
                        }
                        Some(Err(e)) => self.watch_ended(&e.to_string()),
                        None => self.watch_ended("watch closed by the server"),
                    },
                    _ = tokio::time::sleep_until(deadline) => {
                        self.next_resync = Instant::now() + self.jittered(RESYNC_INTERVAL);
                        if open_delivered(&self.watch) {
                            log::debug!("action_loop_resync");
                            self.drop_watch();
                        } else {
                            self.watch_failed(&format!("no event within {:?}", FIRST_EVENT_TIMEOUT));
                        }
                    }
                }
                continue;
            }
            let wake = if self.ordered {
                self.next_poll.min(self.watch_retry_at)
            } else {
                self.next_poll
            };
            tokio::time::sleep_until(wake).await;
        }
    }

    /// The snapshot of `meta`, unless it is older than the newest one handed
    /// out, or equal to it with no pass wanted (see the module doc).
    fn offer(&mut self, meta: &PartialObjectMeta<Pod>, from_get: bool) -> Option<PodSnapshot> {
        let Some(rv) = meta
            .metadata
            .resource_version
            .as_deref()
            .filter(|rv| is_resource_version(rv))
        else {
            if from_get {
                // A GET is never older; it just cannot be compared.
                self.want_pass = false;
                return Some(PodSnapshot::of(meta));
            }
            if self.ordered {
                log::warn!(
                    "action_loop_watch_unordered: resourceVersion {:?} is not an integer; polling every {:?}",
                    meta.metadata.resource_version,
                    POLL_INTERVAL
                );
                self.ordered = false;
            }
            self.watch = None;
            self.get_now = true;
            return None;
        };
        if let Some(newest) = &self.newest {
            match compare_resource_versions(rv, newest) {
                Ordering::Less => {
                    log::debug!(
                        "action_loop_stale_snapshot_dropped resource_version={} newest={}",
                        rv,
                        newest
                    );
                    return None;
                }
                Ordering::Equal if !self.want_pass => return None,
                _ => {}
            }
        }
        self.newest = Some(rv.to_string());
        self.want_pass = false;
        Some(PodSnapshot::of(meta))
    }

    fn delivered(&mut self) {
        if let Some(open) = self.watch.as_mut() {
            open.delivered = true;
        }
    }

    /// The feed replaces the watch itself: not a failure.
    fn drop_watch(&mut self) {
        if let Some(open) = self.watch.take() {
            if open.worked() {
                self.watch_worked();
            }
        }
    }

    /// The server ended the watch. One that worked is reopened at once, from
    /// the current state; any other is a failure.
    fn watch_ended(&mut self, why: &str) {
        match self.watch.take() {
            Some(open) if open.worked() => {
                self.watch_worked();
                log::debug!("action_loop_watch_ended: {}", why);
            }
            _ => self.watch_failed(why),
        }
    }

    fn watch_worked(&mut self) {
        if self.watch_failures > 0 {
            log::info!(
                "action_loop_watch_restored after {} failures",
                self.watch_failures
            );
            self.watch_failures = 0;
        }
    }

    /// The watch could not be opened, or did not work. Poll meanwhile, and
    /// open it again after a backoff.
    fn watch_failed(&mut self, err: &str) {
        self.watch = None;
        self.watch_failures = self.watch_failures.saturating_add(1);
        let delay = self.jittered(watch_backoff(self.watch_failures));
        self.watch_retry_at = Instant::now() + delay;
        if self.watch_failures == 1 {
            log::warn!(
                "action_loop_watch_unavailable: {}; polling every {:?}, retrying the watch with backoff",
                err,
                POLL_INTERVAL
            );
        } else {
            log::debug!(
                "action_loop_watch_retry_failed failures={} next_in={:?}: {}",
                self.watch_failures,
                delay,
                err
            );
        }
    }

    /// `d` shortened by a random 0 to 20%.
    fn jittered(&mut self, d: Duration) -> Duration {
        if self.rng == 0 {
            return d;
        }
        let mut x = self.rng;
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        self.rng = x;
        let unit = (x >> 11) as f64 / (1u64 << 53) as f64;
        d.mul_f64(1.0 - 0.2 * unit)
    }
}

fn open_delivered(watch: &Option<Open>) -> bool {
    watch.as_ref().is_some_and(|w| w.delivered)
}

/// A seed that differs between launchers; never 0.
fn jitter_seed() -> u64 {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or(0);
    (nanos ^ (u64::from(std::process::id()) << 32)) | 1
}

/// A live resourceVersion: a positive integer without leading zeros, of any
/// length (apimachinery's `CompareResourceVersion`).
fn is_resource_version(rv: &str) -> bool {
    !rv.is_empty() && !rv.starts_with('0') && rv.bytes().all(|b| b.is_ascii_digit())
}

/// Numeric order of two resourceVersions that pass [`is_resource_version`].
fn compare_resource_versions(a: &str, b: &str) -> Ordering {
    a.len().cmp(&b.len()).then_with(|| a.cmp(b))
}

/// 1s, 2s, 4s, ... up to [`WATCH_RETRY_MAX`].
fn watch_backoff(failures: u32) -> Duration {
    let secs = 1u64 << failures.saturating_sub(1).min(6);
    Duration::from_secs(secs).min(WATCH_RETRY_MAX)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use futures_channel::mpsc;
    use kube::core::{ObjectMeta, Status};
    use std::cell::RefCell;
    use std::collections::VecDeque;
    use std::rc::Rc;

    pub(crate) type EventTx =
        mpsc::UnboundedSender<Result<WatchEvent<PartialObjectMeta<Pod>>, kube::Error>>;

    /// Fails the test run when a test takes more than a minute of real time.
    /// These tests run on a paused clock and finish in milliseconds; a
    /// regression that spins without yielding would otherwise hang them,
    /// since no timer can fire.
    pub(crate) struct Watchdog(Option<std::sync::mpsc::Sender<()>>);

    pub(crate) fn watchdog(test: &'static str) -> Watchdog {
        let (tx, rx) = std::sync::mpsc::channel::<()>();
        std::thread::spawn(move || {
            if let Err(std::sync::mpsc::RecvTimeoutError::Timeout) =
                rx.recv_timeout(Duration::from_secs(60))
            {
                // Not eprintln!: the test harness captures it, and the
                // capture is lost when the process exits.
                use std::io::Write;
                let _ = writeln!(
                    std::io::stderr(),
                    "{test}: no progress for 60 s of real time, a busy loop?"
                );
                std::process::exit(101);
            }
        });
        Watchdog(Some(tx))
    }

    impl Drop for Watchdog {
        fn drop(&mut self) {
            if let Some(tx) = self.0.take() {
                let _ = tx.send(());
            }
        }
    }

    /// How a scripted watch fails, as kube-rs reports each case.
    #[derive(Clone, Copy, Debug)]
    pub(crate) enum WatchFailure {
        /// The API server refuses the watch (403, 429, 5xx). kube-rs does
        /// not check a watch response's status: the open succeeds and the
        /// error is the stream's first item.
        Refused(u16),
        /// The watch opens and sends an error event (410 Gone, 500).
        ErrorEvent(u16),
        /// The watch opens and ends without an event.
        EndsEmpty,
        /// The watch delivers the current state and ends at once.
        Flaps,
        /// The watch opens and never sends anything.
        Silent,
        /// The request never gets a response.
        Hangs,
        /// The request fails: the API server cannot be reached.
        Unreachable,
    }

    fn api_error(code: u16) -> kube::Error {
        kube::Error::Api(Box::new(
            Status::failure("scripted", "Scripted").with_code(code),
        ))
    }

    pub(crate) fn error_event(code: u16) -> WatchEvent<PartialObjectMeta<Pod>> {
        WatchEvent::Error(Box::new(
            Status::failure("scripted", "Scripted").with_code(code),
        ))
    }

    /// A pod on a scripted API server. Every change gets the next
    /// resourceVersion. A GET returns the current state, and a watch starts
    /// with it, as a watch without a resourceVersion does, unless a lagging
    /// watch cache is scripted. Each live watch's sender is kept so the test
    /// can send events or end it.
    #[derive(Clone, Default)]
    pub(crate) struct Script {
        pod: Rc<RefCell<Option<PartialObjectMeta<Pod>>>>,
        rv: Rc<RefCell<u64>>,
        lag: Rc<RefCell<Option<PartialObjectMeta<Pod>>>>,
        unordered: Rc<RefCell<bool>>,
        failures: Rc<RefCell<VecDeque<WatchFailure>>>,
        every_watch_fails: Rc<RefCell<Option<WatchFailure>>>,
        watches: Rc<RefCell<Vec<EventTx>>>,
        silent: Rc<RefCell<Vec<EventTx>>>,
        opens: Rc<RefCell<usize>>,
        get_count: Rc<RefCell<usize>>,
        get_fails: Rc<RefCell<usize>>,
    }

    impl Script {
        pub fn with(pod: PartialObjectMeta<Pod>) -> Self {
            let script = Script::default();
            *script.rv.borrow_mut() = 10;
            script.set(pod);
            script
        }
        /// Change the pod on the server, without an event. Returns the pod
        /// with its new resourceVersion.
        pub fn set(&self, mut pod: PartialObjectMeta<Pod>) -> PartialObjectMeta<Pod> {
            let rv = {
                let mut rv = self.rv.borrow_mut();
                *rv += 1;
                *rv
            };
            pod.metadata.resource_version = Some(rv.to_string());
            *self.pod.borrow_mut() = Some(pod.clone());
            pod
        }
        /// The next watch starts with this older state: its API server's
        /// watch cache lags.
        pub fn lag_next_watch(&self, pod: PartialObjectMeta<Pod>) {
            *self.lag.borrow_mut() = Some(pod);
        }
        /// Watches start with a resourceVersion that is not an integer.
        pub fn unordered(&self) {
            *self.unordered.borrow_mut() = true;
        }
        /// The next watch opened fails this way.
        pub fn fail_watch(&self, f: WatchFailure) {
            self.failures.borrow_mut().push_back(f);
        }
        /// Every watch opened fails this way.
        pub fn fail_every_watch(&self, f: WatchFailure) {
            *self.every_watch_fails.borrow_mut() = Some(f);
        }
        pub fn fail_gets(&self, n: usize) {
            *self.get_fails.borrow_mut() = n;
        }
        /// The sender of the latest live watch.
        pub fn current_watch(&self) -> Option<EventTx> {
            self.watches.borrow().last().cloned()
        }
        /// The server ends the latest live watch.
        pub fn end_watch(&self) {
            if let Some(tx) = self.watches.borrow().last() {
                tx.close_channel();
            }
        }
        /// Live watches: opened, and started with the pod's state.
        pub fn watch_count(&self) -> usize {
            self.watches.borrow().len()
        }
        /// Watch requests, failed ones included.
        pub fn opens(&self) -> usize {
            *self.opens.borrow()
        }
        pub fn gets(&self) -> usize {
            *self.get_count.borrow()
        }
    }

    impl PodSource for Script {
        async fn get(&self) -> Result<PartialObjectMeta<Pod>, kube::Error> {
            *self.get_count.borrow_mut() += 1;
            assert!(self.gets() < 10_000, "GET repeated without bound");
            {
                let mut fails = self.get_fails.borrow_mut();
                if *fails > 0 {
                    *fails -= 1;
                    return Err(api_error(503));
                }
            }
            self.pod.borrow().clone().ok_or_else(|| api_error(404))
        }

        async fn watch(&self) -> Result<MetaStream, kube::Error> {
            *self.opens.borrow_mut() += 1;
            // A regression that reopens without bound fails, not hangs.
            assert!(self.opens() < 10_000, "watch reopened without bound");
            let failure = self
                .failures
                .borrow_mut()
                .pop_front()
                .or(*self.every_watch_fails.borrow());
            let (tx, rx) = mpsc::unbounded();
            let mut start = self
                .lag
                .borrow_mut()
                .take()
                .or_else(|| self.pod.borrow().clone());
            if *self.unordered.borrow() {
                if let Some(pod) = start.as_mut() {
                    pod.metadata.resource_version = Some("not-a-number".to_string());
                }
            }
            match failure {
                Some(WatchFailure::Unreachable) => {
                    return Err(kube::Error::Service("connection refused".into()))
                }
                Some(WatchFailure::Hangs) => std::future::pending::<()>().await,
                Some(WatchFailure::Refused(code)) => {
                    tx.unbounded_send(Err(api_error(code))).unwrap()
                }
                Some(WatchFailure::ErrorEvent(code)) => {
                    tx.unbounded_send(Ok(error_event(code))).unwrap()
                }
                Some(WatchFailure::EndsEmpty) => {}
                Some(WatchFailure::Flaps) => {
                    if let Some(pod) = start {
                        tx.unbounded_send(Ok(WatchEvent::Added(pod))).unwrap();
                    }
                }
                Some(WatchFailure::Silent) => self.silent.borrow_mut().push(tx.clone()),
                None => {
                    if let Some(pod) = start {
                        tx.unbounded_send(Ok(WatchEvent::Added(pod))).unwrap();
                    }
                    self.watches.borrow_mut().push(tx);
                    return Ok(rx.boxed_local());
                }
            }
            drop(tx);
            Ok(rx.boxed_local())
        }
    }

    pub(crate) fn pod(uid: &str, annotations: &[(&str, &str)]) -> PartialObjectMeta<Pod> {
        PartialObjectMeta {
            types: None,
            metadata: ObjectMeta {
                uid: Some(uid.to_string()),
                annotations: Some(
                    annotations
                        .iter()
                        .map(|(k, v)| (k.to_string(), v.to_string()))
                        .collect(),
                ),
                ..Default::default()
            },
            _phantom: Default::default(),
        }
    }

    /// Send `event` on the latest live watch, once one is open. Fails, rather
    /// than hangs, when none opens.
    pub(crate) async fn send(script: &Script, event: WatchEvent<PartialObjectMeta<Pod>>) {
        for _ in 0..10_000 {
            if let Some(tx) = script.current_watch() {
                if !tx.is_closed() {
                    tx.unbounded_send(Ok(event)).unwrap();
                    return;
                }
            }
            tokio::task::yield_now().await;
        }
        panic!("no watch was open");
    }

    /// The pod changes on the server and the watch delivers the change.
    /// Returns the pod with its new resourceVersion.
    pub(crate) async fn change(
        script: &Script,
        pod: PartialObjectMeta<Pod>,
    ) -> PartialObjectMeta<Pod> {
        let pod = script.set(pod);
        send(script, WatchEvent::Modified(pod.clone())).await;
        pod
    }

    /// A feed with exact intervals.
    pub(crate) fn exact_feed(script: &Script) -> PodFeed<Script> {
        PodFeed::new(script.clone()).without_jitter()
    }

    async fn next_within(feed: &mut PodFeed<Script>, d: Duration) -> Option<PodSnapshot> {
        tokio::time::timeout(d, feed.next()).await.ok()
    }

    /// Snapshot times, from the start, of a feed run for `d`.
    async fn run_for(feed: &mut PodFeed<Script>, d: Duration) -> Vec<Duration> {
        let start = Instant::now();
        let end = start + d;
        let mut at = vec![];
        while Instant::now() < end {
            if next_within(feed, end - Instant::now()).await.is_some() {
                at.push(Instant::now() - start);
                assert!(at.len() < 10_000, "snapshots without bound");
            }
        }
        at
    }

    fn a(snap: &PodSnapshot) -> Option<&str> {
        snap.annotations.get("a").map(String::as_str)
    }

    // The first snapshot is a GET; the watch then opens, and a change
    // arrives without any time passing. The watch's own start repeats the
    // GET's state and is not handed out again.
    #[tokio::test(start_paused = true)]
    async fn starts_with_a_get_then_watches() {
        let _w = watchdog("starts_with_a_get_then_watches");
        let script = Script::with(pod("u1", &[]));
        let mut feed = exact_feed(&script);
        assert_eq!(feed.next().await.uid.as_deref(), Some("u1"));
        assert_eq!((script.gets(), script.opens()), (1, 0));
        let started = Instant::now();
        let (snap, _) = tokio::join!(feed.next(), change(&script, pod("u1", &[("a", "1")])));
        assert_eq!(a(&snap), Some("1"));
        assert_eq!(Instant::now(), started, "no poll interval in the path");
        assert!(feed.watching());
        assert_eq!((script.gets(), script.opens()), (1, 1));
    }

    // The server ends a watch that worked: it is reopened at once, and the
    // new one starts from the current state, a change the old one never
    // delivered included.
    #[tokio::test(start_paused = true)]
    async fn an_ended_watch_is_reopened_from_the_current_state() {
        let _w = watchdog("an_ended_watch_is_reopened_from_the_current_state");
        let script = Script::with(pod("u1", &[]));
        let mut feed = exact_feed(&script);
        feed.next().await;
        assert!(next_within(&mut feed, MIN_STREAM_LIFETIME).await.is_none());
        assert!(feed.watching());
        script.set(pod("u1", &[("a", "2")]));
        script.end_watch();
        let t0 = Instant::now();
        assert_eq!(a(&feed.next().await), Some("2"));
        assert_eq!(Instant::now(), t0, "reopened at once");
        assert_eq!((script.gets(), script.opens()), (1, 2));
    }

    // Every way a watch fails falls back to a GET every POLL_INTERVAL, and
    // the watch is retried with a backoff: the requests stay bounded however
    // often the failure repeats. A GET that keeps returning the same state
    // while every watch sends 410 Gone is one of them. A GET that finds
    // nothing new hands out nothing.
    #[tokio::test(start_paused = true)]
    async fn every_watch_failure_polls_and_backs_off() {
        let _w = watchdog("every_watch_failure_polls_and_backs_off");
        for failure in [
            WatchFailure::Refused(403),
            WatchFailure::Refused(429),
            WatchFailure::ErrorEvent(410),
            WatchFailure::ErrorEvent(500),
            WatchFailure::EndsEmpty,
            WatchFailure::Unreachable,
        ] {
            let script = Script::with(pod("u1", &[]));
            script.fail_every_watch(failure);
            let mut feed = exact_feed(&script);
            let at = run_for(&mut feed, Duration::from_secs(59)).await;
            assert_eq!(at, vec![Duration::ZERO], "{failure:?}: the first GET only");
            assert_eq!(script.gets(), 30, "{failure:?}: a GET every POLL_INTERVAL");
            // Opened at 0, 1, 3, 7, 15 and 31 s; next at 63 s.
            assert_eq!(script.opens(), 6, "{failure:?}: retries back off");
            assert!(!feed.watching());
        }
    }

    // While polling, a change is found by the next GET.
    #[tokio::test(start_paused = true)]
    async fn polling_finds_a_change_within_the_interval() {
        let _w = watchdog("polling_finds_a_change_within_the_interval");
        let script = Script::with(pod("u1", &[]));
        script.fail_every_watch(WatchFailure::Refused(403));
        let mut feed = exact_feed(&script);
        feed.next().await;
        script.set(pod("u1", &[("a", "1")]));
        let t0 = Instant::now();
        assert_eq!(a(&feed.next().await), Some("1"));
        assert_eq!(Instant::now() - t0, POLL_INTERVAL);
    }

    // A watch that delivers and then ends at once worked for no time: it is
    // a failure, so the reopens back off instead of repeating.
    #[tokio::test(start_paused = true)]
    async fn short_lived_watches_back_off() {
        let _w = watchdog("short_lived_watches_back_off");
        let script = Script::with(pod("u1", &[]));
        script.fail_every_watch(WatchFailure::Flaps);
        let mut feed = exact_feed(&script);
        run_for(&mut feed, Duration::from_secs(59)).await;
        assert_eq!(script.opens(), 6);
    }

    // A watch that sends nothing fails after FIRST_EVENT_TIMEOUT, and the
    // feed polls meanwhile.
    #[tokio::test(start_paused = true)]
    async fn a_silent_watch_fails_after_the_first_event_timeout() {
        let _w = watchdog("a_silent_watch_fails_after_the_first_event_timeout");
        let script = Script::with(pod("u1", &[]));
        script.fail_watch(WatchFailure::Silent);
        let mut feed = exact_feed(&script);
        let start = Instant::now();
        feed.next().await;
        script.set(pod("u1", &[("a", "1")]));
        assert_eq!(a(&feed.next().await), Some("1"));
        assert_eq!(Instant::now() - start, FIRST_EVENT_TIMEOUT, "a GET then");
        assert!(next_within(&mut feed, Duration::from_secs(2))
            .await
            .is_none());
        assert!(feed.watching(), "the retried watch works");
        let (snap, _) = tokio::join!(feed.next(), change(&script, pod("u1", &[("a", "2")])));
        assert_eq!(a(&snap), Some("2"));
    }

    // Watches that never send anything are failures like any other: they
    // back off, and the feed keeps polling between them.
    #[tokio::test(start_paused = true)]
    async fn silent_watches_back_off_and_the_feed_polls() {
        let _w = watchdog("silent_watches_back_off_and_the_feed_polls");
        let script = Script::with(pod("u1", &[]));
        script.fail_every_watch(WatchFailure::Silent);
        let mut feed = exact_feed(&script);
        run_for(&mut feed, Duration::from_secs(59)).await;
        // Opened at 0, 6, 13, 22, 35 and 56 s, each given up after
        // FIRST_EVENT_TIMEOUT.
        assert_eq!(script.opens(), 6);
        assert!(script.gets() >= 15, "polled meanwhile: {}", script.gets());
    }

    // A request that never gets a response times out, and the feed polls
    // between the attempts.
    #[tokio::test(start_paused = true)]
    async fn a_hung_request_times_out() {
        let _w = watchdog("a_hung_request_times_out");
        let script = Script::with(pod("u1", &[]));
        script.fail_every_watch(WatchFailure::Hangs);
        let mut feed = exact_feed(&script);
        run_for(&mut feed, Duration::from_secs(30)).await;
        // Opened at 0, 6, 13 and 22 s, each given up after REQUEST_TIMEOUT.
        assert_eq!(script.opens(), 4);
        assert!(script.gets() >= 6, "polled meanwhile: {}", script.gets());
    }

    // After failures the watch is restored, and the next change is
    // delivered at once.
    #[tokio::test(start_paused = true)]
    async fn the_watch_is_restored_after_failures() {
        let _w = watchdog("the_watch_is_restored_after_failures");
        let script = Script::with(pod("u1", &[]));
        for _ in 0..3 {
            script.fail_watch(WatchFailure::Refused(403));
        }
        let mut feed = exact_feed(&script);
        feed.next().await;
        // Opened at 0, 1 and 3 s, failing; at 7 s it works.
        assert!(next_within(&mut feed, Duration::from_secs(8))
            .await
            .is_none());
        assert!(feed.watching());
        assert_eq!(script.opens(), 4);
        let t0 = Instant::now();
        let (snap, _) = tokio::join!(feed.next(), change(&script, pod("u1", &[("a", "1")])));
        assert_eq!(a(&snap), Some("1"));
        assert_eq!(Instant::now(), t0);
    }

    // Every RESYNC_INTERVAL a new watch replaces the old one and starts from
    // the current state; the old stream is dropped, so an event left in it
    // is never delivered after the newer snapshot.
    #[tokio::test(start_paused = true)]
    async fn resync_replaces_the_watch_so_no_older_event_follows() {
        let _w = watchdog("resync_replaces_the_watch_so_no_older_event_follows");
        let script = Script::with(pod("u1", &[]));
        let mut feed = exact_feed(&script);
        feed.next().await;
        assert!(next_within(&mut feed, Duration::from_millis(1))
            .await
            .is_none());
        let old = script.current_watch().unwrap();
        script.set(pod("u1", &[("a", "new")]));
        let snap = next_within(&mut feed, RESYNC_INTERVAL + Duration::from_secs(1))
            .await
            .expect("the resync's new watch");
        assert_eq!(a(&snap), Some("new"));
        assert!(
            old.unbounded_send(Ok(WatchEvent::Modified(pod("u1", &[("a", "old")]))))
                .is_err(),
            "the old stream is gone"
        );
        assert_eq!((script.gets(), script.opens()), (1, 2));
    }

    // refresh(): the next snapshot is the pod as it is after the call,
    // watched or polled, changed or not, without waiting for a change or a
    // poll.
    #[tokio::test(start_paused = true)]
    async fn refresh_reads_the_current_state() {
        let _w = watchdog("refresh_reads_the_current_state");
        let script = Script::with(pod("u1", &[]));
        let mut feed = exact_feed(&script);
        feed.next().await;
        assert!(next_within(&mut feed, Duration::from_millis(1))
            .await
            .is_none());
        script.set(pod("u1", &[("a", "1")]));
        feed.refresh();
        let t0 = Instant::now();
        assert_eq!(a(&feed.next().await), Some("1"));
        assert_eq!(Instant::now(), t0);
        assert_eq!(script.opens(), 2, "a new watch, not a GET");
        feed.refresh();
        assert_eq!(a(&feed.next().await), Some("1"), "unchanged, still a pass");
        assert_eq!(Instant::now(), t0);

        let script = Script::with(pod("u1", &[]));
        script.fail_every_watch(WatchFailure::Refused(403));
        let mut feed = exact_feed(&script);
        feed.next().await;
        script.set(pod("u1", &[("a", "2")]));
        feed.refresh();
        let t0 = Instant::now();
        assert_eq!(a(&feed.next().await), Some("2"));
        assert_eq!(Instant::now(), t0, "polling: a GET at once");
    }

    // A new watch whose cache lags starts with an older state than one
    // already handed out: it is dropped, and the state is handed out once
    // the watch brings it.
    #[tokio::test(start_paused = true)]
    async fn a_lagging_watch_never_hands_out_an_older_state() {
        let _w = watchdog("a_lagging_watch_never_hands_out_an_older_state");
        let script = Script::with(pod("u1", &[]));
        let mut feed = exact_feed(&script);
        feed.next().await;
        let (_, older) = tokio::join!(feed.next(), change(&script, pod("u1", &[("a", "1")])));
        let (snap, newer) = tokio::join!(feed.next(), change(&script, pod("u1", &[("a", "2")])));
        assert_eq!(a(&snap), Some("2"));
        script.lag_next_watch(older);
        feed.refresh();
        assert!(
            next_within(&mut feed, Duration::from_millis(1))
                .await
                .is_none(),
            "the older state is dropped"
        );
        assert_eq!(script.opens(), 2);
        // The lagging cache catches up.
        send(&script, WatchEvent::Modified(newer)).await;
        assert_eq!(a(&feed.next().await), Some("2"));
    }

    // A resourceVersion that is not an integer cannot be ordered: the feed
    // stops watching and polls.
    #[tokio::test(start_paused = true)]
    async fn an_unordered_resource_version_stops_the_watch() {
        let _w = watchdog("an_unordered_resource_version_stops_the_watch");
        let script = Script::with(pod("u1", &[]));
        script.unordered();
        let mut feed = exact_feed(&script);
        feed.next().await;
        script.set(pod("u1", &[("a", "1")]));
        assert_eq!(a(&feed.next().await), Some("1"), "found by a GET");
        run_for(&mut feed, Duration::from_secs(20)).await;
        assert_eq!(script.opens(), 1, "never watched again");
        assert!(script.gets() >= 10);
    }

    // A failed GET (API server unreachable) is retried at the next poll.
    #[tokio::test(start_paused = true)]
    async fn a_failed_get_is_retried_at_the_next_poll() {
        let _w = watchdog("a_failed_get_is_retried_at_the_next_poll");
        let script = Script::with(pod("u1", &[]));
        script.fail_every_watch(WatchFailure::Unreachable);
        script.fail_gets(1);
        let mut feed = exact_feed(&script);
        let start = Instant::now();
        assert_eq!(feed.next().await.uid.as_deref(), Some("u1"));
        assert_eq!(Instant::now() - start, POLL_INTERVAL);
    }

    #[test]
    fn resource_versions_compare_as_numbers() {
        assert!(is_resource_version("1"));
        assert!(is_resource_version("18446744073709551616000"));
        for bad in ["", "0", "012", "1a", "-1", " 1"] {
            assert!(!is_resource_version(bad), "{bad:?}");
        }
        assert_eq!(compare_resource_versions("9", "10"), Ordering::Less);
        assert_eq!(compare_resource_versions("100", "99"), Ordering::Greater);
        assert_eq!(compare_resource_versions("42", "42"), Ordering::Equal);
        assert_eq!(
            compare_resource_versions("18446744073709551616", "18446744073709551615"),
            Ordering::Greater
        );
    }

    #[test]
    fn jitter_shortens_by_at_most_a_fifth() {
        let mut feed = PodFeed::new(Script::default());
        let mut seen = std::collections::BTreeSet::new();
        for _ in 0..1000 {
            let d = feed.jittered(RESYNC_INTERVAL);
            assert!(
                d <= RESYNC_INTERVAL && d >= RESYNC_INTERVAL.mul_f64(0.8),
                "{d:?}"
            );
            seen.insert(d.as_millis());
        }
        assert!(seen.len() > 100, "spread: {}", seen.len());
        let mut fixed = PodFeed::new(Script::default()).without_jitter();
        assert_eq!(fixed.jittered(RESYNC_INTERVAL), RESYNC_INTERVAL);
    }

    #[test]
    fn backoff_doubles_up_to_the_cap() {
        assert_eq!(watch_backoff(1), Duration::from_secs(1));
        assert_eq!(watch_backoff(2), Duration::from_secs(2));
        assert_eq!(watch_backoff(6), Duration::from_secs(32));
        assert_eq!(watch_backoff(7), WATCH_RETRY_MAX);
        assert_eq!(watch_backoff(100), WATCH_RETRY_MAX);
    }
}

//! The action loop's view of its own launcher pod.
//!
//! The controller drives an action by writing annotations on the launcher
//! pod. [`PodFeed`] hands the action loop a snapshot of the pod's annotations
//! every time they may have changed:
//!
//!   - from a watch on the pod's metadata, as soon as the API server delivers
//!     the change, so an action is seen without waiting for a poll;
//!   - from a GET at start, after the watch's resourceVersion expired
//!     (410 Gone), after a watch error, and every [`RESYNC_INTERVAL`] as a
//!     safety net.
//!
//! The pod's current state is the source of truth. An event only says when to
//! look again: the loop decides from the whole snapshot, and its decisions are
//! idempotent by action id, so a repeated or a missed event changes nothing a
//! later snapshot does not correct.
//!
//! Snapshots are monotonic. A watch delivers one object's events in order, and
//! every GET drops the stream and opens a new one from the GET's
//! resourceVersion, so an event older than a snapshot already handed out is
//! never delivered after it.
//!
//! When the watch cannot be opened (a launcher Role from before swiftletd
//! watched its pod grants no `watch`, or the API server is unreachable), the
//! feed polls every [`POLL_INTERVAL`], as swiftletd always did, and retries the
//! watch with a backoff of up to [`WATCH_RETRY_MAX`].

use std::collections::BTreeMap;
use std::time::Duration;

use futures_util::stream::{LocalBoxStream, StreamExt};
use k8s_openapi::api::core::v1::Pod;
use kube::api::{Api, WatchEvent, WatchParams};
use kube::core::PartialObjectMeta;
use tokio::time::Instant;

/// Polling cadence when no watch is open. The action loop's only cadence
/// before it watched its pod.
pub const POLL_INTERVAL: Duration = Duration::from_secs(2);

/// How often a GET replaces the watch's view, in case an event was lost
/// without the watch reporting an error.
pub const RESYNC_INTERVAL: Duration = Duration::from_secs(30);

/// Longest wait between attempts to open the watch.
pub const WATCH_RETRY_MAX: Duration = Duration::from_secs(60);

/// Server-side timeout of one watch request. The feed reopens the watch from
/// the last resourceVersion when the server ends it.
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

/// Reads one pod: a GET of its metadata, and a watch of its metadata from a
/// resourceVersion. The API server, or a script in tests.
#[allow(async_fn_in_trait)]
pub trait PodSource {
    async fn get(&self) -> Result<PartialObjectMeta<Pod>, kube::Error>;
    async fn watch(&self, resource_version: &str) -> Result<MetaStream, kube::Error>;
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

    async fn watch(&self, resource_version: &str) -> Result<MetaStream, kube::Error> {
        Ok(self
            .api
            .watch_metadata(&self.params, resource_version)
            .await?
            .boxed_local())
    }
}

/// Hands out pod snapshots: see the module doc.
pub struct PodFeed<S: PodSource> {
    source: S,
    stream: Option<MetaStream>,
    /// The resourceVersion of the last snapshot or bookmark: where a reopened
    /// watch resumes.
    resource_version: Option<String>,
    /// A GET is due: at start, after 410 Gone or a watch error, on resync, and
    /// on every poll while no watch is open.
    need_get: bool,
    next_resync: Instant,
    /// Next poll while no watch is open.
    next_poll: Instant,
    /// When the watch may be opened again after a failure.
    watch_retry_at: Instant,
    watch_failures: u32,
}

impl<S: PodSource> PodFeed<S> {
    pub fn new(source: S) -> Self {
        let now = Instant::now();
        PodFeed {
            source,
            stream: None,
            resource_version: None,
            need_get: true,
            next_resync: now + RESYNC_INTERVAL,
            next_poll: now,
            watch_retry_at: now,
            watch_failures: 0,
        }
    }

    /// Whether a watch is open: the loop is event-driven rather than polling.
    #[cfg(test)]
    pub fn watching(&self) -> bool {
        self.stream.is_some()
    }

    /// The next snapshot of the pod. Cancel-safe: every await either resumes
    /// from state kept in `self` or is repeated by the next call, so the loop
    /// may drop this future to handle something else.
    pub async fn next(&mut self) -> PodSnapshot {
        loop {
            if self.need_get {
                match self.source.get().await {
                    Ok(meta) => {
                        let now = Instant::now();
                        self.need_get = false;
                        self.resource_version = meta.metadata.resource_version.clone();
                        self.next_resync = now + RESYNC_INTERVAL;
                        self.next_poll = now + POLL_INTERVAL;
                        return PodSnapshot::of(&meta);
                    }
                    Err(e) => {
                        log::warn!("action_loop_get_pod_err: {}", e);
                        self.next_poll = Instant::now() + POLL_INTERVAL;
                    }
                }
            }
            if self.stream.is_none() && !self.need_get && Instant::now() >= self.watch_retry_at {
                if let Some(rv) = self.resource_version.clone() {
                    match self.source.watch(&rv).await {
                        Ok(stream) => {
                            if self.watch_failures > 0 {
                                log::info!("action_loop_watch_restored");
                            }
                            log::debug!("action_loop_watch_opened resource_version={}", rv);
                            self.watch_failures = 0;
                            self.stream = Some(stream);
                        }
                        Err(e) => self.watch_failed(&e.to_string(), is_gone(&e)),
                    }
                }
            }
            match self.stream.as_mut() {
                Some(stream) => {
                    tokio::select! {
                        item = stream.next() => match item {
                            Some(Ok(WatchEvent::Added(meta))) | Some(Ok(WatchEvent::Modified(meta))) => {
                                self.resource_version = meta.metadata.resource_version.clone();
                                return PodSnapshot::of(&meta);
                            }
                            Some(Ok(WatchEvent::Deleted(meta))) => {
                                // The kubelet ends this process with its pod. A pod
                                // created under the same name later arrives as Added.
                                self.resource_version = meta.metadata.resource_version.clone();
                                log::debug!("action_loop_pod_deleted");
                            }
                            Some(Ok(WatchEvent::Bookmark(bookmark))) => {
                                self.resource_version = Some(bookmark.metadata.resource_version);
                            }
                            Some(Ok(WatchEvent::Error(status))) => {
                                self.stream = None;
                                self.need_get = true;
                                let gone = status.code == 410;
                                if !gone {
                                    self.watch_failed(&status.message, false);
                                }
                            }
                            Some(Err(e)) => {
                                self.stream = None;
                                self.need_get = true;
                                self.watch_failed(&e.to_string(), false);
                            }
                            // The server ended the watch (its timeout): reopen from
                            // the last resourceVersion, no GET needed.
                            None => {
                                self.stream = None;
                                log::debug!("action_loop_watch_ended");
                            }
                        },
                        _ = tokio::time::sleep_until(self.next_resync) => {
                            log::debug!("action_loop_resync");
                            self.stream = None;
                            self.need_get = true;
                        }
                    }
                }
                None => {
                    // No watch: poll, as before, until the watch can be opened.
                    let wake = if self.need_get || self.resource_version.is_none() {
                        self.next_poll
                    } else {
                        self.next_poll.min(self.watch_retry_at)
                    };
                    tokio::time::sleep_until(wake).await;
                    if Instant::now() >= self.next_poll {
                        self.need_get = true;
                    }
                }
            }
        }
    }

    /// A watch could not be opened or broke. Poll meanwhile, and retry with a
    /// backoff (none for 410 Gone, which only needs a fresh resourceVersion).
    fn watch_failed(&mut self, err: &str, gone: bool) {
        let now = Instant::now();
        if gone {
            self.need_get = true;
            self.watch_retry_at = now;
            return;
        }
        self.watch_failures = self.watch_failures.saturating_add(1);
        let delay = watch_backoff(self.watch_failures);
        self.watch_retry_at = now + delay;
        self.next_poll = self.next_poll.min(now + POLL_INTERVAL);
        if self.watch_failures == 1 {
            log::warn!(
                "action_loop_watch_unavailable: {}; polling every {:?}, retrying the watch in {:?}",
                err,
                POLL_INTERVAL,
                delay
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
}

/// 1s, 2s, 4s, ... up to [`WATCH_RETRY_MAX`].
fn watch_backoff(failures: u32) -> Duration {
    let secs = 1u64 << failures.saturating_sub(1).min(6);
    Duration::from_secs(secs).min(WATCH_RETRY_MAX)
}

/// A watch from a resourceVersion the server no longer has.
fn is_gone(e: &kube::Error) -> bool {
    matches!(e, kube::Error::Api(status) if status.code == 410)
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

    /// What a scripted GET or watch open returns.
    pub(crate) enum Reply<T> {
        Ok(T),
        Status(u16),
    }

    fn api_error(code: u16) -> kube::Error {
        kube::Error::Api(Box::new(
            Status::failure("scripted", "Scripted").with_code(code),
        ))
    }

    /// A pod source driven by the test: GETs return queued pods (the last one
    /// repeats), each watch open returns the next queued reply, and every
    /// opened watch's sender is kept so the test can push events or end it.
    #[derive(Clone, Default)]
    pub(crate) struct Script {
        pub gets: Rc<RefCell<VecDeque<Reply<PartialObjectMeta<Pod>>>>>,
        pub last_get: Rc<RefCell<Option<PartialObjectMeta<Pod>>>>,
        pub watch_replies: Rc<RefCell<VecDeque<Reply<()>>>>,
        pub watches: Rc<RefCell<Vec<(String, EventTx)>>>,
        pub get_count: Rc<RefCell<usize>>,
    }

    impl Script {
        pub fn get(&self, pod: PartialObjectMeta<Pod>) {
            self.gets.borrow_mut().push_back(Reply::Ok(pod));
        }
        pub fn get_fails(&self, code: u16) {
            self.gets.borrow_mut().push_back(Reply::Status(code));
        }
        pub fn watch_fails(&self, code: u16) {
            self.watch_replies
                .borrow_mut()
                .push_back(Reply::Status(code));
        }
        /// The sender of the latest watch opened, and the resourceVersion it
        /// was opened from.
        pub fn current_watch(&self) -> Option<(String, EventTx)> {
            self.watches.borrow().last().cloned()
        }
        pub fn watch_count(&self) -> usize {
            self.watches.borrow().len()
        }
        pub fn gets(&self) -> usize {
            *self.get_count.borrow()
        }
    }

    impl PodSource for Script {
        async fn get(&self) -> Result<PartialObjectMeta<Pod>, kube::Error> {
            *self.get_count.borrow_mut() += 1;
            let next = self.gets.borrow_mut().pop_front();
            match next {
                Some(Reply::Ok(pod)) => {
                    *self.last_get.borrow_mut() = Some(pod.clone());
                    Ok(pod)
                }
                Some(Reply::Status(code)) => Err(api_error(code)),
                None => self.last_get.borrow().clone().ok_or_else(|| api_error(404)),
            }
        }

        async fn watch(&self, resource_version: &str) -> Result<MetaStream, kube::Error> {
            if let Some(Reply::Status(code)) = self.watch_replies.borrow_mut().pop_front() {
                return Err(api_error(code));
            }
            let (tx, rx) = mpsc::unbounded();
            self.watches
                .borrow_mut()
                .push((resource_version.to_string(), tx));
            Ok(rx.boxed_local())
        }
    }

    pub(crate) fn pod(uid: &str, rv: &str, annotations: &[(&str, &str)]) -> PartialObjectMeta<Pod> {
        PartialObjectMeta {
            types: None,
            metadata: ObjectMeta {
                uid: Some(uid.to_string()),
                resource_version: Some(rv.to_string()),
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

    pub(crate) fn gone() -> WatchEvent<PartialObjectMeta<Pod>> {
        WatchEvent::Error(Box::new(
            Status::failure("too old resource version", "Expired").with_code(410),
        ))
    }

    async fn next_within(feed: &mut PodFeed<Script>, d: Duration) -> Option<PodSnapshot> {
        tokio::time::timeout(d, feed.next()).await.ok()
    }

    /// Push `event` into the watch as soon as one is open. Fails, rather than
    /// hangs, when none opens.
    pub(crate) async fn push_when_open(script: &Script, event: WatchEvent<PartialObjectMeta<Pod>>) {
        for _ in 0..10_000 {
            if let Some((_, tx)) = script.current_watch() {
                tx.unbounded_send(Ok(event)).unwrap();
                return;
            }
            tokio::task::yield_now().await;
        }
        panic!("no watch was opened");
    }

    // The first snapshot is a GET; the watch then opens from its
    // resourceVersion, and a change arrives without any time passing.
    #[tokio::test(start_paused = true)]
    async fn a_watched_change_is_delivered_at_once() {
        let script = Script::default();
        script.get(pod("u1", "10", &[]));
        let mut feed = PodFeed::new(script.clone());
        assert_eq!(feed.next().await.uid.as_deref(), Some("u1"));
        let started = Instant::now();
        let (snap, ()) = tokio::join!(
            feed.next(),
            push_when_open(
                &script,
                WatchEvent::Modified(pod("u1", "11", &[("a", "1")]))
            )
        );
        assert_eq!(snap.annotations.get("a").map(String::as_str), Some("1"));
        assert_eq!(Instant::now(), started, "no poll interval in the path");
        assert_eq!(
            script.current_watch().unwrap().0,
            "10",
            "watch opened from the GET's resourceVersion"
        );
        assert!(feed.watching());
        assert_eq!(script.gets(), 1);
    }

    // A watch the server ends is reopened from the last resourceVersion
    // (a bookmark's included), without a GET.
    #[tokio::test(start_paused = true)]
    async fn an_ended_watch_resumes_from_the_last_resource_version() {
        let script = Script::default();
        script.get(pod("u1", "10", &[]));
        let mut feed = PodFeed::new(script.clone());
        feed.next().await;
        assert!(next_within(&mut feed, Duration::from_millis(1))
            .await
            .is_none());
        let (_, tx) = script.current_watch().unwrap();
        tx.unbounded_send(Ok(WatchEvent::Modified(pod("u1", "12", &[]))))
            .unwrap();
        feed.next().await;
        tx.unbounded_send(Ok(WatchEvent::Bookmark(
            serde_json::from_value(serde_json::json!({
                "apiVersion": "v1", "kind": "Pod", "metadata": {"resourceVersion": "15"}
            }))
            .unwrap(),
        )))
        .unwrap();
        drop(tx);
        script.watches.borrow_mut().clear();
        assert!(next_within(&mut feed, Duration::from_millis(1))
            .await
            .is_none());
        let (rv, tx2) = script.current_watch().expect("reopened");
        assert_eq!(rv, "15");
        assert_eq!(script.gets(), 1, "a normal end needs no GET");
        tx2.unbounded_send(Ok(WatchEvent::Modified(pod("u1", "16", &[("b", "2")]))))
            .unwrap();
        assert_eq!(
            feed.next().await.annotations.get("b").map(String::as_str),
            Some("2")
        );
    }

    // 410 Gone: the resourceVersion is too old; a GET gives the current state
    // and a new one, and the watch reopens from it.
    #[tokio::test(start_paused = true)]
    async fn gone_relists_and_reopens_from_the_new_resource_version() {
        let script = Script::default();
        script.get(pod("u1", "10", &[]));
        let mut feed = PodFeed::new(script.clone());
        feed.next().await;
        assert!(next_within(&mut feed, Duration::from_millis(1))
            .await
            .is_none());
        let (_, tx) = script.current_watch().unwrap();
        script.get(pod("u1", "40", &[("c", "3")]));
        tx.unbounded_send(Ok(gone())).unwrap();
        let t0 = Instant::now();
        let snap = feed.next().await;
        assert_eq!(snap.annotations.get("c").map(String::as_str), Some("3"));
        assert_eq!(
            Instant::now(),
            t0,
            "relisted at once, not after a poll interval"
        );
        assert_eq!(script.gets(), 2);
        assert!(next_within(&mut feed, Duration::from_millis(1))
            .await
            .is_none());
        assert_eq!(script.current_watch().unwrap().0, "40");
    }

    // A watch that cannot be opened (a Role without watch: 403) falls back to
    // polling every POLL_INTERVAL and retries the watch with a backoff.
    #[tokio::test(start_paused = true)]
    async fn without_a_watch_it_polls_as_before_and_retries() {
        let script = Script::default();
        script.get(pod("u1", "10", &[]));
        for _ in 0..3 {
            script.watch_fails(403);
        }
        let mut feed = PodFeed::new(script.clone());
        feed.next().await;
        let start = Instant::now();
        let mut polls = vec![];
        while !feed.watching() && Instant::now() - start < Duration::from_secs(20) {
            if next_within(&mut feed, Duration::from_secs(3))
                .await
                .is_some()
            {
                polls.push(Instant::now() - start);
            }
        }
        assert!(feed.watching(), "the watch is retried and restored");
        // Retries at 1s, 3s (failing) and 7s (opens); meanwhile a GET every
        // POLL_INTERVAL, as the loop always polled.
        assert_eq!(
            polls,
            vec![POLL_INTERVAL, 2 * POLL_INTERVAL, 3 * POLL_INTERVAL]
        );
        assert_eq!(script.watch_count(), 1);
    }

    // Every RESYNC_INTERVAL a GET replaces the watch; the old stream is
    // dropped, so an event pushed into it is never delivered after the newer
    // snapshot.
    #[tokio::test(start_paused = true)]
    async fn resync_replaces_the_stream_so_no_older_event_follows() {
        let script = Script::default();
        script.get(pod("u1", "10", &[]));
        let mut feed = PodFeed::new(script.clone());
        feed.next().await;
        assert!(next_within(&mut feed, Duration::from_millis(1))
            .await
            .is_none());
        let (_, old) = script.current_watch().unwrap();
        script.get(pod("u1", "50", &[("state", "new")]));
        let snap = tokio::time::timeout(RESYNC_INTERVAL + Duration::from_secs(1), feed.next())
            .await
            .expect("a resync GET");
        assert_eq!(
            snap.annotations.get("state").map(String::as_str),
            Some("new")
        );
        assert!(
            old.unbounded_send(Ok(WatchEvent::Modified(pod(
                "u1",
                "20",
                &[("state", "old")]
            ))))
            .is_err(),
            "the old stream is gone"
        );
        assert!(next_within(&mut feed, Duration::from_millis(1))
            .await
            .is_none());
        assert_eq!(script.current_watch().unwrap().0, "50");
    }

    // A failed GET (API server unreachable) is retried after POLL_INTERVAL.
    #[tokio::test(start_paused = true)]
    async fn a_failed_get_is_retried() {
        let script = Script::default();
        script.get_fails(503);
        script.get(pod("u1", "10", &[]));
        let mut feed = PodFeed::new(script.clone());
        let start = Instant::now();
        assert_eq!(feed.next().await.uid.as_deref(), Some("u1"));
        assert_eq!(Instant::now() - start, POLL_INTERVAL);
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

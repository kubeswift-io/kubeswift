//! Cloud Hypervisor's event monitor (`--event-monitor fd=<n>`).
//!
//! Cloud Hypervisor writes one JSON object per event to the monitor:
//! `{"timestamp":{"secs":..,"nanos":..},"source":..,"event":..,"properties":..}`,
//! pretty-printed, followed by a blank line. It is a stream, not a log:
//! nothing is replayed, an event carries no migration id, and the writer
//! never waits. The descriptor is set non-blocking and every write error is
//! ignored, so a full pipe loses events (possibly half of one), and the last
//! events before the process exits may never be written. Use events to learn
//! sooner; never to be correct.
//!
//! [`EventStream::pipe`] gives Cloud Hypervisor the write end of a pipe and
//! reads the other on a thread of its own. A pipe, not a file: a guest can
//! cause events (device resets, reboots) at any rate, and a file would grow
//! on the node for as long as the guest runs; a pipe holds 64 KiB and a
//! stalled reader only costs events. The reader keeps the migration and
//! pause/resume events, at most [`MAX_KEPT`] of them, and counts the rest.

use std::collections::{BTreeMap, VecDeque};
use std::fs::File;
use std::io::{self, Read};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

/// One Cloud Hypervisor event.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Event {
    /// Time since Cloud Hypervisor started.
    pub since_start: Duration,
    /// `vmm`, `vm`, `virtio-device`, ...
    pub source: String,
    /// `migration-started`, `paused`, ...
    pub event: String,
    pub properties: BTreeMap<String, String>,
}

#[derive(serde::Deserialize)]
struct RawTimestamp {
    secs: u64,
    nanos: u32,
}

#[derive(serde::Deserialize)]
struct RawEvent {
    timestamp: RawTimestamp,
    source: String,
    event: String,
    properties: Option<BTreeMap<String, String>>,
}

/// The longest line a well-formed event has is far under this; a longer one
/// is not an event and is skipped.
const MAX_LINE: usize = 4096;
/// The most lines one event spans.
const MAX_FRAME: usize = 16 * 1024;

/// Splits the monitor's byte stream into events.
///
/// Each event starts with a line that is exactly `{` and ends with one that
/// is exactly `}` (nested objects are indented). A torn write leaves a
/// partial object: the next `{` line starts over, so one lost write costs
/// that event and no more.
#[derive(Default, Debug)]
pub struct Framer {
    line: Vec<u8>,
    skipping_line: bool,
    frame: Option<Vec<u8>>,
    /// Objects that were torn, oversized or did not parse.
    pub dropped: u64,
}

impl Framer {
    /// Feed bytes; complete events are appended to `out`.
    pub fn push(&mut self, bytes: &[u8], out: &mut Vec<Event>) {
        for &b in bytes {
            if b != b'\n' {
                if self.skipping_line {
                    continue;
                }
                if self.line.len() >= MAX_LINE {
                    self.skipping_line = true;
                    self.line.clear();
                    if self.frame.take().is_some() {
                        self.dropped += 1;
                    }
                    continue;
                }
                self.line.push(b);
                continue;
            }
            if self.skipping_line {
                self.skipping_line = false;
                continue;
            }
            let line = std::mem::take(&mut self.line);
            self.end_line(line, out);
        }
    }

    fn end_line(&mut self, line: Vec<u8>, out: &mut Vec<Event>) {
        if line == b"{" {
            if self.frame.replace(b"{\n".to_vec()).is_some() {
                self.dropped += 1;
            }
            return;
        }
        let Some(frame) = self.frame.as_mut() else {
            return;
        };
        frame.extend_from_slice(&line);
        frame.push(b'\n');
        if frame.len() > MAX_FRAME {
            self.frame = None;
            self.dropped += 1;
            return;
        }
        if line != b"}" {
            return;
        }
        let frame = self.frame.take().unwrap_or_default();
        match serde_json::from_slice::<RawEvent>(&frame) {
            Ok(raw) => out.push(Event {
                since_start: Duration::new(
                    raw.timestamp.secs,
                    raw.timestamp.nanos.min(999_999_999),
                ),
                source: raw.source,
                event: raw.event,
                properties: raw.properties.unwrap_or_default(),
            }),
            Err(_) => self.dropped += 1,
        }
    }
}

/// How many kept events the stream holds before dropping the oldest.
pub const MAX_KEPT: usize = 256;

/// Whether the stream keeps an event: migrations and the pause/resume
/// transitions around them. Memory iterations are only counted.
fn kept(e: &Event) -> bool {
    e.source == "vm"
        && e.event != "migration-memory-iteration"
        && (e.event.starts_with("migration-")
            || matches!(
                e.event.as_str(),
                "pausing" | "paused" | "resuming" | "resumed"
            ))
}

#[derive(Default, Debug)]
struct Shared {
    /// Kept events with their sequence numbers, oldest first.
    events: VecDeque<(u64, Event)>,
    /// Sequence number the next kept event gets.
    next_seq: u64,
    /// Memory iterations seen.
    iterations: u64,
    /// Cloud Hypervisor closed its end (it exited), or the read failed.
    closed: bool,
    /// Objects that could not be read.
    dropped: u64,
}

/// What [`EventStream::wait_since`] returns.
#[derive(Debug, Default)]
pub struct Batch {
    /// Kept events after the cursor, oldest first.
    pub events: Vec<Event>,
    /// Where to continue from.
    pub cursor: u64,
    /// Events after the cursor were dropped before they were read: the
    /// batch is not the whole story.
    pub gap: bool,
    /// The stream has ended: Cloud Hypervisor exited.
    pub closed: bool,
    /// Memory iterations seen since Cloud Hypervisor started.
    pub iterations: u64,
}

/// The events of one Cloud Hypervisor process.
#[derive(Clone, Debug)]
pub struct EventStream {
    shared: Arc<(Mutex<Shared>, Condvar)>,
}

impl EventStream {
    /// Create the pipe and start reading it. Returns the stream and the
    /// write end to hand to Cloud Hypervisor (see `spawn_ch*`); drop the
    /// write end once the process is spawned, or the stream never ends.
    ///
    /// Both ends are close-on-exec: only the Cloud Hypervisor child gets the
    /// write end, unmarked between fork and exec.
    pub fn pipe() -> io::Result<(EventStream, OwnedFd)> {
        let mut fds = [0 as libc::c_int; 2];
        // SAFETY: fds is a valid two-element array.
        if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: pipe2 just returned these descriptors; each is owned once.
        let (read, write) = unsafe { (File::from_raw_fd(fds[0]), OwnedFd::from_raw_fd(fds[1])) };
        let stream = EventStream {
            shared: Arc::new((Mutex::new(Shared::default()), Condvar::new())),
        };
        let reader = stream.clone();
        std::thread::Builder::new()
            .name("ch-events".to_string())
            .spawn(move || reader.read_all(read))?;
        Ok((stream, write))
    }

    /// A stream fed by the caller instead of a pipe (tests).
    pub fn detached() -> EventStream {
        EventStream {
            shared: Arc::new((Mutex::new(Shared::default()), Condvar::new())),
        }
    }

    fn read_all(&self, mut read: impl Read) {
        let mut framer = Framer::default();
        let mut buf = [0u8; 8192];
        let mut out = Vec::new();
        loop {
            match read.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => {
                    framer.push(&buf[..n], &mut out);
                    self.record(out.drain(..), framer.dropped);
                }
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(_) => break,
            }
        }
        self.close();
    }

    /// Add events as the reader does (tests and the reader).
    pub fn record(&self, events: impl IntoIterator<Item = Event>, dropped: u64) {
        let (lock, cv) = &*self.shared;
        let mut s = lock.lock().unwrap_or_else(|p| p.into_inner());
        for e in events {
            if e.event == "migration-memory-iteration" {
                s.iterations += 1;
            }
            if !kept(&e) {
                continue;
            }
            let seq = s.next_seq;
            s.next_seq += 1;
            s.events.push_back((seq, e));
            if s.events.len() > MAX_KEPT {
                s.events.pop_front();
            }
        }
        s.dropped = s.dropped.max(dropped);
        cv.notify_all();
    }

    /// Mark the stream ended (tests and the reader).
    pub fn close(&self) {
        let (lock, cv) = &*self.shared;
        lock.lock().unwrap_or_else(|p| p.into_inner()).closed = true;
        cv.notify_all();
    }

    /// The cursor of the next event: events recorded after this call are
    /// after it.
    pub fn cursor(&self) -> u64 {
        let (lock, _) = &*self.shared;
        lock.lock().unwrap_or_else(|p| p.into_inner()).next_seq
    }

    /// Memory iterations seen since Cloud Hypervisor started.
    pub fn iterations(&self) -> u64 {
        let (lock, _) = &*self.shared;
        lock.lock().unwrap_or_else(|p| p.into_inner()).iterations
    }

    /// Objects the reader could not read: torn, oversized or malformed.
    pub fn dropped(&self) -> u64 {
        let (lock, _) = &*self.shared;
        lock.lock().unwrap_or_else(|p| p.into_inner()).dropped
    }

    /// Events after `cursor`, waiting up to `timeout` for the first one.
    pub fn wait_since(&self, cursor: u64, timeout: Duration) -> Batch {
        let (lock, cv) = &*self.shared;
        let deadline = Instant::now() + timeout;
        let mut s = lock.lock().unwrap_or_else(|p| p.into_inner());
        while s.next_seq <= cursor && !s.closed {
            let now = Instant::now();
            if now >= deadline {
                break;
            }
            s = match cv.wait_timeout(s, deadline - now) {
                Ok((g, _)) => g,
                Err(p) => p.into_inner().0,
            };
        }
        let oldest = s.events.front().map_or(s.next_seq, |(seq, _)| *seq);
        Batch {
            events: s
                .events
                .iter()
                .filter(|(seq, _)| *seq >= cursor)
                .map(|(_, e)| e.clone())
                .collect(),
            cursor: s.next_seq,
            gap: cursor < oldest,
            closed: s.closed,
            iterations: s.iterations,
        }
    }
}

/// The write end's descriptor number, for `--event-monitor fd=<n>`.
pub fn fd_arg(write: &OwnedFd) -> RawFd {
    write.as_raw_fd()
}

/// How a send migration ended, as its events tell it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SendEnd {
    /// `migration-finished`: the destination took the guest. The source
    /// exits next.
    Finished,
    /// `migration-cancelled`: stopped by `vm.cancel-migration`; the source
    /// guest runs on.
    Cancelled,
    /// `migration-failed`. `established` is whether the migration connection
    /// was set up first (`migration-started`): when it was not, no guest
    /// state left the source.
    Failed { established: bool },
}

/// Follows one send migration through the events after its request.
///
/// Correlation: Cloud Hypervisor runs one migration at a time and emits a
/// migration's end on its worker thread before it takes the VM back, so a
/// new send can only be accepted after the previous one's end was emitted.
/// Events are emitted through a single channel in that order. Reading from a
/// cursor taken before the request, and only after this attempt's
/// `migration-starting`, therefore never attributes an earlier attempt's end
/// to this one.
#[derive(Debug, Default, Clone)]
pub struct SendTracker {
    starting: bool,
    established: bool,
    paused: bool,
    end: Option<SendEnd>,
}

impl SendTracker {
    pub fn observe(&mut self, e: &Event) -> Option<SendEnd> {
        if self.end.is_some() || e.source != "vm" {
            return self.end;
        }
        match e.event.as_str() {
            // CH v53 emits `migration-started` where v54 emits `-starting`.
            "migration-starting" => self.starting = true,
            "migration-started" => {
                self.established = self.starting;
                self.starting = true;
            }
            _ if !self.starting => {}
            "paused" => self.paused = true,
            "resumed" => self.paused = false,
            "migration-finished" => self.end = Some(SendEnd::Finished),
            "migration-cancelled" => self.end = Some(SendEnd::Cancelled),
            "migration-failed" => {
                self.end = Some(SendEnd::Failed {
                    established: self.established,
                })
            }
            _ => {}
        }
        self.end
    }

    /// Whether this attempt's `migration-starting` was seen.
    pub fn started(&self) -> bool {
        self.starting
    }

    /// The guest was paused for the final copy and not resumed since.
    pub fn paused(&self) -> bool {
        self.paused
    }

    pub fn end(&self) -> Option<SendEnd> {
        self.end
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ev(event: &str) -> Event {
        Event {
            since_start: Duration::ZERO,
            source: "vm".to_string(),
            event: event.to_string(),
            properties: BTreeMap::new(),
        }
    }

    fn json(event: &str, secs: u64) -> String {
        format!(
            "{{\n  \"timestamp\": {{\n    \"secs\": {secs},\n    \"nanos\": 5\n  }},\n  \"source\": \"vm\",\n  \"event\": \"{event}\",\n  \"properties\": null\n}}\n\n"
        )
    }

    // The exact shape Cloud Hypervisor v54 writes, split at every byte.
    #[test]
    fn framer_reads_pretty_events_in_any_chunking() {
        let text = format!(
            "{}{}{{\n  \"timestamp\": {{\n    \"secs\": 16,\n    \"nanos\": 1\n  }},\n  \"source\": \"vm\",\n  \"event\": \"migration-memory-iteration\",\n  \"properties\": {{\n    \"id\": \"3\"\n  }}\n}}\n\n",
            json("migration-starting", 15),
            json("migration-started", 15)
        );
        for chunk in [1, 3, 7, text.len()] {
            let mut f = Framer::default();
            let mut out = Vec::new();
            for part in text.as_bytes().chunks(chunk) {
                f.push(part, &mut out);
            }
            assert_eq!(out.len(), 3, "chunk {chunk}");
            assert_eq!(out[0].event, "migration-starting");
            assert_eq!(out[0].since_start, Duration::new(15, 5));
            assert_eq!(out[2].properties.get("id").map(String::as_str), Some("3"));
            assert_eq!(f.dropped, 0);
        }
    }

    // A non-blocking write that hit a full pipe leaves half an object; the
    // next event still reads.
    #[test]
    fn framer_recovers_from_a_torn_event() {
        let whole = json("migration-failed", 3);
        let torn = &json("migration-memory-iteration", 2)[..40];
        let mut f = Framer::default();
        let mut out = Vec::new();
        f.push(torn.as_bytes(), &mut out);
        f.push(b"\n", &mut out);
        f.push(whole.as_bytes(), &mut out);
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].event, "migration-failed");
        assert_eq!(f.dropped, 1);
    }

    #[test]
    fn framer_skips_oversized_and_malformed_objects() {
        let mut f = Framer::default();
        let mut out = Vec::new();
        let long = format!("{{\n  \"x\": \"{}\"\n}}\n", "a".repeat(MAX_LINE + 10));
        f.push(long.as_bytes(), &mut out);
        f.push(b"{\n  \"source\": 1\n}\n", &mut out);
        f.push(b"garbage outside an object\n}\n", &mut out);
        f.push(json("migration-finished", 1).as_bytes(), &mut out);
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].event, "migration-finished");
        assert_eq!(f.dropped, 2);
    }

    #[test]
    fn stream_keeps_migration_events_and_counts_iterations() {
        let s = EventStream::detached();
        let c = s.cursor();
        let mut dev = ev("activated");
        dev.source = "virtio-device".to_string();
        s.record(
            [
                dev,
                ev("migration-starting"),
                ev("migration-memory-iteration"),
                ev("migration-memory-iteration"),
                ev("reboot"),
                ev("paused"),
            ],
            0,
        );
        let b = s.wait_since(c, Duration::ZERO);
        let names: Vec<_> = b.events.iter().map(|e| e.event.as_str()).collect();
        assert_eq!(names, ["migration-starting", "paused"]);
        assert_eq!(b.iterations, 2);
        assert!(!b.gap && !b.closed);
        // Nothing new: an empty batch at the same cursor.
        let again = s.wait_since(b.cursor, Duration::from_millis(10));
        assert!(again.events.is_empty());
        assert_eq!(again.cursor, b.cursor);
    }

    #[test]
    fn stream_reports_a_gap_when_events_were_dropped_unread() {
        let s = EventStream::detached();
        let c = s.cursor();
        s.record((0..MAX_KEPT + 5).map(|_| ev("paused")), 0);
        let b = s.wait_since(c, Duration::ZERO);
        assert!(b.gap);
        assert_eq!(b.events.len(), MAX_KEPT);
    }

    #[test]
    fn stream_wakes_a_waiter_on_close() {
        let s = EventStream::detached();
        let c = s.cursor();
        let s2 = s.clone();
        let t = std::thread::spawn(move || s2.wait_since(c, Duration::from_secs(10)));
        std::thread::sleep(Duration::from_millis(20));
        s.close();
        let b = t.join().unwrap();
        assert!(b.closed && b.events.is_empty());
    }

    // A real pipe: what is written reaches the reader; EOF closes the stream.
    #[test]
    fn pipe_delivers_events_and_ends_when_the_writer_closes() {
        use std::io::Write;
        let (s, w) = EventStream::pipe().unwrap();
        let c = s.cursor();
        let mut f = File::from(w);
        f.write_all(json("migration-starting", 1).as_bytes())
            .unwrap();
        f.write_all(json("migration-finished", 2).as_bytes())
            .unwrap();
        drop(f);
        let mut events = Vec::new();
        let mut cur = c;
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            let b = s.wait_since(cur, Duration::from_millis(200));
            cur = b.cursor;
            events.extend(b.events);
            if b.closed || Instant::now() > deadline {
                break;
            }
        }
        let names: Vec<_> = events.iter().map(|e| e.event.as_str()).collect();
        assert_eq!(names, ["migration-starting", "migration-finished"]);
    }

    fn track(events: &[&str]) -> Option<SendEnd> {
        let mut t = SendTracker::default();
        let mut end = None;
        for e in events {
            end = t.observe(&ev(e));
        }
        end
    }

    // The sequences Cloud Hypervisor v54 was observed to emit.
    #[test]
    fn tracker_reads_each_v54_ending() {
        let ok = [
            "migration-starting",
            "migration-started",
            "pausing",
            "paused",
            "migration-finished",
        ];
        assert_eq!(track(&ok), Some(SendEnd::Finished));
        let cancelled = [
            "migration-starting",
            "migration-started",
            "migration-cancelled",
        ];
        assert_eq!(track(&cancelled), Some(SendEnd::Cancelled));
        let dst_died = [
            "migration-starting",
            "migration-started",
            "migration-failed",
        ];
        assert_eq!(
            track(&dst_died),
            Some(SendEnd::Failed { established: true })
        );
        let never_connected = ["migration-starting", "migration-failed"];
        assert_eq!(
            track(&never_connected),
            Some(SendEnd::Failed { established: false })
        );
        // v53: `migration-started` alone opens the attempt.
        assert_eq!(
            track(&["migration-started", "migration-failed"]),
            Some(SendEnd::Failed { established: false })
        );
    }

    // Phase 3's case: attempt A ends, attempt B starts; anything before B's
    // start -- A's end, or A's events delivered late but still ahead of B's
    // start in the stream -- does not end B.
    #[test]
    fn tracker_ignores_everything_before_its_own_start() {
        let mut t = SendTracker::default();
        for e in [
            "migration-failed",
            "migration-cancelled",
            "migration-finished",
            "resumed",
        ] {
            assert_eq!(t.observe(&ev(e)), None, "{e} before start");
        }
        assert!(!t.started());
        t.observe(&ev("migration-starting"));
        assert_eq!(
            t.observe(&ev("migration-finished")),
            Some(SendEnd::Finished)
        );
    }

    // Duplicates and anything after the end change nothing.
    #[test]
    fn tracker_end_is_final() {
        let mut t = SendTracker::default();
        for e in [
            "migration-starting",
            "migration-started",
            "migration-cancelled",
        ] {
            t.observe(&ev(e));
        }
        for e in [
            "migration-cancelled",
            "migration-finished",
            "migration-failed",
        ] {
            assert_eq!(t.observe(&ev(e)), Some(SendEnd::Cancelled));
        }
    }

    #[test]
    fn tracker_follows_pause_and_resume() {
        let mut t = SendTracker::default();
        for e in [
            "migration-starting",
            "migration-started",
            "pausing",
            "paused",
        ] {
            t.observe(&ev(e));
        }
        assert!(t.paused());
        t.observe(&ev("migration-failed"));
        t.observe(&ev("resumed"));
        assert_eq!(t.end(), Some(SendEnd::Failed { established: true }));
    }
}

//! Annotation-driven control surface for snapshot/restore actions.
//!
//! The SwiftSnapshot / SwiftRestore controllers drive lifecycle by
//! writing annotations onto the launcher pod; swiftletd watches its
//! own pod, dispatches the requested action to a handler, and writes
//! a status annotation back.
//!
//! Phase 2 wiring status:
//!   * commit 5: skeleton — annotation contract, idempotency, action loop
//!   * commit 6: snapshot capture (pause + snapshot + resume) and
//!               standalone resume — IMPLEMENTED below
//!   * commit 7: restore-receive (spawn_ch_restore + resume sequence)
//!               — still a no-op handler; ships in commit 7
//!
//! # Annotation contract
//!
//! Controller writes (input):
//!
//!   - `kubeswift.io/snapshot-action` — action verb (`capture`,
//!     `resume`, `prepare`).
//!   - `kubeswift.io/snapshot-action-id` — opaque, unique per action
//!     attempt (typically `<resource-name>-<resourceVersion>`).
//!   - `kubeswift.io/snapshot-action-args` — JSON args; shape depends
//!     on the action verb. Empty/absent is allowed.
//!
//! swiftletd writes (output):
//!
//!   - `kubeswift.io/snapshot-status` — `running`, `ready`, `failed`,
//!     `rejected`.
//!   - `kubeswift.io/snapshot-status-id` — mirrors the action-id this
//!     status corresponds to. The controller waits for status-id to
//!     match its own action-id before reading status.
//!   - `kubeswift.io/snapshot-status-detail` — optional human-readable
//!     diagnostic. Always overwritten when status is rewritten.
//!
//! # Concurrency model
//!
//! All annotation writes from this module flow through a single tokio
//! task per launcher pod (the action loop). The other writers in
//! swiftletd (lease poller, socket-ready callback, runtime reporter)
//! own their own annotation keys and do not collide.
//!
//! The action loop owns its [`ActionState`] by value — there is no
//! shared mutable state between the loop and the rest of swiftletd.
//! Idempotency is enforced via the action-id: if the loop sees the
//! same action-id twice, the second occurrence is a no-op. If a
//! different action-id arrives while one is already in flight, the
//! second is rejected (status: `rejected`) without disturbing the
//! first.
//!
//! # Why polling instead of watcher
//!
//! kube-rs offers a watcher stream that emits resource changes. We
//! poll via `Api::get()` instead because:
//!
//!   1. Action annotations change a handful of times per snapshot
//!      operation, not per second; polling at 2s cadence is plenty.
//!   2. The polling loop is straightforward to reason about and the
//!      annotation-decision logic stays a pure function (`decide`),
//!      keeping the test surface tight.
//!   3. RBAC narrows naturally to `pods/get` on the pod's name, the
//!      same scope as a watch + field selector.
//!
//! Important: we read annotations from the apiserver, not from the
//! pod's downward-API mount. Kubelet's downward-API sync interval is
//! ~60s; the apiserver is current.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::sync::OnceLock;
use std::time::Duration;
use tokio::sync::Notify;

/// W22 (PR #46 follow-up cluster re-walkthrough): tracks whether a
/// migration-send has completed cleanly. Set inside
/// `dispatch_migration_send`'s success branch (after the W1 gate
/// confirms CH is gone post-vm.send-migration); cleared never (the
/// flag has process lifetime — once a swiftletd has done a successful
/// send, its CH is irrevocably retired).
///
/// main.rs's post-launch report_running(VmStopped) path reads this
/// flag and suppresses the GuestRunning=False write when it's set,
/// avoiding the race against swiftletd-on-dst's W16 GuestRunning=True
/// write. Without this, the two writes (both Patches on the same
/// SwiftGuest condition) race on the apiserver: in S8's
/// cancel-during-Resuming scenario, src's VmStopped landed last,
/// stuck Resuming-live until spec.timeout.
///
/// Local-state-tracking-in-swiftletd-on-src per F2.4 (no
/// controller→swiftletd command channel needed). Matches the D1
/// pattern of tracking state via local primitives (D1 uses
/// runtime_dir/ch.pid for the cancel handler).
pub static MIGRATION_SEND_COMPLETED: AtomicBool = AtomicBool::new(false);

/// Returns true if `dispatch_migration_send` has completed cleanly in
/// this swiftletd process. main.rs uses this to decide whether to
/// suppress the post-launch VmStopped report (W22).
pub fn migration_send_completed_clean() -> bool {
    MIGRATION_SEND_COMPLETED.load(Ordering::SeqCst)
}

/// W23 (PR #46 follow-up cluster re-walkthrough, post-W22): graceful-
/// shutdown signal between the action loop and main.rs on the
/// migration-send terminal write.
///
/// **Distinct from W22 — do not conflate.** W22's
/// MIGRATION_SEND_COMPLETED flag prevents `main.rs` from writing
/// GuestRunning=False/VmStopped after a successful migration-send
/// (avoiding the race against dst-side W16 GuestRunning=True). W23
/// addresses a different race that the W22 fix INTRODUCED:
///
///   - Pre-W22: main's post-launch path called report_running(false,
///     VmStopped), whose apiserver Patch round-trip incidentally
///     gave the action loop ~tens of ms to finish writing the
///     terminal `migration-status: complete` annotation.
///   - Post-W22: main's W22-flag-set branch skips the patch and
///     returns immediately, killing the action loop thread before
///     its terminal-status write call lands. The src pod retains
///     `migration-status: running` forever; the controller never
///     observes the W1 gate and stalls until spec.timeout.
///
/// Fix: action loop fires `notify_one()` on this Notify AFTER the
/// terminal write for a MigrationSend action returns (success or
/// failure). main.rs's W22 success branch awaits `.notified()` with
/// a 10s bounded timeout before exiting. On timeout, log warn and
/// exit anyway — controller's spec.timeout is the ultimate floor.
///
/// Notify semantics: notify_one before notified wakes the next
/// notified call; notify_one is idempotent (at most one stored
/// permit). Matches the "fire once at terminal-write completion,
/// wait once in main" contract.
///
/// F2.4 preserved: no controller→swiftletd channel; this is purely
/// swiftletd-internal main↔action-loop coordination via standard
/// Rust primitives.
static MIGRATION_SEND_TERMINAL_WRITTEN: OnceLock<Arc<Notify>> = OnceLock::new();

/// Returns the singleton Notify used by the action loop to signal
/// "terminal-status write for MigrationSend has completed". main.rs
/// awaits `.notified()` on this with a bounded timeout. Initialised
/// lazily on first call.
pub fn migration_send_terminal_signal() -> Arc<Notify> {
    MIGRATION_SEND_TERMINAL_WRITTEN
        .get_or_init(|| Arc::new(Notify::new()))
        .clone()
}

use kube::api::{Api, Patch, PatchParams};
use kube::Client;
use serde_json::json;

use crate::kube_client;

// Annotation keys — snapshot namespace.
//
// The action loop runs against multiple action namespaces (snapshot,
// migration). Each namespace has its own KeySet (see `SNAPSHOT_KEYS`,
// `MIGRATION_KEYS`); these per-namespace constants are the identifier
// strings the KeySet entries point at. Kept as `pub const` so external
// code can reference the canonical key names directly without going
// through the KeySet abstraction.

// Snapshot namespace — controller writes (read by swiftletd).
pub const ACTION_KEY: &str = "kubeswift.io/snapshot-action";
pub const ACTION_ID_KEY: &str = "kubeswift.io/snapshot-action-id";
pub const ACTION_ARGS_KEY: &str = "kubeswift.io/snapshot-action-args";

// Snapshot namespace — swiftletd writes (read by controller).
pub const STATUS_KEY: &str = "kubeswift.io/snapshot-status";
pub const STATUS_ID_KEY: &str = "kubeswift.io/snapshot-status-id";
pub const STATUS_DETAIL_KEY: &str = "kubeswift.io/snapshot-status-detail";
/// Pause window in milliseconds, observed at the action handler. The
/// controller surfaces this in SwiftSnapshot.status.observedPauseWindow.
/// Set only on a successful capture.
pub const STATUS_PAUSE_WINDOW_MS_KEY: &str = "kubeswift.io/snapshot-pause-window-ms";

// Migration namespace — controller / operator writes (read by swiftletd).
pub const MIGRATION_ACTION_KEY: &str = "kubeswift.io/migration-action";
pub const MIGRATION_ACTION_ID_KEY: &str = "kubeswift.io/migration-action-id";
pub const MIGRATION_ACTION_ARGS_KEY: &str = "kubeswift.io/migration-action-args";

// Migration namespace — swiftletd writes (read by controller / operator).
pub const MIGRATION_STATUS_KEY: &str = "kubeswift.io/migration-status";
pub const MIGRATION_STATUS_ID_KEY: &str = "kubeswift.io/migration-status-id";
pub const MIGRATION_STATUS_DETAIL_KEY: &str = "kubeswift.io/migration-status-detail";
/// Observed vCPU-paused window in ms, set on a successful migration's
/// terminal `complete` status. Mirrors snapshot's STATUS_PAUSE_WINDOW_MS_KEY
/// shape but in the migration namespace.
pub const MIGRATION_PAUSE_WINDOW_MS_KEY: &str = "kubeswift.io/migration-pause-window-ms";
/// Best-effort heuristic progress percentage emitted by swiftletd-source
/// during `vm.send-migration` per Phase 3b design doc §5.4. Monotonically
/// increasing 0-95 (capped to make it obvious to operators that the
/// number is heuristic, never authoritative). Read by `swiftctl
/// migration describe` and surfaced with an explicit
/// "(estimate)" qualifier per design §6.2.
pub const MIGRATION_PROGRESS_ESTIMATE_KEY: &str = "kubeswift.io/migration-progress-estimate";

/// Pod-network baseline bandwidth used to compute the
/// `migration-progress-estimate` annotation per Phase 3b design doc
/// §5.4. **Calico-VXLAN-specific** measurement from spike Q4 (107.2
/// MB/s CH `vm.send-migration` throughput on the spike cluster's
/// Hetzner gigabit interconnect, which is ~95% of the 112.75 MB/s raw
/// TCP ceiling). Operators on other CNI implementations may see
/// different efficiency floors; the estimate is least accurate on
/// cluster configurations furthest from spike.
///
/// If walkthroughs find the heuristic drifts materially on
/// representative workloads, the planned follow-up is to expose the
/// baseline as `SwiftGuestClass.spec.migrationProgressBaselineMBps`
/// (operator override). Phase 3b PR 1 ships the hardcoded constant;
/// don't ship the field pre-emptively.
pub const PROGRESS_BASELINE_MBPS: f64 = 108.0;

/// Phase 2 unsafe-plaintext acknowledgement annotation. Required on
/// the launcher pod for swiftletd to accept any migration action — the
/// S2 mitigation gate from `docs/design/live-migration-phase-2.md` §8.2.1.
/// Set to the literal string `ack` by the operator to acknowledge that
/// Phase 2 carries unauthenticated guest state in cleartext. Phase 3
/// removes this annotation entirely once mTLS lands.
pub const MIGRATION_PHASE2_ACK_KEY: &str = "kubeswift.io/migration-phase2-unsafe-plaintext";

/// Annotation key set for one action namespace. Each namespace
/// (snapshot, migration) has its own KeySet; the action loop runs
/// `decide` against each per tick.
///
/// `KeySet` is `Copy` so it can be passed by value to async handlers
/// without lifetime gymnastics. All entries are `'static` strings.
#[derive(Debug, Clone, Copy)]
pub struct KeySet {
    pub action_key: &'static str,
    pub action_id_key: &'static str,
    pub action_args_key: &'static str,
    pub status_key: &'static str,
    pub status_id_key: &'static str,
    pub status_detail_key: &'static str,
    /// When `Some`, `decide` requires this annotation to be set to the
    /// literal string `ack` for any action in this namespace to be
    /// accepted. Phase 2 migration uses this for the
    /// `migration-phase2-unsafe-plaintext` gate (§8.2.1). Snapshot's
    /// KeySet has `None`.
    pub ack_key: Option<&'static str>,
    /// Maps the action verb string from the action_key annotation to
    /// an [`ActionKind`]. Each namespace has its own verb dictionary.
    pub parse_verb: fn(&str) -> ActionKind,
    /// Identifier string used in log lines and rejection details. Stays
    /// stable across releases; treated as part of the operator-visible
    /// surface.
    pub namespace: &'static str,
}

/// Snapshot namespace key set. Used by `dispatch_capture`, `dispatch_resume`,
/// and (Phase 2 commit 7) `dispatch_restore_prepare`.
pub static SNAPSHOT_KEYS: KeySet = KeySet {
    action_key: ACTION_KEY,
    action_id_key: ACTION_ID_KEY,
    action_args_key: ACTION_ARGS_KEY,
    status_key: STATUS_KEY,
    status_id_key: STATUS_ID_KEY,
    status_detail_key: STATUS_DETAIL_KEY,
    ack_key: None,
    parse_verb: parse_snapshot_verb,
    namespace: "snapshot",
};

/// Migration namespace key set. Used by `dispatch_migration_send`,
/// `dispatch_migration_receive`, `dispatch_migration_cancel`. The ack-key
/// gate enforces the Phase 2 plaintext-transport acknowledgement
/// (`docs/design/live-migration-phase-2.md` §8.2.1).
pub static MIGRATION_KEYS: KeySet = KeySet {
    action_key: MIGRATION_ACTION_KEY,
    action_id_key: MIGRATION_ACTION_ID_KEY,
    action_args_key: MIGRATION_ACTION_ARGS_KEY,
    status_key: MIGRATION_STATUS_KEY,
    status_id_key: MIGRATION_STATUS_ID_KEY,
    status_detail_key: MIGRATION_STATUS_DETAIL_KEY,
    ack_key: Some(MIGRATION_PHASE2_ACK_KEY),
    parse_verb: parse_migration_verb,
    namespace: "migration",
};

/// Env var the SwiftGuest controller sets on the launcher container when
/// live-migration mTLS is enabled (Phase 3c PR 4). When set, swiftletd runs
/// in "secured mode": it validates the migration URL is a loopback address
/// (S1) and bypasses the plaintext-ack gate (the channel is TLS). Must
/// match `internal/migrationsidecar.EnvMTLSEnabled` on the Go side.
pub const MIGRATION_MTLS_ENV: &str = "KUBESWIFT_MIGRATION_MTLS";

/// True when the launcher is running in secured (mTLS) migration mode.
pub fn secured_migration_mode() -> bool {
    secured_from(std::env::var(MIGRATION_MTLS_ENV).ok().as_deref())
}

/// Pure helper for [`secured_migration_mode`] — secured when the value is
/// exactly "1" or "true".
fn secured_from(v: Option<&str>) -> bool {
    matches!(v, Some("1") | Some("true"))
}

/// Returns the migration [`KeySet`], dropping the plaintext-ack requirement
/// when secured mode is active. Under mTLS the channel is authenticated, so
/// the `migration-phase2-unsafe-plaintext: ack` escape-hatch is moot and
/// must not gate the secured flow (design §6.2). The controller KEEPS
/// emitting the ack for now (harmless — we ignore it here), so there is no
/// version-skew window during a rolling upgrade.
pub fn migration_keys() -> KeySet {
    migration_keys_for(secured_migration_mode())
}

/// Pure helper for [`migration_keys`].
fn migration_keys_for(secured: bool) -> KeySet {
    let mut k = MIGRATION_KEYS;
    if secured {
        k.ack_key = None;
    }
    k
}

/// Validates that a CH migration URL targets a loopback address. Under
/// secured mode the controller writes `tcp:127.0.0.1:<port>` (the local
/// stunnel proxy); an attacker who rewrote the operator-writable
/// `migration-action-args` to a remote IP would otherwise make CH stream
/// the guest in cleartext to that endpoint — the S1 risk mTLS does not
/// close on its own (design §6.1). `unix:` sockets are inherently local and
/// always pass. Only invoked under secured mode (the plaintext path still
/// targets the remote dst IP directly).
pub fn validate_loopback_url(url: &str) -> Result<(), String> {
    if url.strip_prefix("unix:").is_some() {
        return Ok(());
    }
    let rest = url.strip_prefix("tcp:").ok_or_else(|| {
        format!(
            "secured migration: unsupported URL scheme (want tcp:/unix:): {}",
            url
        )
    })?;
    let host = if let Some(after) = rest.strip_prefix('[') {
        // [ipv6]:port
        let end = after
            .find(']')
            .ok_or_else(|| format!("secured migration: malformed bracketed host: {}", url))?;
        &after[..end]
    } else {
        match rest.rfind(':') {
            Some(i) => &rest[..i],
            None => rest,
        }
    };
    let loopback = host == "localhost"
        || host
            .parse::<std::net::IpAddr>()
            .map(|ip| ip.is_loopback())
            .unwrap_or(false);
    if loopback {
        Ok(())
    } else {
        Err(format!(
            "secured migration: refusing non-loopback host {:?} (S1: migration URL must be localhost under mTLS): {}",
            host, url
        ))
    }
}

/// Maps a snapshot-namespace verb string to an [`ActionKind`].
fn parse_snapshot_verb(verb: &str) -> ActionKind {
    match verb {
        "capture" => ActionKind::SnapshotCapture,
        "resume" => ActionKind::SnapshotResume,
        "prepare" => ActionKind::RestorePrepare,
        other => ActionKind::Unknown(other.to_string()),
    }
}

/// Maps a migration-namespace verb string to an [`ActionKind`].
fn parse_migration_verb(verb: &str) -> ActionKind {
    match verb {
        "send" => ActionKind::MigrationSend,
        "receive" => ActionKind::MigrationReceive,
        "cancel" => ActionKind::MigrationCancel,
        other => ActionKind::Unknown(other.to_string()),
    }
}

/// Default per-call timeout for pause/resume/snapshot when the action
/// args don't carry a `timeout_seconds` hint. 600s covers a 200 GiB VM
/// at the Phase 0 ~2.8s/GiB curve with margin; the controller passes
/// a tighter, size-derived value in practice.
pub const DEFAULT_ACTION_TIMEOUT_SECS: u64 = 600;

/// Polling cadence for the action loop. 2s matches the lease poller
/// and is well below any plausible action latency (CH pause is ms;
/// snapshot+memory write is seconds).
pub const POLL_INTERVAL: Duration = Duration::from_secs(2);

/// Action verbs the controller (or operator, in Phase 2 manual demo)
/// can ask for. Each variant belongs to one namespace (snapshot or
/// migration); the parser is selected via `KeySet.parse_verb`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ActionKind {
    // Snapshot namespace — `kubeswift.io/snapshot-action` verbs.
    /// pause -> snapshot(destination_url) -> resume. Ships in commit 6.
    SnapshotCapture,
    /// Stand-alone resume after a paused VM (used after restore-prepare
    /// completes). Ships in commit 6 alongside capture.
    SnapshotResume,
    /// Bring up a fresh CH process via spawn_ch_restore. Ships in commit 7.
    RestorePrepare,

    // Migration namespace — `kubeswift.io/migration-action` verbs.
    // Ships in PR-B / PR-C of live-migration Phase 2.
    /// Source-side: invoke `vm.send-migration` to stream guest state to
    /// the destination CH. Long-lived (tens of seconds typical).
    /// `docs/design/live-migration-phase-2.md` §2 row 3.
    MigrationSend,
    /// Destination-side: invoke `vm.receive-migration` to await the
    /// source's connection and restore guest state. Long-lived.
    /// `docs/design/live-migration-phase-2.md` §2 row 2.
    MigrationReceive,
    /// Destination-side cancel primitive: SIGKILL the local CH child.
    /// The source CH automatically resumes on the dst-kill (Q1d-F2).
    /// `docs/design/live-migration-phase-2.md` §2 row 7.
    MigrationCancel,

    /// Anything else — surfaces as a malformed-rejection so the
    /// controller learns immediately rather than the launcher silently
    /// stalling on an unknown verb.
    Unknown(String),
}

impl ActionKind {
    /// Map an `kubeswift.io/snapshot-action` value to a kind.
    ///
    /// Retained for backward compatibility with callers outside the
    /// action loop (none today). Internally, prefer
    /// `KeySet.parse_verb(verb)` which is namespace-aware.
    pub fn from_verb(verb: &str) -> Self {
        parse_snapshot_verb(verb)
    }
}

/// One action requested by the controller, with its idempotency key.
#[derive(Debug, Clone, PartialEq)]
pub struct PendingAction {
    pub kind: ActionKind,
    pub id: String,
    pub args: serde_json::Value,
}

/// Output of [`decide`] — what the action loop should do given the
/// current pod annotations and its own local state.
#[derive(Debug, Clone, PartialEq)]
pub enum ActionDecision {
    /// No action annotation, or annotation matches what we already
    /// finished. Loop sleeps and tries again next tick.
    Idle,
    /// New action — start handling it.
    Accept(PendingAction),
    /// Same action-id as one we already finished. No-op (do not
    /// re-execute; the controller is just rewriting unchanged spec).
    Idempotent { id: String },
    /// A different action-id arrived while we still have one in
    /// flight. We refuse; the controller learns via the rejected
    /// status annotation.
    RejectInFlight {
        incoming_id: String,
        current_id: String,
    },
    /// The namespace requires an ack-gate annotation (Phase 2's
    /// `migration-phase2-unsafe-plaintext: ack`) and it is missing,
    /// empty, or set to a value other than `ack`. Surfaced as a
    /// `Rejected` status with the action-id mirrored — different
    /// from `RejectMalformed` because the action-id IS present and
    /// the controller can correlate the rejection.
    RejectAckMissing {
        incoming_id: String,
        ack_key: &'static str,
    },
    /// Annotation set is incomplete or otherwise unparseable.
    RejectMalformed(String),
}

/// Status verbs swiftletd emits.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StatusKind {
    Running,
    Ready,
    Failed,
    Rejected,
    /// Namespace-specific status verb override. Used by migration to
    /// emit `"complete"` (source success) and `"running"` (destination
    /// success) per `docs/design/live-migration-phase-2.md` §3.1; the
    /// shared `Ready` variant ("ready") stays the snapshot success
    /// verb to avoid breaking SwiftSnapshot/SwiftRestore controllers.
    Custom(&'static str),
}

impl StatusKind {
    pub fn as_str(self) -> &'static str {
        match self {
            StatusKind::Running => "running",
            StatusKind::Ready => "ready",
            StatusKind::Failed => "failed",
            StatusKind::Rejected => "rejected",
            StatusKind::Custom(s) => s,
        }
    }
}

/// Per-namespace local state. One slot in `ActionState` per
/// namespace (snapshot, migration). Independent across namespaces:
/// finishing a snapshot does not reset migration state and vice versa.
#[derive(Default, Debug)]
pub struct NamespaceState {
    /// action-id of the last action we drove to a terminal status
    /// (Ready, Failed, or Rejected). Used for idempotency: if the
    /// controller's annotations still reference this id, we ignore
    /// rather than re-execute.
    pub last_completed_id: Option<String>,
    /// Action currently being processed in this namespace. `None`
    /// when idle. The loop dispatches at most one action per
    /// namespace at a time; namespaces can have at most one in-flight
    /// each.
    pub in_flight: Option<PendingAction>,
}

/// Local state the action loop carries across iterations. Holds
/// per-namespace state for snapshot and migration so the two
/// namespaces' idempotency and in-flight tracking remain independent.
///
/// Mutual exclusion across namespaces (no concurrent snapshot +
/// migration on the same guest) is enforced at dispatch time in
/// `handle_pod_state`, not by sharing this state.
#[derive(Default, Debug)]
pub struct ActionState {
    pub snapshot: NamespaceState,
    pub migration: NamespaceState,
}

/// Pure-function action-decision logic. Tested in isolation — no
/// kube-rs dependency, no I/O, no async. The action loop calls this
/// each tick with the freshly-fetched annotations, the namespace's
/// `KeySet`, and the namespace's current in-memory state.
///
/// The decision tree:
///
/// 1. **Idle** — no `action_key` set, or set empty.
/// 2. **RejectMalformed** — `action_key` set without `action_id_key`.
/// 3. **Idempotent** — `action_id_key` matches `last_completed_id` or
///    matches the in-flight action's id.
/// 4. **RejectInFlight** — different action-id arrives while one is in
///    flight (cancel verbs are exempt — they bypass this gate so they
///    can interrupt a running migration).
/// 5. **RejectAckMissing** — namespace has `ack_key=Some(_)` but the
///    annotation is absent or has a value other than `ack`. Phase 2
///    plaintext-transport gate (§8.2.1).
/// 6. **Accept** — all gates pass.
pub fn decide(
    annotations: &BTreeMap<String, String>,
    keys: &KeySet,
    last_completed_id: Option<&str>,
    in_flight_id: Option<&str>,
) -> ActionDecision {
    let verb = match annotations.get(keys.action_key) {
        Some(v) if !v.is_empty() => v.as_str(),
        _ => return ActionDecision::Idle,
    };
    let id = match annotations.get(keys.action_id_key) {
        Some(i) if !i.is_empty() => i.clone(),
        _ => {
            return ActionDecision::RejectMalformed(format!(
                "{} set without {}",
                keys.action_key, keys.action_id_key
            ))
        }
    };
    if last_completed_id == Some(id.as_str()) {
        return ActionDecision::Idempotent { id };
    }
    let kind = (keys.parse_verb)(verb);
    // Cancel verbs bypass the in-flight gate so they can interrupt an
    // in-flight migration (see `docs/design/live-migration-phase-2.md`
    // §2 row 7 + Q1d-F2). All other verbs follow the normal
    // RejectInFlight rule.
    let is_cancel = matches!(kind, ActionKind::MigrationCancel);
    if let Some(current) = in_flight_id {
        if current == id {
            // Same action still being processed — quiet no-op.
            return ActionDecision::Idempotent { id };
        }
        if !is_cancel {
            return ActionDecision::RejectInFlight {
                incoming_id: id,
                current_id: current.to_string(),
            };
        }
        // is_cancel: fall through to ack-gate + Accept.
    }
    // Phase 2 ack-gate: Phase 2 plaintext-transport ack annotation must
    // be present on any namespace that requires it. Run AFTER action-id
    // parsing so the rejection's status-id can be correlated by the
    // controller — RejectAckMissing carries the incoming action-id.
    // `docs/design/live-migration-phase-2.md` §8.2.1.
    if let Some(ack_key) = keys.ack_key {
        let acked = matches!(annotations.get(ack_key).map(|s| s.as_str()), Some("ack"));
        if !acked {
            return ActionDecision::RejectAckMissing {
                incoming_id: id,
                ack_key,
            };
        }
    }
    let args = annotations
        .get(keys.action_args_key)
        .and_then(|s| serde_json::from_str(s).ok())
        .unwrap_or(serde_json::Value::Null);
    ActionDecision::Accept(PendingAction { kind, id, args })
}

/// Outcome returned by a dispatched action. Carries an optional
/// detail string (mirrored into `kubeswift.io/snapshot-status-detail`)
/// and an optional observed pause window in milliseconds (only set by
/// the capture path; mirrored into
/// `kubeswift.io/snapshot-pause-window-ms`).
#[derive(Debug, Default, Clone, PartialEq)]
pub struct ActionOutcome {
    pub detail: Option<String>,
    pub pause_window_ms: Option<u64>,
    /// Override the terminal-success status string. Defaults to
    /// `StatusKind::Ready` ("ready") when None — that's the snapshot
    /// success verb.
    ///
    /// Migration source uses `Some("complete")`; migration destination
    /// uses `Some("running")`. The string is written verbatim to the
    /// status annotation; the controller (or operator, in Phase 2
    /// manual demo) reads the matching value to detect terminal
    /// success. See `docs/design/live-migration-phase-2.md` §3.1
    /// for the per-direction status enum.
    pub success_status: Option<&'static str>,
}

impl ActionOutcome {
    pub fn detail(s: impl Into<String>) -> Self {
        Self {
            detail: Some(s.into()),
            pause_window_ms: None,
            success_status: None,
        }
    }
}

/// Dispatch a pending action. Returns the outcome on success or a
/// human-readable detail string on failure (mirrored into the
/// status-detail annotation).
///
/// Migration variants dispatch through their own handlers; see
/// `dispatch_migration_send`, `dispatch_migration_receive`,
/// `dispatch_migration_cancel`.
pub async fn dispatch(action: &PendingAction, api_socket: &Path) -> Result<ActionOutcome, String> {
    match &action.kind {
        ActionKind::SnapshotCapture => dispatch_capture(action, api_socket).await,
        ActionKind::SnapshotResume => dispatch_resume(action, api_socket).await,
        ActionKind::RestorePrepare => {
            // Phase 2 commit 7 wires this in.
            log::info!(
                "dispatch_restore_prepare id={} (skeleton no-op; commit 7)",
                action.id
            );
            Ok(ActionOutcome::detail(
                "skeleton no-op: restore-prepare not yet wired",
            ))
        }
        ActionKind::MigrationSend => dispatch_migration_send(action, api_socket).await,
        ActionKind::MigrationReceive => dispatch_migration_receive(action, api_socket).await,
        ActionKind::MigrationCancel => dispatch_migration_cancel(action, api_socket).await,
        ActionKind::Unknown(verb) => {
            log::warn!(
                "dispatch_unknown_action_verb verb={} id={}",
                verb,
                action.id
            );
            Err(format!("unknown action verb: {}", verb))
        }
    }
}

/// Args parsed from `kubeswift.io/migration-action-args` for the
/// `send` verb. The destination URL is the load-bearing field and
/// is read straight from the args (Phase 2 manual path) — see the
/// `SECURITY-S1` tag in the implementation for the Phase 3 hand-off.
#[derive(Debug, serde::Deserialize)]
struct MigrationSendArgs {
    /// Destination URL in CH's syntax: `tcp:<host>:<port>` or
    /// `unix:<path>`. Phase 2 reads this from operator-set pod
    /// annotations; Phase 3 will read from the SwiftMigration CR
    /// directly to mitigate S1 (annotation-trust-boundary).
    target_url: String,
    /// Override for the per-call HTTP timeout in seconds. Migration
    /// can take tens of seconds for typical workloads (Phase 2 spike
    /// Q2 — LOW dirty-rate ~19 s, HIGH dirty-rate ~37 s for 1 GiB
    /// guest). The controller (Phase 3) will derive this from VM
    /// memory size; Phase 2 defaults to DEFAULT_ACTION_TIMEOUT_SECS.
    #[serde(default)]
    timeout_seconds: Option<u64>,
    /// Phase 3b PR 1: guest RAM in MiB, used to compute the
    /// `migration-progress-estimate` annotation heuristic per
    /// design doc §5.4. When absent, swiftletd skips the
    /// progress-estimate emission entirely — best-effort posture.
    /// The Phase 3b PR 2 controller integration will always set
    /// this from `SwiftGuest.spec.resources.memory`; PR 1 manual-
    /// demo callers set it directly in the action-args annotation.
    #[serde(default)]
    guest_ram_mib: Option<u32>,
    /// Cloud Hypervisor `downtime_ms` target for vm.send-migration
    /// (CH >= v52). CH iterates pre-copy until the estimated final
    /// stop-and-copy fits under this vCPU-pause budget, then commits
    /// (classical convergence, superseding v51.1's hardcoded 5-iter
    /// cap). When absent, swiftletd omits it from the send body and CH
    /// keeps its native behaviour. Set by the controller from
    /// SwiftMigration.spec.downtimeTarget.
    #[serde(default)]
    downtime_ms: Option<u64>,
}

/// Args parsed from `kubeswift.io/migration-action-args` for the
/// `receive` verb.
#[derive(Debug, serde::Deserialize)]
struct MigrationReceiveArgs {
    /// Receive URL CH binds the listener on. Typically
    /// `tcp:0.0.0.0:<port>`. Same Phase 2 / Phase 3 hand-off as
    /// `MigrationSendArgs.target_url`.
    listen_url: String,
    /// Override for the per-call HTTP timeout in seconds.
    #[serde(default)]
    timeout_seconds: Option<u64>,
    /// Phase 3a D3: guest IP to propagate onto the dst pod's
    /// `kubeswift.io/guest-ip` annotation post-resume. Live migration
    /// preserves the guest's network state byte-for-byte (Phase 2
    /// spike Q1e: virtio-net device state, MAC, queue config); the
    /// guest does NOT re-DHCP on resume, so the dst pod cannot
    /// re-discover the IP locally. The controller reads the src
    /// pod's existing `kubeswift.io/guest-ip` annotation and forwards
    /// it here. See `docs/design/live-migration-phase-3a.md` §3.6
    /// + §7.2 D3.
    ///
    // SECURITY-S1: guest_ip is read from operator-set pod annotation
    // args (Phase 2 manual path; Phase 3a controller-mediated). Phase
    // 3b moves operator-controlled inputs to the SwiftMigration CR
    // and removes the annotation read; this // SECURITY-S1 marker
    // makes the read site greppable for the Phase 3b sweep.
    #[serde(default)]
    guest_ip: Option<String>,
}

/// Source-side migration handler. Invokes `vm.send-migration` against
/// the local CH and returns the outcome.
///
/// `docs/design/live-migration-phase-2.md` §2 row 3 + §4.3.1.
///
/// Blocking semantics: the underlying `send_migration` call blocks for
/// the entire pre-copy + stop-and-copy duration (tens of seconds for
/// typical workloads). The action loop's tokio runtime is
/// `current_thread`, so this dispatch effectively serializes the loop
/// during the migration. Cancel cannot fire on the SOURCE side
/// (cancel is destination-kill per F2). Future versions may run this
/// on a worker thread for symmetry with receive; for Phase 2 the
/// current-thread block is acceptable on the source.
/// Drop-guarded handle for the progress-estimate emitter thread.
/// Stores `true` into the shared cancel flag on drop so the emitter
/// exits at its next ~5s tick. The std::thread is intentionally
/// not joined — emission is best-effort and the thread cleanly
/// exits once its tokio runtime block_on returns; we don't gate
/// dispatch return on cleanup.
///
/// Set on drop covers both the happy path (`client.send_migration`
/// returns, the guard goes out of scope) AND async cancellation
/// (the dispatch_migration_send future is dropped mid-flight, the
/// guard's destructor still runs).
struct ProgressEmitterGuard {
    cancel: Arc<AtomicBool>,
}
impl Drop for ProgressEmitterGuard {
    fn drop(&mut self) {
        self.cancel.store(true, Ordering::SeqCst);
    }
}

/// Spawn the progress-estimate emitter per Phase 3b design doc §5.4.
///
/// Implementation notes:
///
/// - **Dedicated std::thread, not tokio::spawn.** The action loop's
///   tokio runtime is `current_thread`; `client.send_migration` is a
///   sync HTTP call that blocks the thread for tens of seconds. A
///   `tokio::spawn`'d task would not get scheduled until send_migration
///   returns, defeating the point of progress emission. Mirrors the
///   lease poller's std::thread + own-runtime pattern at
///   [`crate::lease`].
/// - **Best-effort.** If `guest_ram_mib` is absent, env vars
///   POD_NAMESPACE / POD_NAME are unset, or the kube client can't be
///   created, the emitter logs at debug and exits cleanly. Annotation
///   patch failures during the loop are also debug-level; we do NOT
///   abort the send call on emitter problems.
/// - **Cancellation.** The returned guard's Drop impl stores `true`
///   into the cancel flag. The thread observes the flag at its next
///   tick (worst-case ~5s after dispatch return). The thread is not
///   joined.
/// - **Why not also publish to the CRD status field?** Per design
///   §5.4: the transient nature of progress data (changes every 5s,
///   valid for ~30-100s) doesn't match status semantics; persisting
///   it across post-completion reconciles would be misleading.
///   swiftctl reads the annotation directly.
fn spawn_progress_emitter(action_id: String, guest_ram_mib: Option<u32>) -> ProgressEmitterGuard {
    let cancel = Arc::new(AtomicBool::new(false));
    let cancel_for_thread = cancel.clone();
    std::thread::spawn(move || {
        let Some(ram_mib) = guest_ram_mib else {
            log::debug!(
                "progress_estimate_disabled id={} reason=guest_ram_mib_absent",
                action_id
            );
            return;
        };
        let (namespace, pod_name) = match (
            std::env::var("POD_NAMESPACE").ok(),
            std::env::var("POD_NAME").ok(),
        ) {
            (Some(ns), Some(name)) if !ns.is_empty() && !name.is_empty() => (ns, name),
            _ => {
                log::debug!(
                    "progress_estimate_disabled id={} reason=POD_NAMESPACE_or_POD_NAME_unset",
                    action_id
                );
                return;
            }
        };
        let rt = match tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
        {
            Ok(r) => r,
            Err(e) => {
                log::debug!(
                    "progress_estimate_runtime_failed id={} err={}",
                    action_id,
                    e
                );
                return;
            }
        };
        rt.block_on(async move {
            let client = match crate::kube_client::create_client().await {
                Ok(c) => c,
                Err(e) => {
                    log::debug!(
                        "progress_estimate_kube_client_unavailable id={} err={}",
                        action_id,
                        e
                    );
                    return;
                }
            };
            let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client, &namespace);
            let started = std::time::Instant::now();
            let expected_s = (ram_mib as f64) / PROGRESS_BASELINE_MBPS;
            let mut ticker = tokio::time::interval(Duration::from_secs(5));
            // First tick fires immediately; skip it so the first
            // emission lands ~5s into the send call. Operators
            // reading the annotation right at dispatch entry would
            // otherwise see 0% which is uninformative noise.
            ticker.tick().await;
            loop {
                ticker.tick().await;
                if cancel_for_thread.load(Ordering::SeqCst) {
                    log::debug!("progress_estimate_cancelled id={}", action_id);
                    return;
                }
                let elapsed_s = started.elapsed().as_secs_f64();
                let pct = compute_progress_estimate(elapsed_s, expected_s);
                let mut annotations = BTreeMap::new();
                annotations.insert(MIGRATION_PROGRESS_ESTIMATE_KEY.to_string(), pct.to_string());
                let patch = json!({"metadata": {"annotations": annotations}});
                let pp = PatchParams::default();
                match api.patch(&pod_name, &pp, &Patch::Merge(&patch)).await {
                    Ok(_) => log::debug!("progress_estimate_patched id={} pct={}", action_id, pct),
                    Err(e) => log::debug!(
                        "progress_estimate_patch_failed id={} pct={} err={}",
                        action_id,
                        pct,
                        e
                    ),
                }
            }
        });
    });
    ProgressEmitterGuard { cancel }
}

async fn dispatch_migration_send(
    action: &PendingAction,
    api_socket: &Path,
) -> Result<ActionOutcome, String> {
    // SECURITY-S1: target_url is read from the operator-writable pod
    // annotation args. Phase 3c PR 4 mitigates the annotation-trust-boundary
    // issue under mTLS by validating the URL is loopback below (a tampered
    // remote URL is rejected, not dialed) rather than reading from the
    // SwiftMigration CR — under the sidecar architecture the URL is always
    // the local stunnel proxy, so loopback-validation is the equivalent and
    // simpler mitigation. See docs/design/live-migration-phase-3c.md §6.1.
    let args: MigrationSendArgs = serde_json::from_value(action.args.clone())
        .map_err(|e| format!("parse migration_send args: {}", e))?;

    // S1 (secured mode): the target_url comes from the operator-writable
    // action-args annotation. Under mTLS it MUST be the local stunnel proxy
    // (loopback); refuse a non-loopback target so a tampered annotation
    // cannot redirect the plaintext CH stream to an attacker endpoint. The
    // "secured migration:" detail is NOT a connection_refused token, so the
    // controller does not retry it (it is a real rejection, not a sidecar-
    // not-ready race).
    if secured_migration_mode() {
        if let Err(e) = validate_loopback_url(&args.target_url) {
            return Err(format!("send_migration: {}", e));
        }
    }

    log::info!(
        "dispatch_migration_send id={} target={}",
        action.id,
        args.target_url
    );

    let timeout =
        std::time::Duration::from_secs(args.timeout_seconds.unwrap_or(DEFAULT_ACTION_TIMEOUT_SECS));
    let client = swift_ch_client::ApiClient::new(api_socket).with_timeout(timeout);

    // Phase 3b PR 1 Commit D — spawn the progress-estimate emitter
    // BEFORE the blocking send_migration call. Drop guard ensures
    // the emitter is signaled to exit on any return path (success,
    // error, async cancellation).
    let _progress = spawn_progress_emitter(action.id.clone(), args.guest_ram_mib);

    let started = std::time::Instant::now();
    if let Err(e) = client.send_migration(&args.target_url, args.downtime_ms) {
        return Err(format!(
            "send_migration: {}",
            sanitize_ch_error(&format!("{:?}", e))
        ));
    }

    // W1 completion-gate (load-bearing item C, walkthrough W1).
    // `send_migration` exit=0 is necessary but not sufficient; the
    // source CH must actually have exited cleanly (Q1c finding).
    // We probe vm_info: if it returns ConnectionRefused / similar,
    // CH is gone (the expected post-success state). If it returns
    // Running, send-migration claimed success but the guest is
    // still here — abnormal, treat as failure.
    //
    // `docs/design/live-migration-phase-2.md` §6.1.
    match client.vm_info() {
        Err(_) => {
            // CH is gone — the expected outcome on success.
            let elapsed_ms = started.elapsed().as_millis().min(u64::MAX as u128) as u64;
            // W22: mark migration-send as cleanly completed so main.rs's
            // post-launch path suppresses the VmStopped write that
            // would race the dst-side W16 GuestRunning=True write.
            // Set BEFORE logging the success message so any future
            // observer sees a consistent state.
            MIGRATION_SEND_COMPLETED.store(true, Ordering::SeqCst);
            log::info!(
                "dispatch_migration_send_complete id={} elapsed_ms={} w22_send_completed_flag=set",
                action.id,
                elapsed_ms
            );
            Ok(ActionOutcome {
                detail: Some(format!("sent to {} ({}ms)", args.target_url, elapsed_ms)),
                pause_window_ms: Some(elapsed_ms),
                // Source-side migration success verb per design §3.1:
                // "complete" (CH gone, guest now running on destination).
                success_status: Some("complete"),
            })
        }
        Ok(info) => {
            // CH is still alive after a "successful" send-migration.
            // This contradicts Q1c; surface as failure with the W1
            // category so operators learn the gate fired.
            log::warn!(
                "migration_send_w1_violation id={} state={}",
                action.id,
                info.state
            );
            Err(format!(
                "w1_violation: send_migration returned 0 but CH state={}",
                info.state
            ))
        }
    }
}

/// Destination-side migration handler. Invokes `vm.receive-migration`
/// against the local empty CH and returns the outcome.
///
/// `docs/design/live-migration-phase-2.md` §2 row 2 + §4.3.2.
///
/// Blocking semantics: blocks until source connects + transfers state,
/// or until CH's TCP retransmit timeout (~3-5 s of network silence —
/// F4 finding). The action loop's current-thread runtime cannot
/// process the cancel verb while this handler is in flight; cancel's
/// dst-kill primitive (PR-C) will need to bypass the action loop
/// (e.g., a sibling thread that watches a separate cancel annotation
/// and SIGKILLs CH directly). Phase 2 PR-B does NOT implement cancel.
async fn dispatch_migration_receive(
    action: &PendingAction,
    api_socket: &Path,
) -> Result<ActionOutcome, String> {
    // SECURITY-S1: listen_url is read from the operator-writable pod
    // annotation args. Phase 3c PR 4 validates it is loopback below under
    // secured mode (same mitigation as the send path). See
    // docs/design/live-migration-phase-3c.md §6.1.
    let args: MigrationReceiveArgs = serde_json::from_value(action.args.clone())
        .map_err(|e| format!("parse migration_receive args: {}", e))?;

    // S1 (secured mode): same as the send path — the listen_url must bind a
    // loopback address (CH receives behind the local stunnel server);
    // refuse anything else so a tampered annotation cannot make CH accept a
    // cleartext connection over the pod network.
    if secured_migration_mode() {
        if let Err(e) = validate_loopback_url(&args.listen_url) {
            return Err(format!("receive_migration: {}", e));
        }
    }

    log::info!(
        "dispatch_migration_receive id={} listen={}",
        action.id,
        args.listen_url
    );

    let timeout =
        std::time::Duration::from_secs(args.timeout_seconds.unwrap_or(DEFAULT_ACTION_TIMEOUT_SECS));
    let client = swift_ch_client::ApiClient::new(api_socket).with_timeout(timeout);

    let started = std::time::Instant::now();
    if let Err(e) = client.receive_migration(&args.listen_url) {
        return Err(format!(
            "receive_migration: {}",
            sanitize_ch_error(&format!("{:?}", e))
        ));
    }

    // W1 completion-gate (load-bearing item C, walkthrough W1).
    // `receive_migration` exit=0 is necessary but not sufficient;
    // the destination CH must actually be Running (Q1c — auto-resume
    // on receive completion). Probe vm_info; require state=Running.
    //
    // `docs/design/live-migration-phase-2.md` §6.1.
    match client.vm_info() {
        Ok(info) if info.state == "Running" => {
            let elapsed_ms = started.elapsed().as_millis().min(u64::MAX as u128) as u64;
            log::info!(
                "dispatch_migration_receive_complete id={} state=Running elapsed_ms={}",
                action.id,
                elapsed_ms
            );
            // Phase 3a D3: propagate guest IP to dst pod annotation.
            // Best-effort; failure is non-fatal — the migration itself
            // succeeded and the controller has a fallback path
            // (post-Resuming reconcile reads src pod's guest-ip if
            // dst's is absent). See `docs/design/live-migration-phase-3a.md`
            // §7.2 D3.
            if let Some(ip) = &args.guest_ip {
                propagate_guest_ip_annotation(ip).await;
            }
            // W16: flip SwiftGuest's GuestRunning condition to True on
            // the destination side. Without this, the controller's
            // Resuming-live handler waits indefinitely for a signal
            // that never arrives (the src-side swiftletd wrote
            // GuestRunning=False/VmStopped at cutover step 1 when its
            // CH exited; receiver-mode swiftletd never overwrites it
            // because main.rs's on_socket_ready callback is skipped in
            // receiver mode and the launcher's post-launch report path
            // only fires when CH exits). Best-effort with the same
            // posture as D3 — if the patch fails, the controller's
            // spec.timeout is the floor.
            //
            // Reads the SwiftGuest name from the dst pod's
            // swift.kubeswift.io/guest label (LabelGuestName from
            // dst_pod.go); the SwiftGuest is named guest.Name throughout
            // its lifetime (canonical pod is dst post-cutover, but the
            // SwiftGuest CR's name is invariant).
            report_guest_running_post_receive().await;
            Ok(ActionOutcome {
                detail: Some(format!(
                    "received on {} ({}ms)",
                    args.listen_url, elapsed_ms
                )),
                pause_window_ms: Some(elapsed_ms),
                // Destination-side migration success verb per design §3.1:
                // "running" (CH state=Running with the migrated guest).
                success_status: Some("running"),
            })
        }
        Ok(info) => {
            log::warn!(
                "migration_receive_w1_violation id={} state={}",
                action.id,
                info.state
            );
            Err(format!(
                "w1_violation: receive_migration returned 0 but CH state={}",
                info.state
            ))
        }
        Err(e) => {
            log::warn!(
                "migration_receive_vm_info_failed id={} err={:?}",
                action.id,
                e
            );
            Err(format!(
                "w1_violation: receive_migration returned 0 but vm_info: {}",
                sanitize_ch_error(&format!("{:?}", e))
            ))
        }
    }
}

/// Destination-side cancel handler — Phase 3a D1
/// (`docs/design/live-migration-phase-3a.md` §7.2).
///
/// Cancel primitive on the destination is **SIGKILL the receiver CH**.
/// Cloud Hypervisor v51.1 has no `vm.cancel-migration` API (Phase 2
/// spike F4 + direct audit of `swift-ch-client::methods`). Closing the
/// dst CH process is what unwinds the in-flight `vm.send-migration`
/// HTTP call on the source side: the TCP stream closes, src CH errors
/// out, src swiftletd writes terminal `migration-status: failed`.
///
/// Mechanism:
///
/// 1. Read CH PID from `<runtime_dir>/ch.pid` (written by
///    `launch::run_ch_receive` immediately after spawn).
/// 2. Defense-in-depth: confirm `/proc/<pid>/exe` resolves to a
///    `cloud-hypervisor` binary. Guards against PID reuse if the
///    receiver CH already exited and the kernel reassigned the PID
///    to an unrelated process.
/// 3. `kill(pid, SIGKILL)`. Synchronous; `EAGAIN`/`ESRCH`/`EPERM`
///    surface as a cancel failure.
/// 4. Return `Err("cancelled")` so the action loop's natural Err
///    branch writes `migration-status: failed` with the cancel
///    action-id and detail `"cancelled"` (the controller's match
///    string per design §2.3 and §4.1).
///
/// The write-once race between this handler's failed-write and D2's
/// watchdog-on-CH-exit failed-write is resolved by D2's
/// already-terminal guard (D2's responsibility, not D1's). D1 just
/// kills + returns Err.
///
/// Cancel is **one-shot per migration**: re-firing the same cancel
/// action-id is idempotent (action loop's `last_completed_id` gate);
/// re-firing with a new cancel action-id while the receiver CH is
/// already gone returns `Err("cancel kill failed: ESRCH")` which the
/// controller treats the same as a successful cancel (the migration
/// is already in a terminal state).
async fn dispatch_migration_cancel(
    action: &PendingAction,
    api_socket: &Path,
) -> Result<ActionOutcome, String> {
    log::info!("dispatch_migration_cancel id={}", action.id);

    // ch.pid lives next to ch.sock in <runtime_dir>.
    let pid_path = api_socket
        .parent()
        .map(|p| p.join("ch.pid"))
        .ok_or_else(|| "cancel kill failed: api_socket has no parent dir".to_string())?;

    let pid = read_ch_pid(&pid_path).map_err(|e| format!("cancel kill failed: {}", e))?;

    if let Err(e) = verify_pid_is_cloud_hypervisor(pid) {
        // PID reuse, /proc unreadable, or CH already gone. Treat as
        // cancel failure; operator can fall back to kubectl delete pod.
        return Err(format!("cancel kill failed: {}", e));
    }

    sigkill(pid).map_err(|e| format!("cancel kill failed: {}", e))?;

    log::info!(
        "dispatch_migration_cancel_killed id={} pid={}",
        action.id,
        pid
    );
    // Err triggers write_migration_status(Failed, detail="cancelled")
    // via handle_namespace's existing Err path.
    Err("cancelled".to_string())
}

/// Read CH's PID from `<runtime_dir>/ch.pid`. Used by the cancel
/// handler. Returned errors are user-facing strings (no source-error
/// chain leaked).
fn read_ch_pid(path: &Path) -> Result<i32, String> {
    let raw =
        std::fs::read_to_string(path).map_err(|e| format!("read {}: {}", path.display(), e))?;
    raw.trim()
        .parse::<i32>()
        .map_err(|e| format!("parse pid from {}: {}", path.display(), e))
}

/// Defense-in-depth: confirm the PID at `pid_path` is actually a
/// `cloud-hypervisor` process via `readlink /proc/<pid>/exe` +
/// basename match. Linux kernel may reuse PIDs; catching reuse here
/// prevents an errant SIGKILL of an unrelated process if the
/// receiver CH already exited.
///
/// `comm` (15-char truncation) is NOT used: "cloud-hypervisor" is 16
/// chars and currently fits; a future rename or wrapper script would
/// silently bypass a comm-based check. `/proc/<pid>/exe` symlink
/// derefs to the real binary path inside the launcher pod's mount
/// namespace, which is reliable here.
fn verify_pid_is_cloud_hypervisor(pid: i32) -> Result<(), String> {
    let exe_link = format!("/proc/{}/exe", pid);
    let target =
        std::fs::read_link(&exe_link).map_err(|e| format!("readlink {}: {}", exe_link, e))?;
    let basename = target
        .file_name()
        .and_then(|s| s.to_str())
        .ok_or_else(|| format!("exe target has no basename: {}", target.display()))?;
    if basename != "cloud-hypervisor" {
        return Err(format!(
            "pid {} exe={} (expected cloud-hypervisor); refusing to SIGKILL",
            pid, basename
        ));
    }
    Ok(())
}

/// Send SIGKILL to `pid`. Wraps `libc::kill` with a Rust-friendly
/// error string. Synchronous; the kernel does not block on signal
/// delivery (the target may take additional time to actually exit
/// and be reaped, but that is the receiver-mode supervisor's
/// concern, not the cancel handler's).
fn sigkill(pid: i32) -> Result<(), String> {
    // SAFETY: libc::kill is FFI; pid_t == i32 on Linux. SIGKILL
    // (signal 9) is well-defined. Return value 0 = success;
    // -1 = error with errno.
    let rc = unsafe { libc::kill(pid, libc::SIGKILL) };
    if rc == 0 {
        Ok(())
    } else {
        let err = std::io::Error::last_os_error();
        Err(format!("kill({}, SIGKILL): {}", pid, err))
    }
}

/// Phase 3a D3 (`docs/design/live-migration-phase-3a.md` §7.2): write
/// the migrated guest's IP to the dst pod's `kubeswift.io/guest-ip`
/// annotation post-resume. Reuses the same annotation key Phase 1's
/// first-boot lease discovery uses (`lease::ANNOTATION_GUEST_IP`).
///
/// Best-effort: any kube-rs failure is logged at WARN and ignored.
/// The migration itself has succeeded by the time we get here (W1
/// gate cleared); IP propagation is informational. The controller's
/// Resuming-phase reconcile observes this annotation and reflects
/// the value into `SwiftMigration.status.targetIP` for operator
/// visibility (PR 1 controller integration).
///
/// Reads `POD_NAMESPACE` / `POD_NAME` from the env (set by the pod
/// spec via downward API, same as `main::report_running`'s env
/// reads). If either is absent, the propagation is skipped — same
/// best-effort posture.
async fn propagate_guest_ip_annotation(ip: &str) {
    let (namespace, pod_name) = match (
        std::env::var("POD_NAMESPACE").ok(),
        std::env::var("POD_NAME").ok(),
    ) {
        (Some(ns), Some(name)) if !ns.is_empty() && !name.is_empty() => (ns, name),
        _ => {
            log::warn!("d3_guest_ip_propagation_skipped reason=POD_NAMESPACE_or_POD_NAME_unset");
            return;
        }
    };
    let client = match crate::kube_client::create_client().await {
        Ok(c) => c,
        Err(e) => {
            log::warn!("d3_kube_client_unavailable: {}", e);
            return;
        }
    };
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client, &namespace);
    let mut annotations = BTreeMap::new();
    annotations.insert(
        crate::lease::ANNOTATION_GUEST_IP.to_string(),
        ip.to_string(),
    );
    let patch = json!({"metadata": {"annotations": annotations}});
    let pp = PatchParams::default();
    match api.patch(&pod_name, &pp, &Patch::Merge(&patch)).await {
        Ok(_) => log::info!("d3_guest_ip_propagated ip={}", ip),
        Err(e) => log::warn!("d3_guest_ip_patch_failed: {}", e),
    }
}

/// W16: post-receive SwiftGuest GuestRunning condition flip on dst.
///
/// In receiver mode, main.rs skips the on_socket_ready callback (CH
/// has no VM at socket-ready time; reporting "guest running" before
/// receive_migration completes would be a lie). The post-launch
/// report path in main.rs only fires when CH exits — which on a
/// successful migration only happens when the guest itself shuts
/// down, not at receive-completion time.
///
/// This helper closes the gap. Called from
/// `dispatch_migration_receive`'s success branch, after the W1 gate
/// confirmed CH state=Running with the migrated guest. It:
///
///   1. Reads `POD_NAMESPACE` / `POD_NAME` from env (downward API).
///   2. Fetches the dst pod and reads `swift.kubeswift.io/guest`
///      label to get the SwiftGuest's name (the canonical-pod name
///      changed at cutover step 1, but the SwiftGuest CR name is
///      invariant; dst pod construction in B2.2 preserves the
///      guest label).
///   3. Patches `SwiftGuest.status.conditions[GuestRunning] = True`
///      via the same `report::report_guest_running` helper used by
///      main.rs's on_socket_ready callback in non-receiver mode.
///
/// Best-effort like D3: any failure (kube client unavailable,
/// missing env, missing label, API error) is logged but doesn't
/// affect the migration outcome — the controller's spec.timeout is
/// the floor for stall detection.
async fn report_guest_running_post_receive() {
    let (namespace, pod_name) = match (
        std::env::var("POD_NAMESPACE").ok(),
        std::env::var("POD_NAME").ok(),
    ) {
        (Some(ns), Some(name)) if !ns.is_empty() && !name.is_empty() => (ns, name),
        _ => {
            log::warn!("w16_guest_running_skipped reason=POD_NAMESPACE_or_POD_NAME_unset");
            return;
        }
    };
    let client = match crate::kube_client::create_client().await {
        Ok(c) => c,
        Err(e) => {
            log::warn!("w16_kube_client_unavailable: {}", e);
            return;
        }
    };
    // Fetch the dst pod to read the swift.kubeswift.io/guest label.
    let pod_api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), &namespace);
    let pod = match pod_api.get(&pod_name).await {
        Ok(p) => p,
        Err(e) => {
            log::warn!("w16_pod_get_failed: {}", e);
            return;
        }
    };
    let guest_name = match extract_guest_name_from_labels(pod.metadata.labels.as_ref()) {
        Some(g) => g,
        None => {
            log::warn!("w16_guest_label_missing pod={}/{}", namespace, pod_name);
            return;
        }
    };
    match crate::report::report_guest_running(&client, &namespace, &guest_name, true, None).await {
        Ok(()) => log::info!(
            "w16_guest_running_reported guest={}/{}",
            namespace,
            guest_name
        ),
        Err(e) => log::warn!(
            "w16_guest_running_patch_failed guest={}/{} err={}",
            namespace,
            guest_name,
            e
        ),
    }
}

/// W16 helper: extract the SwiftGuest's name from a launcher pod's
/// labels. Pure function (no I/O); unit-testable in isolation.
///
/// The label key (`swift.kubeswift.io/guest`) is set on the dst pod
/// at construction time in B2.2's `mergeLabelsForDst`. The
/// SwiftGuest CR's name is invariant through cutover (canonical pod
/// changes; SwiftGuest CR doesn't), so this is the source of truth
/// for "which SwiftGuest does this dst launcher pod represent."
///
/// Returns `None` if labels are absent, the key is missing, or the
/// value is empty — caller logs and returns without patching.
fn extract_guest_name_from_labels(
    labels: Option<&std::collections::BTreeMap<String, String>>,
) -> Option<String> {
    labels
        .and_then(|l| l.get("swift.kubeswift.io/guest"))
        .filter(|v| !v.is_empty())
        .cloned()
}

/// Sanitize a CH error string into a category. Defensive against
/// future failure modes that could leak partial guest state through
/// error strings — Phase 2 spike S3 finding. Conservative default:
/// pattern-match on the leading error variant; collapse longer
/// messages into a category token.
///
/// `docs/design/live-migration-phase-2.md` §3.1 + §6.2.
fn sanitize_ch_error(raw: &str) -> &'static str {
    // Match against the leading variant produced by ApiError's
    // Debug impl. The variants are stable in swift-ch-client.
    if raw.contains("Connect") {
        "connection_refused"
    } else if raw.contains("Configure") {
        "socket_configure_failed"
    } else if raw.contains("Status") {
        if raw.contains("400") {
            "bad_request"
        } else if raw.contains("500") {
            "internal_server_error"
        } else {
            "ch_status_error"
        }
    } else if raw.contains("Read") || raw.contains("Write") {
        "transport_error"
    } else if raw.contains("Malformed") {
        "malformed_response"
    } else {
        "ch_error"
    }
}

/// Args parsed from `kubeswift.io/snapshot-action-args` for the
/// capture verb. Defaults match the controller's documented behavior
/// so that empty/missing fields don't surprise the operator.
#[derive(Debug, serde::Deserialize)]
struct CaptureArgs {
    /// `file:///<absolute-path>/` — destination directory CH writes
    /// `config.json`, `state.json`, and `memory-ranges` into. The
    /// directory must already exist and be writable by the launcher
    /// (the controller pre-seeds it on the source node).
    destination_url: String,
    /// Override for the per-call HTTP timeout in seconds. The
    /// controller derives this from VM RAM size (≈2.8s/GiB on
    /// Longhorn per the Phase 0 spike) and a generosity factor. If
    /// missing, [`DEFAULT_ACTION_TIMEOUT_SECS`] applies.
    #[serde(default)]
    timeout_seconds: Option<u64>,
    /// If true (default), resume the VM after the snapshot completes.
    /// `false` leaves the VM Paused for operator inspection — the
    /// controller still considers the SwiftSnapshot Ready in this
    /// case.
    #[serde(default = "default_true")]
    resume_after_snapshot: bool,
}

fn default_true() -> bool {
    true
}

/// Capture handler: pause → snapshot → (optional) resume.
///
/// Sequence in detail:
///
///   1. `t1 = now()`; issue pause(). On error, no state to roll back —
///      the VM is still Running and the controller will retry.
///   2. `t2 = now()`. The VM is now in Paused state.
///   3. Issue snapshot(destination_url). On error, attempt resume()
///      best-effort so the VM doesn't sit Paused forever; the action
///      still reports failed.
///   4. `t3 = now()`. The snapshot dir contains config.json / state.json
///      / memory-ranges.
///   5. If `resume_after_snapshot`: issue resume(); `t4 = now()`. On
///      error, log and continue — the controller decides what to do
///      with a paused-but-snapshot-complete state.
///   6. Return ActionOutcome with `pause_window_ms = t4 - t2` (or
///      `t3 - t2` when resume_after_snapshot=false).
async fn dispatch_capture(
    action: &PendingAction,
    api_socket: &Path,
) -> Result<ActionOutcome, String> {
    let args: CaptureArgs = serde_json::from_value(action.args.clone())
        .map_err(|e| format!("parse capture args: {}", e))?;

    log::info!(
        "dispatch_snapshot_capture id={} dest={} resume_after={}",
        action.id,
        args.destination_url,
        args.resume_after_snapshot
    );

    // CH's vm.snapshot endpoint requires the destination directory to
    // exist AND be empty. It writes config.json, state.json, and
    // memory-ranges into the dir without auto-creating it, and refuses
    // when those files already exist with "File exists (os error 17)".
    //
    // The SwiftSnapshot controller owns naming the path (under
    // /var/lib/kubeswift/snapshots/<ns>-<name>/) but cannot mkdir or
    // rm on the source node's filesystem itself; the launcher pod has
    // the hostPath mount and is the only place this can happen.
    //
    // We wipe-and-recreate before each capture so:
    //   - A stale prior attempt that didn't get cleaned (cleanup
    //     finalizer didn't run, partial failure left files) doesn't
    //     block the new capture.
    //   - An idempotent retry (same action-id, same destination) gets
    //     a fresh empty dir each time.
    //
    // This is safe because the action loop is single-threaded per
    // launcher pod (one capture-action in flight at a time, gated by
    // action-id) and across launchers each SwiftSnapshot owns a
    // unique destination subdirectory.
    if let Some(local_path) = args.destination_url.strip_prefix("file://") {
        let local_path = local_path.trim_end_matches('/');
        // remove_dir_all on a missing path returns an error we ignore;
        // the create_dir_all below is the only authoritative step.
        let _ = std::fs::remove_dir_all(local_path);
        if let Err(e) = std::fs::create_dir_all(local_path) {
            return Err(format!("create destination dir {}: {}", local_path, e));
        }
    }

    let timeout =
        std::time::Duration::from_secs(args.timeout_seconds.unwrap_or(DEFAULT_ACTION_TIMEOUT_SECS));
    let client = swift_ch_client::ApiClient::new(api_socket).with_timeout(timeout);

    // Pause and stamp the start of the pause window.
    client.pause().map_err(|e| format!("pause: {}", e))?;
    let pause_started = std::time::Instant::now();

    // Take the snapshot. If it fails, best-effort resume so the VM
    // doesn't sit Paused forever, then surface the original error.
    if let Err(e) = client.snapshot(&args.destination_url) {
        log::warn!(
            "snapshot_failed id={}: {} — attempting resume to recover VM",
            action.id,
            e
        );
        if let Err(re) = client.resume() {
            log::error!(
                "resume_after_snapshot_failure_failed id={}: {}",
                action.id,
                re
            );
        }
        return Err(format!("snapshot: {}", e));
    }

    // Resume (or not, per args). pause_window includes the resume call
    // itself when we resume — that's the time the VM is actually frozen
    // from the guest's point of view.
    let pause_window = if args.resume_after_snapshot {
        client.resume().map_err(|e| format!("resume: {}", e))?;
        pause_started.elapsed()
    } else {
        // Caller wants the VM left Paused for inspection. Capture the
        // pause window up to snapshot completion only.
        pause_started.elapsed()
    };
    let pause_window_ms = pause_window.as_millis().min(u64::MAX as u128) as u64;
    log::info!(
        "dispatch_snapshot_capture_complete id={} pause_window_ms={} resumed={}",
        action.id,
        pause_window_ms,
        args.resume_after_snapshot
    );

    let detail = if args.resume_after_snapshot {
        format!(
            "captured to {} ({}ms pause window, resumed)",
            args.destination_url, pause_window_ms
        )
    } else {
        format!(
            "captured to {} ({}ms pause window, left paused per resume_after_snapshot=false)",
            args.destination_url, pause_window_ms
        )
    };
    Ok(ActionOutcome {
        detail: Some(detail),
        pause_window_ms: Some(pause_window_ms),
        success_status: None, // Snapshot uses the default StatusKind::Ready ("ready").
    })
}

/// Args for the standalone resume action. Used by the restore flow
/// (commit 7) to bring a Paused VM up after `--restore` completes.
#[derive(Debug, serde::Deserialize)]
struct ResumeArgs {
    #[serde(default)]
    timeout_seconds: Option<u64>,
}

/// Standalone resume handler. Idempotent at the CH level — resuming
/// an already-running VM is a no-op (CH treats this gracefully).
async fn dispatch_resume(
    action: &PendingAction,
    api_socket: &Path,
) -> Result<ActionOutcome, String> {
    let args: ResumeArgs = if action.args.is_null() {
        ResumeArgs {
            timeout_seconds: None,
        }
    } else {
        serde_json::from_value(action.args.clone())
            .map_err(|e| format!("parse resume args: {}", e))?
    };
    log::info!("dispatch_snapshot_resume id={}", action.id);
    let timeout =
        std::time::Duration::from_secs(args.timeout_seconds.unwrap_or(DEFAULT_ACTION_TIMEOUT_SECS));
    let client = swift_ch_client::ApiClient::new(api_socket).with_timeout(timeout);
    client.resume().map_err(|e| format!("resume: {}", e))?;
    Ok(ActionOutcome::detail("resumed"))
}

/// Patch the launcher pod's status annotations for the snapshot
/// namespace. Always merge-patches the four keys together so a status
/// rewrite never leaves stale detail or stale pause-window from a
/// prior status. The pause-window key is snapshot-specific.
///
/// The migration namespace has its own writer
/// ([`write_migration_status`]) per `docs/design/live-migration-phase-2.md`
/// §4.5 — keeping the writers separate avoids parameterizing across
/// namespace-specific status fields.
pub async fn write_status(
    client: &Client,
    namespace: &str,
    pod_name: &str,
    action_id: &str,
    status: StatusKind,
    detail: Option<&str>,
    pause_window_ms: Option<u64>,
) -> Result<(), kube::Error> {
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let mut annotations = BTreeMap::new();
    annotations.insert(STATUS_KEY.to_string(), status.as_str().to_string());
    annotations.insert(STATUS_ID_KEY.to_string(), action_id.to_string());
    // Detail and pause-window are overwritten on every write so they
    // never lag the status. Empty string is sent rather than omitted
    // so the merge-patch actually clears any prior value.
    annotations.insert(
        STATUS_DETAIL_KEY.to_string(),
        detail.unwrap_or("").to_string(),
    );
    annotations.insert(
        STATUS_PAUSE_WINDOW_MS_KEY.to_string(),
        pause_window_ms.map(|n| n.to_string()).unwrap_or_default(),
    );
    let patch = json!({
        "metadata": {
            "annotations": annotations
        }
    });
    let pp = PatchParams::default();
    api.patch(pod_name, &pp, &Patch::Merge(&patch)).await?;
    Ok(())
}

/// Patch the launcher pod's status annotations for the migration
/// namespace. Mirrors [`write_status`] but writes the migration keys
/// (action-id-mirror, status, status-detail, pause-window-ms in the
/// migration namespace).
///
/// Status-id-paired-write discipline (`docs/design/live-migration-phase-2.md`
/// §3.2): all four annotations land in a single Patch::Merge call, so
/// the controller never observes a `migration-status` without its
/// associated `migration-status-id`. This is the snapshot Phase 2
/// Bug 14 precedent applied prophylactically.
pub async fn write_migration_status(
    client: &Client,
    namespace: &str,
    pod_name: &str,
    action_id: &str,
    status: StatusKind,
    detail: Option<&str>,
    pause_window_ms: Option<u64>,
) -> Result<(), kube::Error> {
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let mut annotations = BTreeMap::new();
    annotations.insert(
        MIGRATION_STATUS_KEY.to_string(),
        status.as_str().to_string(),
    );
    annotations.insert(MIGRATION_STATUS_ID_KEY.to_string(), action_id.to_string());
    annotations.insert(
        MIGRATION_STATUS_DETAIL_KEY.to_string(),
        detail.unwrap_or("").to_string(),
    );
    annotations.insert(
        MIGRATION_PAUSE_WINDOW_MS_KEY.to_string(),
        pause_window_ms.map(|n| n.to_string()).unwrap_or_default(),
    );
    let patch = json!({
        "metadata": {
            "annotations": annotations
        }
    });
    let pp = PatchParams::default();
    api.patch(pod_name, &pp, &Patch::Merge(&patch)).await?;
    Ok(())
}

// ─── Phase 3a D2 (`docs/design/live-migration-phase-3a.md` §7.2) ─────────────
//
// Auto-write `migration-status: failed` on abnormal CH listener exit.
//
// Without this, if the dst CH listener process exits abnormally during
// migration (panic, SIGKILL by something inside the pod, kernel OOM),
// the controller waits indefinitely on a terminal annotation that
// never arrives. D2 closes the gap: when `launch::run` returns
// abnormally and we were a migration receiver, swiftletd patches the
// dst pod's migration-status to `failed` paired with the last-observed
// migration-action-id and a detail string describing the exit.
//
// D2 is the canonical write-once guard for the D1+D2 race
// (`docs/design/live-migration-phase-3a.md` §7.2 D1 commentary): D1's
// SIGKILL-then-Err path also produces a `migration-status: failed`
// write, and the two writes race on the apiserver. D2's `decide_watchdog`
// inspects the freshly-fetched annotations and skips when a terminal
// failure is already recorded for the same action-id (the LWW
// scenario the architect-discipline review accepted as good-enough
// for Phase 3a).

/// Output of [`decide_watchdog`] — what `write_migration_failed_on_abnormal_exit`
/// should do given the current pod annotations.
#[derive(Debug, Clone, PartialEq)]
pub enum WatchdogDecision {
    /// Skip the watchdog write. Carries a human-readable reason for
    /// logging.
    Skip(&'static str),
    /// Write `migration-status: failed` paired with this action-id and
    /// detail string.
    WriteFailed { action_id: String, detail: String },
}

/// Pure-function watchdog decision logic. Tested in isolation — no
/// kube-rs dependency, no I/O. The kube wrapper
/// [`write_migration_failed_on_abnormal_exit`] calls this with
/// freshly-fetched pod annotations.
///
/// `exit_detail` is appended to the human-readable detail string
/// (typically the `exit_status.code()` or launch::run error message).
///
/// Skip rules (`docs/design/live-migration-phase-3a.md` §7.2 D2):
///
/// 1. No `migration-action-id` annotation, or empty — there's no
///    active migration action to fail. Skip.
/// 2. `migration-status` is already `failed` for the SAME action-id —
///    a terminal failure was already recorded (D1 won the race, or a
///    prior dispatch's Err path wrote it). Skip to preserve write-once
///    semantics.
///
/// Otherwise: emit a write paired with the last-observed action-id.
/// This handles the post-success-then-CH-death case (`status: running`
/// with matching id, then CH died) — D2 surfaces a NEW failure event
/// because the dst guest is now dead and the migration's effect has
/// been undone. Operationally correct.
pub fn decide_watchdog(
    annotations: &BTreeMap<String, String>,
    exit_detail: &str,
) -> WatchdogDecision {
    let action_id = match annotations.get(MIGRATION_ACTION_ID_KEY) {
        Some(id) if !id.is_empty() => id.clone(),
        _ => return WatchdogDecision::Skip("no active migration-action-id"),
    };

    let status = annotations
        .get(MIGRATION_STATUS_KEY)
        .map(String::as_str)
        .unwrap_or("");
    let status_id = annotations
        .get(MIGRATION_STATUS_ID_KEY)
        .map(String::as_str)
        .unwrap_or("");

    if status == "failed" && status_id == action_id {
        return WatchdogDecision::Skip("terminal failed status already present for action-id");
    }

    WatchdogDecision::WriteFailed {
        action_id,
        detail: format!("destination listener exited abnormally: {}", exit_detail),
    }
}

/// kube wrapper: read pod annotations, run [`decide_watchdog`], and
/// write `migration-status: failed` when the decision says to.
///
/// Called from `main.rs` after `launch::run` returns abnormally on a
/// migration-receiver pod. Best-effort: a kube client failure here is
/// logged and ignored — the controller's `spec.timeout` is the floor
/// and will eventually transition the SwiftMigration to Failed even
/// if D2's write doesn't land.
pub async fn write_migration_failed_on_abnormal_exit(
    namespace: &str,
    pod_name: &str,
    exit_detail: &str,
) {
    let client = match crate::kube_client::create_client().await {
        Ok(c) => c,
        Err(e) => {
            log::warn!(
                "watchdog_kube_client_unavailable: {} (controller's spec.timeout is the floor)",
                e
            );
            return;
        }
    };
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let pod = match api.get(pod_name).await {
        Ok(p) => p,
        Err(e) => {
            log::warn!("watchdog_pod_get_failed: {}", e);
            return;
        }
    };
    let annotations: BTreeMap<String, String> = pod
        .metadata
        .annotations
        .clone()
        .unwrap_or_default()
        .into_iter()
        .collect();

    match decide_watchdog(&annotations, exit_detail) {
        WatchdogDecision::Skip(reason) => {
            log::info!("watchdog_skip reason={}", reason);
        }
        WatchdogDecision::WriteFailed { action_id, detail } => {
            log::warn!("watchdog_write_failed id={} detail={}", action_id, detail);
            if let Err(e) = write_migration_status(
                &client,
                namespace,
                pod_name,
                &action_id,
                StatusKind::Failed,
                Some(&detail),
                None,
            )
            .await
            {
                log::error!("watchdog_status_write_failed: {}", e);
            }
        }
    }
}

/// Spawn the action loop on a dedicated thread with its own tokio
/// runtime. Mirrors the lease-poller pattern in [`crate::lease`].
pub fn spawn_action_loop(namespace: String, pod_name: String, api_socket: PathBuf) {
    std::thread::spawn(move || {
        let rt = match tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
        {
            Ok(r) => r,
            Err(e) => {
                log::error!("action_loop_runtime_failed: {}", e);
                return;
            }
        };
        rt.block_on(action_loop(namespace, pod_name, api_socket));
    });
}

async fn action_loop(namespace: String, pod_name: String, api_socket: PathBuf) {
    let client = match kube_client::create_client().await {
        Ok(c) => c,
        Err(e) => {
            log::warn!(
                "action_loop_kube_client_unavailable: {} — disabling annotation handler",
                e
            );
            return;
        }
    };
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), &namespace);
    let mut state = ActionState::default();
    log::info!("action_loop_started pod={}/{}", namespace, pod_name);
    loop {
        match api.get(&pod_name).await {
            Ok(pod) => {
                let annotations = pod.metadata.annotations.clone().unwrap_or_default();
                let annotations: BTreeMap<String, String> = annotations.into_iter().collect();
                handle_pod_state(
                    &client,
                    &namespace,
                    &pod_name,
                    &api_socket,
                    &mut state,
                    &annotations,
                )
                .await;
            }
            Err(e) => {
                log::warn!("action_loop_get_pod_err: {}", e);
            }
        }
        tokio::time::sleep(POLL_INTERVAL).await;
    }
}

/// Detect whether a namespace has an active action annotation
/// (non-empty `action_key`). Used to detect concurrent snapshot +
/// migration actions for mutual rejection.
fn is_namespace_active(annotations: &BTreeMap<String, String>, keys: &KeySet) -> bool {
    matches!(annotations.get(keys.action_key), Some(v) if !v.is_empty())
}

/// Read the action-id for a namespace if both action and id are set.
/// Returns None if either is missing (the malformed case decide()
/// would surface separately).
fn namespace_action_id<'a>(
    annotations: &'a BTreeMap<String, String>,
    keys: &KeySet,
) -> Option<&'a str> {
    let id = annotations.get(keys.action_id_key)?.as_str();
    if id.is_empty() {
        None
    } else {
        Some(id)
    }
}

async fn handle_pod_state(
    client: &Client,
    namespace: &str,
    pod_name: &str,
    api_socket: &Path,
    state: &mut ActionState,
    annotations: &BTreeMap<String, String>,
) {
    let snap_active = is_namespace_active(annotations, &SNAPSHOT_KEYS);
    let mig_active = is_namespace_active(annotations, &MIGRATION_KEYS);

    // Mutual rejection across namespaces (`docs/design/live-migration-phase-2.md`
    // §4.2.1): if both namespaces have non-empty action annotations
    // AND neither has an in-flight action of its own (which would
    // mean we already accepted one before the other arrived), reject
    // BOTH. The controller learns immediately that one operation
    // must complete before the other starts. Rejection is per-tick:
    // when the conflicting annotation clears, the next tick's
    // `decide` accepts normally — controllers MUST NOT clear
    // annotations on observed `rejected` status.
    if snap_active
        && mig_active
        && state.snapshot.in_flight.is_none()
        && state.migration.in_flight.is_none()
    {
        if let Some(id) = namespace_action_id(annotations, &SNAPSHOT_KEYS) {
            log::warn!("action_reject_concurrent namespace=snapshot id={}", id);
            if let Err(e) = write_status(
                client,
                namespace,
                pod_name,
                id,
                StatusKind::Rejected,
                Some("concurrent action with migration rejected"),
                None,
            )
            .await
            {
                log::error!("action_status_write_failed: {}", e);
            }
        }
        if let Some(id) = namespace_action_id(annotations, &MIGRATION_KEYS) {
            log::warn!("action_reject_concurrent namespace=migration id={}", id);
            if let Err(e) = write_migration_status(
                client,
                namespace,
                pod_name,
                id,
                StatusKind::Rejected,
                Some("concurrent action with snapshot rejected"),
                None,
            )
            .await
            {
                log::error!("action_status_write_failed: {}", e);
            }
        }
        return;
    }

    // Per-namespace processing. Each namespace's writer/state is
    // independent; calling them sequentially is safe because the
    // action loop is single-threaded per pod.
    handle_namespace(
        client,
        namespace,
        pod_name,
        api_socket,
        &mut state.snapshot,
        &SNAPSHOT_KEYS,
        annotations,
        write_status_fn,
    )
    .await;

    // Secured mode drops the ack-gate from the migration KeySet (PR 4 §6.2):
    // under mTLS the plaintext-ack escape-hatch is moot, so decide() must
    // not reject the secured flow for a missing ack.
    let mig_keys = migration_keys();
    handle_namespace(
        client,
        namespace,
        pod_name,
        api_socket,
        &mut state.migration,
        &mig_keys,
        annotations,
        write_migration_status_fn,
    )
    .await;
}

/// Type alias for the namespace-specific status writer used by
/// `handle_namespace`. Callers pass either `write_status_fn` (snapshot)
/// or `write_migration_status_fn` (migration).
type StatusWriter = for<'a> fn(
    &'a Client,
    &'a str,
    &'a str,
    &'a str,
    StatusKind,
    Option<&'a str>,
    Option<u64>,
) -> std::pin::Pin<
    Box<dyn std::future::Future<Output = Result<(), kube::Error>> + Send + 'a>,
>;

fn write_status_fn<'a>(
    client: &'a Client,
    ns: &'a str,
    pod: &'a str,
    id: &'a str,
    status: StatusKind,
    detail: Option<&'a str>,
    pause: Option<u64>,
) -> std::pin::Pin<Box<dyn std::future::Future<Output = Result<(), kube::Error>> + Send + 'a>> {
    Box::pin(write_status(client, ns, pod, id, status, detail, pause))
}

fn write_migration_status_fn<'a>(
    client: &'a Client,
    ns: &'a str,
    pod: &'a str,
    id: &'a str,
    status: StatusKind,
    detail: Option<&'a str>,
    pause: Option<u64>,
) -> std::pin::Pin<Box<dyn std::future::Future<Output = Result<(), kube::Error>> + Send + 'a>> {
    Box::pin(write_migration_status(
        client, ns, pod, id, status, detail, pause,
    ))
}

/// Pre-dispatch status verb for a freshly-accepted action.
///
/// `handle_namespace` writes a status annotation between
/// `ActionDecision::Accept` and the dispatcher returning. For most
/// actions (snapshot, restore, migration-send, migration-cancel) the
/// generic `StatusKind::Running` ("running") is the right pre-dispatch
/// verb — operationally meaning "swiftletd has accepted the action and
/// is now executing it."
///
/// For `MigrationReceive` we override to `Custom("receive-ready")`
/// per Phase 3b design doc §5.1: the controller's PreparingLive
/// phase gate-observes this annotation to know it can safely patch
/// `migration-action: send` on the source pod. The annotation fires
/// here (a few microseconds before `client.receive_migration()` is
/// issued to CH) rather than from inside the dispatcher because
/// threading the kube client into `dispatch()` would have grown the
/// signature churn past the operator-set sanity-check budget
/// (see Phase 3b PR 1 prompt; ~20 test-site callers of
/// `dispatch()`). Operationally the few-microsecond gap between
/// the annotation write and the actual TCP listener open is dwarfed
/// by the controller's ~540ms reconcile→patch-send round-trip
/// (Phase 3b spike Q1), so no race surfaces on cluster timing.
///
/// Commit D extended the match with
/// `MigrationSend => Custom("sending")` per design doc §5.2.
fn pre_dispatch_status(kind: &ActionKind) -> StatusKind {
    match kind {
        ActionKind::MigrationReceive => StatusKind::Custom("receive-ready"),
        ActionKind::MigrationSend => StatusKind::Custom("sending"),
        _ => StatusKind::Running,
    }
}

/// Compute the heuristic progress percentage per Phase 3b design doc
/// §5.4. Pure function — no I/O, no state. The emitter task wraps
/// this with annotation patching at ~5s intervals.
///
///   raw = 100 * elapsed_s / expected_s
///   capped = clamp(raw, 0, 95)
///
/// The cap at 95% (NOT 100%) is intentional: the transition from "95%
/// from the heuristic" to "vm.send-migration RPC returned successfully"
/// is the next discrete observable event, and we don't want operators
/// seeing "100% complete" while the RPC is still in finalize. The
/// floor at 0 is defensive against clock drift on swiftletd startup.
///
/// Defensive: if `expected_s` is non-positive (e.g., guest RAM
/// unavailable, controller didn't set the args), return 0 — the
/// emitter task should also skip emission entirely in this case;
/// the helper's defensive zero is the fall-through.
pub fn compute_progress_estimate(elapsed_s: f64, expected_s: f64) -> i64 {
    if expected_s <= 0.0 || !expected_s.is_finite() || !elapsed_s.is_finite() {
        return 0;
    }
    let raw = 100.0 * elapsed_s / expected_s;
    raw.clamp(0.0, 95.0) as i64
}

#[allow(clippy::too_many_arguments)]
async fn handle_namespace(
    client: &Client,
    namespace: &str,
    pod_name: &str,
    api_socket: &Path,
    state: &mut NamespaceState,
    keys: &KeySet,
    annotations: &BTreeMap<String, String>,
    write: StatusWriter,
) {
    let last = state.last_completed_id.as_deref();
    let in_flight = state.in_flight.as_ref().map(|p| p.id.as_str());
    match decide(annotations, keys, last, in_flight) {
        ActionDecision::Idle | ActionDecision::Idempotent { .. } => {}
        ActionDecision::RejectMalformed(reason) => {
            log::warn!(
                "action_reject_malformed namespace={}: {}",
                keys.namespace,
                reason
            );
            // No action-id to mirror — we can't write a meaningful
            // status. Surface in logs only; the controller will learn
            // from the absent status and time out.
        }
        ActionDecision::RejectInFlight {
            incoming_id,
            current_id,
        } => {
            log::warn!(
                "action_reject_inflight namespace={} incoming={} current={}",
                keys.namespace,
                incoming_id,
                current_id
            );
            if let Err(e) = write(
                client,
                namespace,
                pod_name,
                &incoming_id,
                StatusKind::Rejected,
                Some(&format!(
                    "rejected: action {} already in flight",
                    current_id
                )),
                None,
            )
            .await
            {
                log::error!("action_status_write_failed: {}", e);
            }
        }
        ActionDecision::RejectAckMissing {
            incoming_id,
            ack_key,
        } => {
            // Phase 2 plaintext-transport ack gate fired (S2 mitigation).
            // The action-id IS present; emit a Rejected status so the
            // controller can correlate the rejection to its specific
            // attempt (mirror-write order from §8.2.1: status-id is
            // written paired with the status in the same Patch::Merge).
            log::warn!(
                "action_reject_ack_missing namespace={} incoming={} ack_key={}",
                keys.namespace,
                incoming_id,
                ack_key
            );
            if let Err(e) = write(
                client,
                namespace,
                pod_name,
                &incoming_id,
                StatusKind::Rejected,
                Some("phase2_plaintext_ack_missing"),
                None,
            )
            .await
            {
                log::error!("action_status_write_failed: {}", e);
            }
        }
        ActionDecision::Accept(pending) => {
            log::info!(
                "action_accept namespace={} kind={:?} id={}",
                keys.namespace,
                pending.kind,
                pending.id
            );
            state.in_flight = Some(pending.clone());
            if let Err(e) = write(
                client,
                namespace,
                pod_name,
                &pending.id,
                pre_dispatch_status(&pending.kind),
                None,
                None,
            )
            .await
            {
                log::error!("action_status_write_failed: {}", e);
            }
            let result = dispatch(&pending, api_socket).await;
            let (status, detail, pause_window_ms) = match result {
                Ok(outcome) => {
                    // `success_status` is None for snapshot (defaults to
                    // StatusKind::Ready → "ready"); migration source
                    // sets Some("complete"); migration destination sets
                    // Some("running"). See ActionOutcome.success_status.
                    let status = outcome
                        .success_status
                        .map_or(StatusKind::Ready, StatusKind::Custom);
                    (status, outcome.detail, outcome.pause_window_ms)
                }
                Err(d) => (StatusKind::Failed, Some(d), None),
            };
            if let Err(e) = write(
                client,
                namespace,
                pod_name,
                &pending.id,
                status,
                detail.as_deref(),
                pause_window_ms,
            )
            .await
            {
                log::error!("action_status_write_failed: {}", e);
            }
            // W23: signal main.rs that the terminal write for a
            // MigrationSend action has completed (success or failure).
            // main.rs's W22 success branch awaits this before exiting,
            // ensuring the apiserver actually received the
            // `migration-status: complete` annotation patch before the
            // process exits and kills this thread mid-flight.
            //
            // Fired AFTER the write call returns — the whole point is
            // ensuring the write lands. Send-failure on the channel is
            // benign (notify_one stores at most one permit; idempotent
            // if no awaiter is parked yet).
            //
            // Scoped to MigrationSend only because that's the action
            // whose completion triggers main's exit. Other actions
            // (snapshot, restore, MigrationReceive, MigrationCancel)
            // don't have main.rs exiting on their completion so they
            // don't need the signal. Defensive completeness: fire on
            // both success ("complete") and failure paths since a
            // future code change might add an exit-on-send-failure
            // path; cheap to fire either way.
            if pending.kind == ActionKind::MigrationSend {
                migration_send_terminal_signal().notify_one();
                log::info!("w23_terminal_write_signal_fired id={}", pending.id);
            }
            state.in_flight = None;
            state.last_completed_id = Some(pending.id);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ann(kvs: &[(&str, &str)]) -> BTreeMap<String, String> {
        kvs.iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    #[test]
    fn no_action_annotation_is_idle() {
        let a = ann(&[]);
        assert_eq!(decide(&a, &SNAPSHOT_KEYS, None, None), ActionDecision::Idle);
    }

    #[test]
    fn empty_action_value_is_idle() {
        let a = ann(&[(ACTION_KEY, "")]);
        assert_eq!(decide(&a, &SNAPSHOT_KEYS, None, None), ActionDecision::Idle);
    }

    #[test]
    fn action_without_id_is_malformed() {
        let a = ann(&[(ACTION_KEY, "capture")]);
        match decide(&a, &SNAPSHOT_KEYS, None, None) {
            ActionDecision::RejectMalformed(_) => {}
            other => panic!("expected RejectMalformed, got {:?}", other),
        }
    }

    #[test]
    fn action_with_id_no_state_is_accepted() {
        let a = ann(&[(ACTION_KEY, "capture"), (ACTION_ID_KEY, "snap1-r42")]);
        match decide(&a, &SNAPSHOT_KEYS, None, None) {
            ActionDecision::Accept(p) => {
                assert_eq!(p.kind, ActionKind::SnapshotCapture);
                assert_eq!(p.id, "snap1-r42");
            }
            other => panic!("expected Accept, got {:?}", other),
        }
    }

    #[test]
    fn action_with_id_matching_last_completed_is_idempotent() {
        let a = ann(&[(ACTION_KEY, "capture"), (ACTION_ID_KEY, "snap1-r42")]);
        match decide(&a, &SNAPSHOT_KEYS, Some("snap1-r42"), None) {
            ActionDecision::Idempotent { id } => assert_eq!(id, "snap1-r42"),
            other => panic!("expected Idempotent, got {:?}", other),
        }
    }

    #[test]
    fn action_with_id_matching_in_flight_is_idempotent() {
        let a = ann(&[(ACTION_KEY, "capture"), (ACTION_ID_KEY, "snap1-r42")]);
        match decide(&a, &SNAPSHOT_KEYS, None, Some("snap1-r42")) {
            ActionDecision::Idempotent { id } => assert_eq!(id, "snap1-r42"),
            other => panic!("expected Idempotent, got {:?}", other),
        }
    }

    #[test]
    fn different_id_while_in_flight_is_rejected() {
        let a = ann(&[(ACTION_KEY, "capture"), (ACTION_ID_KEY, "snap2-r99")]);
        match decide(&a, &SNAPSHOT_KEYS, None, Some("snap1-r42")) {
            ActionDecision::RejectInFlight {
                incoming_id,
                current_id,
            } => {
                assert_eq!(incoming_id, "snap2-r99");
                assert_eq!(current_id, "snap1-r42");
            }
            other => panic!("expected RejectInFlight, got {:?}", other),
        }
    }

    #[test]
    fn unknown_action_verb_accepted_for_dispatch_to_reject() {
        let a = ann(&[(ACTION_KEY, "frobnicate"), (ACTION_ID_KEY, "x-1")]);
        match decide(&a, &SNAPSHOT_KEYS, None, None) {
            ActionDecision::Accept(p) => {
                assert_eq!(p.kind, ActionKind::Unknown("frobnicate".to_string()));
            }
            other => panic!("expected Accept of Unknown, got {:?}", other),
        }
    }

    #[test]
    fn args_parsed_when_present() {
        let a = ann(&[
            (ACTION_KEY, "capture"),
            (ACTION_ID_KEY, "snap1-r42"),
            (
                ACTION_ARGS_KEY,
                r#"{"destination_url":"file:///snap1","include_memory":true}"#,
            ),
        ]);
        match decide(&a, &SNAPSHOT_KEYS, None, None) {
            ActionDecision::Accept(p) => {
                assert_eq!(p.args["destination_url"], "file:///snap1");
                assert_eq!(p.args["include_memory"], true);
            }
            other => panic!("expected Accept, got {:?}", other),
        }
    }

    #[test]
    fn malformed_args_become_null_not_error() {
        let a = ann(&[
            (ACTION_KEY, "capture"),
            (ACTION_ID_KEY, "snap1-r42"),
            (ACTION_ARGS_KEY, "this is not json"),
        ]);
        match decide(&a, &SNAPSHOT_KEYS, None, None) {
            ActionDecision::Accept(p) => {
                assert_eq!(p.args, serde_json::Value::Null);
            }
            other => panic!("expected Accept with null args, got {:?}", other),
        }
    }

    #[test]
    fn from_verb_maps_known_and_unknown() {
        assert_eq!(
            ActionKind::from_verb("capture"),
            ActionKind::SnapshotCapture
        );
        assert_eq!(ActionKind::from_verb("resume"), ActionKind::SnapshotResume);
        assert_eq!(ActionKind::from_verb("prepare"), ActionKind::RestorePrepare);
        assert_eq!(
            ActionKind::from_verb("xyz"),
            ActionKind::Unknown("xyz".to_string())
        );
    }

    #[test]
    fn status_kind_strings_are_lowercase() {
        assert_eq!(StatusKind::Running.as_str(), "running");
        assert_eq!(StatusKind::Ready.as_str(), "ready");
        assert_eq!(StatusKind::Failed.as_str(), "failed");
        assert_eq!(StatusKind::Rejected.as_str(), "rejected");
    }

    // Phase 3b PR 1 Commit C — pre-dispatch annotation verb-
    // specialization. Per design doc §5.1, the controller's
    // PreparingLive phase gate-observes `migration-status:
    // receive-ready` before patching `migration-action: send` on the
    // source pod. This helper is the emission site.
    #[test]
    fn pre_dispatch_status_receive_emits_receive_ready() {
        assert_eq!(
            pre_dispatch_status(&ActionKind::MigrationReceive).as_str(),
            "receive-ready"
        );
    }

    #[test]
    fn pre_dispatch_status_send_emits_sending() {
        // Phase 3b PR 1 Commit D extends the match with a `sending`
        // arm per design doc §5.2. The controller's StopAndCopyLive
        // phase gate-observes this annotation as the substateSrcSending
        // entry signal.
        assert_eq!(
            pre_dispatch_status(&ActionKind::MigrationSend).as_str(),
            "sending"
        );
    }

    #[test]
    fn pre_dispatch_status_other_actions_unchanged() {
        // Snapshot + restore + cancel actions retain the
        // pre-Phase-3b semantics; only the receive + send verbs get
        // specialized pre-dispatch annotations.
        assert_eq!(
            pre_dispatch_status(&ActionKind::SnapshotCapture).as_str(),
            "running"
        );
        assert_eq!(
            pre_dispatch_status(&ActionKind::SnapshotResume).as_str(),
            "running"
        );
        assert_eq!(
            pre_dispatch_status(&ActionKind::RestorePrepare).as_str(),
            "running"
        );
        assert_eq!(
            pre_dispatch_status(&ActionKind::MigrationCancel).as_str(),
            "running"
        );
    }

    // Phase 3b PR 1 Commit D — progress-estimate computation per
    // design doc §5.4. The pure function is testable in isolation;
    // the I/O wrapper (spawn_progress_emitter) is exercised in the
    // manual demo walkthrough (Commit F) where annotation emission
    // and the ~5s cadence can be observed on a real cluster.

    #[test]
    fn compute_progress_estimate_at_one_quarter() {
        // 4096 MB guest at PROGRESS_BASELINE_MBPS=108.0:
        //   expected_s = 4096 / 108 ≈ 37.93s
        //   elapsed_s = 10s → raw ≈ 26.36%
        //   capped at 26 (truncation, not rounding — `as i64`).
        let expected_s = 4096.0 / PROGRESS_BASELINE_MBPS;
        assert_eq!(compute_progress_estimate(10.0, expected_s), 26);
    }

    #[test]
    fn compute_progress_estimate_caps_at_95() {
        // 4096 MB guest at PROGRESS_BASELINE_MBPS=108.0, elapsed=60s:
        //   expected_s ≈ 37.93s
        //   raw ≈ 158% → capped to 95.
        // Operators see 95 (never 100) until vm.send-migration returns;
        // the transition to send-complete is the next discrete event.
        let expected_s = 4096.0 / PROGRESS_BASELINE_MBPS;
        assert_eq!(compute_progress_estimate(60.0, expected_s), 95);
    }

    #[test]
    fn compute_progress_estimate_handles_degenerate_inputs() {
        // expected_s = 0 (e.g., guest_ram_mib was 0): defensive 0.
        assert_eq!(compute_progress_estimate(10.0, 0.0), 0);
        // expected_s negative (impossible from the production path
        // but cheap to defend): defensive 0.
        assert_eq!(compute_progress_estimate(10.0, -1.0), 0);
        // elapsed_s = 0 (called at t=0): clamps to 0.
        assert_eq!(compute_progress_estimate(0.0, 38.0), 0);
        // NaN / infinity (defensive against clock-drift or panic-into-
        // f64-conversion): 0.
        assert_eq!(compute_progress_estimate(f64::NAN, 38.0), 0);
        assert_eq!(compute_progress_estimate(10.0, f64::INFINITY), 0);
    }

    #[test]
    fn compute_progress_estimate_baseline_matches_spike_q4_constant() {
        // PROGRESS_BASELINE_MBPS is the spike Q4 empirical baseline
        // (107.2 MB/s rounded to 108.0). If a future spike measurement
        // adjusts the constant, this assertion fails loud so the
        // companion test data (and operator docs) get updated.
        assert!(
            (PROGRESS_BASELINE_MBPS - 108.0).abs() < f64::EPSILON,
            "PROGRESS_BASELINE_MBPS changed to {}; update tests + docs",
            PROGRESS_BASELINE_MBPS
        );
    }

    // Note: spawn_progress_emitter's thread + drop-guard behaviour
    // is not unit-tested here. The emitter requires POD_NAMESPACE
    // / POD_NAME env vars and a working kube client to fire
    // annotation patches; both are present on cluster but absent
    // in the test harness. Cluster validation lands in the Commit F
    // manual-demo walkthrough doc, where the operator reads
    // `kubeswift.io/migration-progress-estimate` at 5s intervals
    // during a real send_migration. The drop-guard's cancel-on-drop
    // semantics ARE exercised by every test that calls
    // dispatch_migration_send (the guard is constructed before
    // send_migration and dropped on return).

    #[test]
    fn status_kind_custom_emits_verbatim() {
        // Migration source uses StatusKind::Custom("complete"); migration
        // destination uses StatusKind::Custom("running"). The string
        // is written verbatim to the migration-status annotation per
        // design §3.1.
        assert_eq!(StatusKind::Custom("complete").as_str(), "complete");
        assert_eq!(StatusKind::Custom("running").as_str(), "running");
        assert_eq!(StatusKind::Custom("listening").as_str(), "listening");
        assert_eq!(StatusKind::Custom("precopy").as_str(), "precopy");
    }

    #[tokio::test]
    async fn dispatch_restore_prepare_is_skeleton_no_op() {
        // Phase 2 commit 7 wires this in; for now it's still a no-op.
        let api_socket = PathBuf::from("/run/foo/ch.sock");
        let action = PendingAction {
            kind: ActionKind::RestorePrepare,
            id: "id-1".to_string(),
            args: serde_json::Value::Null,
        };
        let outcome = dispatch(&action, &api_socket).await.unwrap();
        assert!(outcome
            .detail
            .as_deref()
            .unwrap_or("")
            .contains("not yet wired"));
        assert_eq!(outcome.pause_window_ms, None);
    }

    #[tokio::test]
    async fn dispatch_unknown_kind_returns_err() {
        let api_socket = PathBuf::from("/run/foo/ch.sock");
        let action = PendingAction {
            kind: ActionKind::Unknown("bogus".to_string()),
            id: "id-2".to_string(),
            args: serde_json::Value::Null,
        };
        match dispatch(&action, &api_socket).await {
            Err(detail) => assert!(detail.contains("unknown action verb")),
            other => panic!("expected Err, got {:?}", other),
        }
    }

    // -------- Capture/resume tests using a multi-request mock UDS --------

    use std::io::{Read, Write};
    use std::os::unix::net::UnixListener;
    use std::sync::mpsc;
    use std::thread;

    /// Multi-request mock UDS that handles N sequential requests on
    /// the same socket path, each on a fresh accept (we send
    /// Connection: close from the client). For each request, the
    /// server records the raw bytes it received and replies with the
    /// next response from the supplied list.
    struct MultiMockServer {
        path: PathBuf,
        _tmp: tempfile::TempDir,
        handle: Option<thread::JoinHandle<Vec<Vec<u8>>>>,
    }

    impl MultiMockServer {
        fn spawn(responses: Vec<Vec<u8>>) -> Self {
            let tmp = tempfile::tempdir().unwrap();
            let path = tmp.path().join("ch.sock");
            let listener = UnixListener::bind(&path).unwrap();
            let (ready_tx, ready_rx) = mpsc::channel::<()>();
            let handle = thread::spawn(move || {
                ready_tx.send(()).unwrap();
                let mut got = Vec::new();
                for response in responses {
                    let (mut conn, _) = listener.accept().expect("accept");
                    let mut buf = vec![0u8; 4096];
                    let mut req = Vec::new();
                    loop {
                        let n = conn.read(&mut buf).unwrap();
                        if n == 0 {
                            break;
                        }
                        req.extend_from_slice(&buf[..n]);
                        if let Some(_pos) = find_subseq(&req, b"\r\n\r\n") {
                            if let Some(cl) = parse_content_length(&req) {
                                let header_end = find_subseq(&req, b"\r\n\r\n").unwrap() + 4;
                                if req.len() - header_end >= cl {
                                    break;
                                }
                            } else {
                                break;
                            }
                        }
                    }
                    conn.write_all(&response).unwrap();
                    drop(conn);
                    got.push(req);
                }
                got
            });
            ready_rx.recv().unwrap();
            Self {
                path,
                _tmp: tmp,
                handle: Some(handle),
            }
        }

        fn collect(mut self) -> Vec<Vec<u8>> {
            self.handle.take().unwrap().join().unwrap()
        }
    }

    fn find_subseq(haystack: &[u8], needle: &[u8]) -> Option<usize> {
        haystack.windows(needle.len()).position(|w| w == needle)
    }

    fn parse_content_length(req: &[u8]) -> Option<usize> {
        let s = std::str::from_utf8(req).ok()?;
        for line in s.lines() {
            let lower = line.to_ascii_lowercase();
            if let Some(rest) = lower.strip_prefix("content-length:") {
                return rest.trim().parse().ok();
            }
        }
        None
    }

    fn no_content() -> Vec<u8> {
        b"HTTP/1.1 204 No Content\r\n\r\n".to_vec()
    }

    fn capture_action(args: serde_json::Value) -> PendingAction {
        PendingAction {
            kind: ActionKind::SnapshotCapture,
            id: "snap1-r42".to_string(),
            args,
        }
    }

    /// Build a destination_url whose path is inside a tempdir so the
    /// dispatch_capture mkdir step (added when CH started rejecting
    /// non-existent destination dirs at runtime) succeeds in tests
    /// without root permissions.
    fn tmp_dest_url(tmp: &tempfile::TempDir, sub: &str) -> String {
        format!("file://{}/{}/", tmp.path().display(), sub)
    }

    #[tokio::test]
    async fn capture_drives_pause_then_snapshot_then_resume() {
        // Three responses for three sequential calls.
        let server = MultiMockServer::spawn(vec![no_content(), no_content(), no_content()]);
        let dest_tmp = tempfile::tempdir().unwrap();
        let dest_url = tmp_dest_url(&dest_tmp, "default-snap1");
        let action = capture_action(serde_json::json!({
            "destination_url": dest_url,
        }));
        let outcome = dispatch(&action, &server.path).await.unwrap();
        let reqs = server.collect();
        assert_eq!(reqs.len(), 3);
        let r0 = String::from_utf8_lossy(&reqs[0]);
        let r1 = String::from_utf8_lossy(&reqs[1]);
        let r2 = String::from_utf8_lossy(&reqs[2]);
        assert!(r0.starts_with("PUT /api/v1/vm.pause "), "got {}", r0);
        assert!(r1.starts_with("PUT /api/v1/vm.snapshot "), "got {}", r1);
        // Snapshot body must carry the destination_url.
        let (_, body) = r1.split_once("\r\n\r\n").unwrap();
        let parsed: serde_json::Value = serde_json::from_str(body).unwrap();
        assert_eq!(parsed["destination_url"], dest_url);
        assert!(r2.starts_with("PUT /api/v1/vm.resume "), "got {}", r2);
        // pause_window_ms is set on success.
        assert!(outcome.pause_window_ms.is_some());
        let detail = outcome.detail.unwrap();
        assert!(detail.contains(&format!("captured to {}", dest_url)));
        assert!(detail.contains("resumed"));
        // mkdir created the destination directory.
        assert!(std::path::Path::new(dest_tmp.path())
            .join("default-snap1")
            .is_dir());
    }

    #[tokio::test]
    async fn capture_with_resume_after_snapshot_false_skips_resume() {
        // Only two responses — pause and snapshot. If dispatch tried a
        // third call (resume) the test would hang on accept; we cap
        // the test runtime via the action-handler's own timeout.
        let server = MultiMockServer::spawn(vec![no_content(), no_content()]);
        let dest_tmp = tempfile::tempdir().unwrap();
        let action = capture_action(serde_json::json!({
            "destination_url": tmp_dest_url(&dest_tmp, "snap1"),
            "resume_after_snapshot": false,
        }));
        let outcome = dispatch(&action, &server.path).await.unwrap();
        let reqs = server.collect();
        assert_eq!(reqs.len(), 2);
        assert!(String::from_utf8_lossy(&reqs[0]).starts_with("PUT /api/v1/vm.pause "));
        assert!(String::from_utf8_lossy(&reqs[1]).starts_with("PUT /api/v1/vm.snapshot "));
        assert!(outcome.pause_window_ms.is_some());
        assert!(outcome
            .detail
            .as_deref()
            .unwrap_or("")
            .contains("left paused"));
    }

    #[tokio::test]
    async fn capture_attempts_resume_on_snapshot_failure() {
        // Pause ok, snapshot fails (4xx), then capture must still try
        // to resume so the VM doesn't sit Paused forever.
        let server = MultiMockServer::spawn(vec![
            no_content(),
            b"HTTP/1.1 400 Bad Request\r\nContent-Length: 21\r\n\r\nVM not in Paused state"
                .to_vec(),
            no_content(),
        ]);
        let dest_tmp = tempfile::tempdir().unwrap();
        let action = capture_action(serde_json::json!({
            "destination_url": tmp_dest_url(&dest_tmp, "snap1"),
        }));
        let err = dispatch(&action, &server.path).await.unwrap_err();
        assert!(err.contains("snapshot:"), "got {}", err);
        let reqs = server.collect();
        assert_eq!(reqs.len(), 3, "must have attempted recovery resume");
        assert!(String::from_utf8_lossy(&reqs[2]).starts_with("PUT /api/v1/vm.resume "));
    }

    #[tokio::test]
    async fn capture_fails_on_pause_error_without_resume_attempt() {
        // Pause fails; we have no Paused VM to recover, so we must
        // NOT call resume — calling resume on a Running VM is a no-op
        // at CH but burns an API call we shouldn't make.
        let server = MultiMockServer::spawn(vec![
            b"HTTP/1.1 500 Internal\r\nContent-Length: 0\r\n\r\n".to_vec(),
        ]);
        let dest_tmp = tempfile::tempdir().unwrap();
        let action = capture_action(serde_json::json!({
            "destination_url": tmp_dest_url(&dest_tmp, "snap1"),
        }));
        let err = dispatch(&action, &server.path).await.unwrap_err();
        assert!(err.contains("pause:"), "got {}", err);
        let reqs = server.collect();
        assert_eq!(reqs.len(), 1);
        assert!(String::from_utf8_lossy(&reqs[0]).starts_with("PUT /api/v1/vm.pause "));
    }

    #[tokio::test]
    async fn capture_creates_destination_dir_idempotently() {
        // Re-running the action handler with the same destination must
        // be a no-op on the directory (mkdir -p semantics): the dir
        // already exists, no error, snapshot proceeds.
        let server = MultiMockServer::spawn(vec![no_content(), no_content(), no_content()]);
        let dest_tmp = tempfile::tempdir().unwrap();
        let pre_existing = dest_tmp.path().join("preexisting");
        std::fs::create_dir_all(&pre_existing).unwrap();
        let dest_url = format!("file://{}/", pre_existing.display());
        let action = capture_action(serde_json::json!({"destination_url": dest_url}));
        dispatch(&action, &server.path).await.unwrap();
    }

    #[tokio::test]
    async fn capture_wipes_stale_files_in_destination() {
        // CH refuses to vm.snapshot when config.json / state.json /
        // memory-ranges already exist. The handler wipes the dir
        // before snapshotting so a stale prior attempt (cleanup
        // finalizer didn't fire, controller-driven retry, etc.)
        // doesn't block the new capture.
        let server = MultiMockServer::spawn(vec![no_content(), no_content(), no_content()]);
        let dest_tmp = tempfile::tempdir().unwrap();
        let snap_dir = dest_tmp.path().join("stale-snap");
        std::fs::create_dir_all(&snap_dir).unwrap();
        // Pre-populate with the three known CH outputs.
        std::fs::write(snap_dir.join("config.json"), b"{stale}").unwrap();
        std::fs::write(snap_dir.join("state.json"), b"{stale}").unwrap();
        std::fs::write(snap_dir.join("memory-ranges"), vec![0u8; 1024]).unwrap();
        let dest_url = format!("file://{}/", snap_dir.display());
        let action = capture_action(serde_json::json!({"destination_url": dest_url}));
        dispatch(&action, &server.path).await.unwrap();
        // Snapshot completed; mkdir-after-wipe gave CH a clean dir.
        // (CH itself is mocked here so it doesn't write files; the
        // assertion is that the wipe + mkdir didn't error.)
        assert!(snap_dir.is_dir());
        // The stale files we pre-populated are gone (wiped).
        assert!(!snap_dir.join("config.json").exists());
        assert!(!snap_dir.join("state.json").exists());
        assert!(!snap_dir.join("memory-ranges").exists());
    }

    #[tokio::test]
    async fn capture_args_missing_destination_url_is_error() {
        // No mock server needed — we fail before any API call.
        let action = capture_action(serde_json::json!({}));
        let err = dispatch(&action, Path::new("/does/not/matter"))
            .await
            .unwrap_err();
        assert!(err.contains("parse capture args"), "got {}", err);
    }

    #[tokio::test]
    async fn resume_action_calls_vm_resume() {
        let server = MultiMockServer::spawn(vec![no_content()]);
        let action = PendingAction {
            kind: ActionKind::SnapshotResume,
            id: "resume-1".to_string(),
            args: serde_json::Value::Null,
        };
        let outcome = dispatch(&action, &server.path).await.unwrap();
        let reqs = server.collect();
        assert_eq!(reqs.len(), 1);
        assert!(String::from_utf8_lossy(&reqs[0]).starts_with("PUT /api/v1/vm.resume "));
        assert_eq!(outcome.pause_window_ms, None);
    }

    #[tokio::test]
    async fn resume_action_propagates_4xx_from_ch() {
        let server = MultiMockServer::spawn(vec![
            b"HTTP/1.1 400 Bad Request\r\nContent-Length: 4\r\n\r\nnope".to_vec(),
        ]);
        let action = PendingAction {
            kind: ActionKind::SnapshotResume,
            id: "resume-1".to_string(),
            args: serde_json::Value::Null,
        };
        let err = dispatch(&action, &server.path).await.unwrap_err();
        assert!(err.contains("resume:"), "got {}", err);
    }

    // -------- Migration namespace decide() tests (PR-B) --------

    #[test]
    fn migration_no_action_annotation_is_idle() {
        let a = ann(&[]);
        assert_eq!(
            decide(&a, &MIGRATION_KEYS, None, None),
            ActionDecision::Idle
        );
    }

    #[test]
    fn migration_action_without_id_is_malformed() {
        let a = ann(&[(MIGRATION_ACTION_KEY, "send")]);
        match decide(&a, &MIGRATION_KEYS, None, None) {
            ActionDecision::RejectMalformed(_) => {}
            other => panic!("expected RejectMalformed, got {:?}", other),
        }
    }

    #[test]
    fn migration_send_without_ack_is_rejected() {
        // The Phase 2 plaintext-transport ack gate (S2 mitigation): a
        // migration action without `migration-phase2-unsafe-plaintext: ack`
        // is rejected at decide() time. The action-id IS present so the
        // controller can correlate the rejection.
        let a = ann(&[
            (MIGRATION_ACTION_KEY, "send"),
            (MIGRATION_ACTION_ID_KEY, "mig-001"),
        ]);
        match decide(&a, &MIGRATION_KEYS, None, None) {
            ActionDecision::RejectAckMissing {
                incoming_id,
                ack_key,
            } => {
                assert_eq!(incoming_id, "mig-001");
                assert_eq!(ack_key, MIGRATION_PHASE2_ACK_KEY);
            }
            other => panic!("expected RejectAckMissing, got {:?}", other),
        }
    }

    #[test]
    fn migration_send_with_invalid_ack_value_is_rejected() {
        // Anything other than the literal "ack" string is treated as
        // not-acked — including "true", "yes", empty string, etc.
        for invalid in ["", "true", "ACK", "yes", "1"] {
            let a = ann(&[
                (MIGRATION_ACTION_KEY, "send"),
                (MIGRATION_ACTION_ID_KEY, "mig-001"),
                (MIGRATION_PHASE2_ACK_KEY, invalid),
            ]);
            match decide(&a, &MIGRATION_KEYS, None, None) {
                ActionDecision::RejectAckMissing { .. } => {}
                other => panic!(
                    "expected RejectAckMissing for ack={:?}, got {:?}",
                    invalid, other
                ),
            }
        }
    }

    #[test]
    fn migration_send_with_ack_is_accepted() {
        let a = ann(&[
            (MIGRATION_ACTION_KEY, "send"),
            (MIGRATION_ACTION_ID_KEY, "mig-001"),
            (MIGRATION_PHASE2_ACK_KEY, "ack"),
        ]);
        match decide(&a, &MIGRATION_KEYS, None, None) {
            ActionDecision::Accept(p) => {
                assert_eq!(p.kind, ActionKind::MigrationSend);
                assert_eq!(p.id, "mig-001");
            }
            other => panic!("expected Accept, got {:?}", other),
        }
    }

    #[test]
    fn migration_cancel_with_ack_is_accepted() {
        let a = ann(&[
            (MIGRATION_ACTION_KEY, "cancel"),
            (MIGRATION_ACTION_ID_KEY, "mig-cancel-001"),
            (MIGRATION_PHASE2_ACK_KEY, "ack"),
        ]);
        match decide(&a, &MIGRATION_KEYS, None, None) {
            ActionDecision::Accept(p) => {
                assert_eq!(p.kind, ActionKind::MigrationCancel);
            }
            other => panic!("expected Accept, got {:?}", other),
        }
    }

    #[test]
    fn migration_cancel_bypasses_in_flight_gate() {
        // Cancel verbs bypass the RejectInFlight gate so they can
        // interrupt a running migration. Q1d-F2 documents that the
        // dst-kill is the cancel primitive — cancel must reach
        // dispatch even when receive is in flight.
        let a = ann(&[
            (MIGRATION_ACTION_KEY, "cancel"),
            (MIGRATION_ACTION_ID_KEY, "mig-cancel-001"),
            (MIGRATION_PHASE2_ACK_KEY, "ack"),
        ]);
        match decide(&a, &MIGRATION_KEYS, None, Some("mig-receive-999")) {
            ActionDecision::Accept(p) => {
                assert_eq!(p.kind, ActionKind::MigrationCancel);
            }
            other => panic!("cancel must bypass in-flight gate, got {:?}", other),
        }
    }

    #[test]
    fn migration_send_with_in_flight_other_id_is_rejected() {
        // Non-cancel verbs follow the normal RejectInFlight rule
        // even with ack present.
        let a = ann(&[
            (MIGRATION_ACTION_KEY, "send"),
            (MIGRATION_ACTION_ID_KEY, "mig-002"),
            (MIGRATION_PHASE2_ACK_KEY, "ack"),
        ]);
        match decide(&a, &MIGRATION_KEYS, None, Some("mig-001")) {
            ActionDecision::RejectInFlight {
                incoming_id,
                current_id,
            } => {
                assert_eq!(incoming_id, "mig-002");
                assert_eq!(current_id, "mig-001");
            }
            other => panic!("expected RejectInFlight, got {:?}", other),
        }
    }

    #[test]
    fn migration_idempotent_when_id_matches_last_completed() {
        let a = ann(&[
            (MIGRATION_ACTION_KEY, "send"),
            (MIGRATION_ACTION_ID_KEY, "mig-001"),
            (MIGRATION_PHASE2_ACK_KEY, "ack"),
        ]);
        match decide(&a, &MIGRATION_KEYS, Some("mig-001"), None) {
            ActionDecision::Idempotent { id } => assert_eq!(id, "mig-001"),
            other => panic!("expected Idempotent, got {:?}", other),
        }
    }

    #[test]
    fn parse_migration_verb_maps_known_and_unknown() {
        assert_eq!(parse_migration_verb("send"), ActionKind::MigrationSend);
        assert_eq!(
            parse_migration_verb("receive"),
            ActionKind::MigrationReceive
        );
        assert_eq!(parse_migration_verb("cancel"), ActionKind::MigrationCancel);
        assert_eq!(
            parse_migration_verb("xyz"),
            ActionKind::Unknown("xyz".to_string())
        );
    }

    #[test]
    fn migration_keyset_carries_ack_gate() {
        // Snapshot's KeySet has no ack gate; migration's does.
        // This is the structural signal that ack-gate enforcement is
        // namespace-specific and Phase 3 will turn it off when mTLS
        // lands.
        assert!(SNAPSHOT_KEYS.ack_key.is_none());
        assert_eq!(MIGRATION_KEYS.ack_key, Some(MIGRATION_PHASE2_ACK_KEY));
    }

    // --- Phase 3c PR 4: secured-mode (mTLS) ----------------------------

    #[test]
    fn secured_from_recognises_truthy_values() {
        assert!(secured_from(Some("1")));
        assert!(secured_from(Some("true")));
        assert!(!secured_from(Some("0")));
        assert!(!secured_from(Some("false")));
        assert!(!secured_from(Some("")));
        assert!(!secured_from(Some("yes")));
        assert!(!secured_from(None));
    }

    #[test]
    fn migration_keys_for_drops_ack_gate_when_secured() {
        // Plaintext: ack gate present. Secured: ack gate bypassed.
        assert_eq!(
            migration_keys_for(false).ack_key,
            Some(MIGRATION_PHASE2_ACK_KEY)
        );
        assert!(migration_keys_for(true).ack_key.is_none());
        // All other fields are untouched by the flip.
        assert_eq!(
            migration_keys_for(true).action_key,
            MIGRATION_KEYS.action_key
        );
        assert_eq!(migration_keys_for(true).namespace, MIGRATION_KEYS.namespace);
    }

    #[test]
    fn validate_loopback_url_accepts_loopback() {
        for url in [
            "tcp:127.0.0.1:6790",
            "tcp:127.5.5.5:6790",
            "tcp:localhost:6790",
            "tcp:[::1]:6790",
            "unix:/var/run/ch/migration.sock",
        ] {
            assert!(
                validate_loopback_url(url).is_ok(),
                "expected {} to be accepted as loopback",
                url
            );
        }
    }

    #[test]
    fn validate_loopback_url_rejects_remote_and_bad_scheme() {
        for url in [
            "tcp:1.2.3.4:6789",
            "tcp:10.0.0.9:6789",
            "tcp:example.com:6789",
            "tcp:[2001:db8::1]:6789",
            "http://127.0.0.1:6790",
        ] {
            let res = validate_loopback_url(url);
            assert!(res.is_err(), "expected {} to be rejected", url);
            // The rejection detail must NOT contain a connection_refused
            // token (the controller must not retry an S1 rejection).
            assert!(
                !res.unwrap_err().contains("connection_refused"),
                "S1 rejection detail must not look retryable: {}",
                url
            );
        }
    }

    #[test]
    fn migration_keyset_namespace_field() {
        assert_eq!(SNAPSHOT_KEYS.namespace, "snapshot");
        assert_eq!(MIGRATION_KEYS.namespace, "migration");
    }

    // -------- Sanitize_ch_error (S3 detail-string sanitization) --------

    #[test]
    fn sanitize_ch_error_categorizes_status_codes() {
        // The sanitizer must NOT pass through raw error bodies — only
        // category tokens. This protects against future failure modes
        // that could leak partial guest state through error strings
        // (S3 finding from the spike).
        assert_eq!(
            sanitize_ch_error("Status(ApiResponse { status: 400, body: ... })"),
            "bad_request"
        );
        assert_eq!(
            sanitize_ch_error("Status(ApiResponse { status: 500, body: ... })"),
            "internal_server_error"
        );
        assert_eq!(
            sanitize_ch_error("Connect { path: ..., source: ... }"),
            "connection_refused"
        );
        assert_eq!(
            sanitize_ch_error("Configure(...)"),
            "socket_configure_failed"
        );
        assert_eq!(sanitize_ch_error("Read(...)"), "transport_error");
        assert_eq!(sanitize_ch_error("Write(...)"), "transport_error");
        assert_eq!(sanitize_ch_error("Malformed(...)"), "malformed_response");
        assert_eq!(sanitize_ch_error("totally novel error"), "ch_error");
    }

    #[test]
    fn sanitize_ch_error_does_not_pass_through_payload() {
        // Defensive: even if a future ApiError variant carries an
        // unexpected structure, the sanitizer must return one of the
        // hardcoded categories. The output is a `&'static str` so by
        // construction no payload bytes can leak.
        let raw = "Status(ApiResponse { status: 503, body: [secret guest memory bytes] })";
        let category = sanitize_ch_error(raw);
        assert_eq!(category, "ch_status_error");
        assert!(!category.contains("secret"));
        assert!(!category.contains("body"));
    }

    // -------- Migration dispatch tests --------

    fn migration_send_action(args: serde_json::Value) -> PendingAction {
        PendingAction {
            kind: ActionKind::MigrationSend,
            id: "mig-001".to_string(),
            args,
        }
    }

    fn migration_receive_action(args: serde_json::Value) -> PendingAction {
        PendingAction {
            kind: ActionKind::MigrationReceive,
            id: "mig-recv-001".to_string(),
            args,
        }
    }

    #[tokio::test]
    async fn migration_send_returns_complete_when_ch_exits() {
        // Phase 2 spike Q1c: source CH auto-exits cleanly on successful
        // send-migration. The dispatch handler probes vm_info post-send;
        // ConnectionRefused is the expected outcome (CH gone).
        //
        // We model this with a mock that responds to the send-migration
        // call with 204 No Content, then the second accept (vm_info)
        // never connects because the mock has run out of responses.
        // The vm_info call returns a Connect error → sanitizer maps to
        // `connection_refused` → W1 gate accepts as "CH gone clean".
        let server = MultiMockServer::spawn(vec![no_content()]);
        let action = migration_send_action(serde_json::json!({ "target_url": "tcp:1.2.3.4:6789" }));
        let outcome = dispatch(&action, &server.path).await.unwrap();
        let detail = outcome.detail.unwrap();
        assert!(
            detail.contains("sent to tcp:1.2.3.4:6789"),
            "got {}",
            detail
        );
        assert!(outcome.pause_window_ms.is_some());
        // Source-side migration success verb per design §3.1.
        assert_eq!(outcome.success_status, Some("complete"));
    }

    #[tokio::test]
    async fn migration_send_w1_violates_when_ch_still_running() {
        // W1 gate (load-bearing item C): if send_migration returns 0
        // but vm_info still reports state=Running, that's abnormal —
        // CH should have exited. Surface as failure with the
        // w1_violation category.
        let body = br#"{"config":{},"state":"Running"}"#;
        let info_response = format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n", body.len());
        let mut info_full = info_response.into_bytes();
        info_full.extend_from_slice(body);
        let server = MultiMockServer::spawn(vec![no_content(), info_full]);
        let action = migration_send_action(serde_json::json!({ "target_url": "tcp:1.2.3.4:6789" }));
        let err = dispatch(&action, &server.path).await.unwrap_err();
        assert!(err.contains("w1_violation"), "got {}", err);
        assert!(err.contains("state=Running"), "got {}", err);
    }

    #[tokio::test]
    async fn migration_send_propagates_send_failure_with_sanitized_detail() {
        // F2/F3 in spike: send-migration fails when destination is
        // gone or network drops. The sanitizer converts the raw error
        // into a category token; raw bytes never reach the
        // status-detail annotation.
        let server = MultiMockServer::spawn(vec![
            b"HTTP/1.1 500 Internal\r\nContent-Length: 25\r\n\r\nguest secret memory bytes"
                .to_vec(),
        ]);
        let action = migration_send_action(serde_json::json!({ "target_url": "tcp:1.2.3.4:6789" }));
        let err = dispatch(&action, &server.path).await.unwrap_err();
        assert!(err.contains("internal_server_error"), "got {}", err);
        // Critical: raw body bytes must not be in the error detail
        // (S3 sanitization invariant).
        assert!(
            !err.contains("guest secret memory bytes"),
            "raw body leaked into error: {}",
            err
        );
    }

    #[tokio::test]
    async fn migration_receive_post_dispatch_status_is_running_when_state_running() {
        // Phase 2 spike Q1c: destination CH auto-resumes after
        // receive-migration completes. The dispatch handler probes
        // vm_info post-receive and requires state=Running.
        //
        // Note: this asserts the POST-dispatch terminal status verb
        // (`outcome.success_status == Some("running")`, the
        // destination-side success verb per design §3.1). It is NOT
        // the Phase 3b pre-dispatch `receive-ready` annotation, which
        // is covered by pre_dispatch_status_receive_emits_receive_ready.
        // The test name was clarified in Phase 3b PR 1 Commit D to
        // make the distinction unambiguous.
        let body = br#"{"config":{},"state":"Running"}"#;
        let info_response = format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n", body.len());
        let mut info_full = info_response.into_bytes();
        info_full.extend_from_slice(body);
        let server = MultiMockServer::spawn(vec![no_content(), info_full]);
        let action =
            migration_receive_action(serde_json::json!({ "listen_url": "tcp:0.0.0.0:6789" }));
        let outcome = dispatch(&action, &server.path).await.unwrap();
        let detail = outcome.detail.unwrap();
        assert!(
            detail.contains("received on tcp:0.0.0.0:6789"),
            "got {}",
            detail
        );
        // Destination-side migration success verb per design §3.1.
        assert_eq!(outcome.success_status, Some("running"));
    }

    #[tokio::test]
    async fn migration_receive_w1_violates_when_state_not_running() {
        // If receive_migration returns 0 but vm_info reports Paused
        // (or any non-Running state), the W1 gate fires.
        let body = br#"{"config":{},"state":"Paused"}"#;
        let info_response = format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n", body.len());
        let mut info_full = info_response.into_bytes();
        info_full.extend_from_slice(body);
        let server = MultiMockServer::spawn(vec![no_content(), info_full]);
        let action =
            migration_receive_action(serde_json::json!({ "listen_url": "tcp:0.0.0.0:6789" }));
        let err = dispatch(&action, &server.path).await.unwrap_err();
        assert!(err.contains("w1_violation"), "got {}", err);
        assert!(err.contains("state=Paused"), "got {}", err);
    }

    // ─── Phase 3a D1 (`docs/design/live-migration-phase-3a.md` §7.2) ─────────
    //
    // Cancel handler tests: the placeholder shipped in Phase 2 PR-B is
    // now the real SIGKILL-via-PID-file implementation. Tests cover the
    // failure paths that don't require a live CH child (no-pid-file,
    // unparseable pid, exe-symlink-mismatch). The actual SIGKILL path
    // is exercised by the D1 cluster integration test (manual demo
    // re-run in PR description) — unit-testing libc::kill against a
    // real process requires a fork+exec harness disproportionate to
    // the bug-surface of three lines of code.

    #[tokio::test]
    async fn migration_cancel_no_pid_file_is_error() {
        // No `<runtime_dir>/ch.pid` (CH never started, or the
        // launcher failed to write the pid file). Cancel returns
        // an error; controller falls back to kubectl delete pod.
        let tmp = tempfile::tempdir().unwrap();
        let api_socket = tmp.path().join("ch.sock");
        let action = PendingAction {
            kind: ActionKind::MigrationCancel,
            id: "mig-cancel-no-pid".to_string(),
            args: serde_json::Value::Null,
        };
        let err = dispatch(&action, &api_socket).await.unwrap_err();
        assert!(
            err.starts_with("cancel kill failed:"),
            "expected cancel-kill-failed prefix, got {}",
            err
        );
        assert!(err.contains("ch.pid"), "got {}", err);
    }

    #[tokio::test]
    async fn migration_cancel_unparseable_pid_is_error() {
        // Garbage in ch.pid (corrupted file, race with launcher
        // truncating it). Cancel returns an error.
        let tmp = tempfile::tempdir().unwrap();
        std::fs::write(tmp.path().join("ch.pid"), "not-a-pid").unwrap();
        let api_socket = tmp.path().join("ch.sock");
        let action = PendingAction {
            kind: ActionKind::MigrationCancel,
            id: "mig-cancel-bad-pid".to_string(),
            args: serde_json::Value::Null,
        };
        let err = dispatch(&action, &api_socket).await.unwrap_err();
        assert!(err.contains("parse pid"), "got {}", err);
    }

    #[tokio::test]
    async fn migration_cancel_pid_reuse_check_rejects_unrelated_process() {
        // ch.pid points at a real PID but it's not cloud-hypervisor
        // (PID reuse: the receiver CH already exited and the kernel
        // reassigned the PID). The /proc/<pid>/exe check catches it
        // before the SIGKILL fires. Use the test process's own PID,
        // whose /proc/<pid>/exe basename is the test binary, NOT
        // cloud-hypervisor.
        let tmp = tempfile::tempdir().unwrap();
        let our_pid = std::process::id();
        std::fs::write(tmp.path().join("ch.pid"), our_pid.to_string()).unwrap();
        let api_socket = tmp.path().join("ch.sock");
        let action = PendingAction {
            kind: ActionKind::MigrationCancel,
            id: "mig-cancel-reuse".to_string(),
            args: serde_json::Value::Null,
        };
        let err = dispatch(&action, &api_socket).await.unwrap_err();
        assert!(err.starts_with("cancel kill failed:"), "got {}", err);
        assert!(
            err.contains("expected cloud-hypervisor"),
            "expected exe-mismatch reason, got {}",
            err
        );
        // Critically: the test process is still alive (we did NOT
        // SIGKILL ourselves). If the verify step were skipped, this
        // test would terminate with SIGKILL instead of asserting.
    }

    #[test]
    fn read_ch_pid_strips_trailing_newline() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("ch.pid");
        // launch.rs writes pid via write(&p, pid.to_string()) — no
        // trailing newline. Operators or tooling editing the file
        // may add one; trim() handles both.
        std::fs::write(&p, "12345\n").unwrap();
        assert_eq!(read_ch_pid(&p).unwrap(), 12345);
        std::fs::write(&p, "67890").unwrap();
        assert_eq!(read_ch_pid(&p).unwrap(), 67890);
    }

    #[test]
    fn verify_pid_is_cloud_hypervisor_rejects_self() {
        // /proc/self/exe basename is the test binary, NOT
        // cloud-hypervisor. Defense-in-depth check rejects.
        let our_pid = std::process::id() as i32;
        let err = verify_pid_is_cloud_hypervisor(our_pid).unwrap_err();
        assert!(err.contains("expected cloud-hypervisor"), "got {}", err);
    }

    #[test]
    fn verify_pid_is_cloud_hypervisor_rejects_dead_pid() {
        // PID 1 in a non-init namespace would be /sbin/init or
        // similar; we use a pid that almost certainly doesn't exist
        // (max u16 + 1). readlink fails; the verify step returns
        // err — caller treats as cancel failure.
        let err = verify_pid_is_cloud_hypervisor(99_999_999).unwrap_err();
        assert!(err.contains("readlink"), "got {}", err);
    }

    // ─── Phase 3a D2 watchdog (`docs/design/live-migration-phase-3a.md` §7.2) ─

    #[test]
    fn watchdog_skips_when_no_action_id() {
        // No active migration; CH may have died for unrelated reasons
        // (eg pod-shutdown after a graceful stop). Nothing to fail.
        let a = ann(&[]);
        match decide_watchdog(&a, "code=1") {
            WatchdogDecision::Skip(reason) => {
                assert!(
                    reason.contains("no active migration-action-id"),
                    "got {}",
                    reason
                );
            }
            other => panic!("expected Skip, got {:?}", other),
        }
    }

    #[test]
    fn watchdog_skips_when_action_id_is_empty_string() {
        // Annotation present but empty — same as absent for our purposes.
        let a = ann(&[(MIGRATION_ACTION_ID_KEY, "")]);
        match decide_watchdog(&a, "code=1") {
            WatchdogDecision::Skip(_) => {}
            other => panic!("expected Skip, got {:?}", other),
        }
    }

    #[test]
    fn watchdog_skips_when_terminal_failed_already_present_for_same_id() {
        // D1 race winner: D1's cancel handler returned Err, the action
        // loop wrote `migration-status: failed` paired with CANCEL_ID
        // before CH's child.wait() unblocked in main.rs. D2's watchdog
        // observes the existing terminal failure and skips, preserving
        // write-once semantics so the controller sees D1's "cancelled"
        // detail rather than D2's "destination listener exited
        // abnormally".
        let a = ann(&[
            (MIGRATION_ACTION_ID_KEY, "mig-cancel-001"),
            (MIGRATION_STATUS_KEY, "failed"),
            (MIGRATION_STATUS_ID_KEY, "mig-cancel-001"),
            (MIGRATION_STATUS_DETAIL_KEY, "cancelled"),
        ]);
        match decide_watchdog(&a, "code=signal:9") {
            WatchdogDecision::Skip(reason) => {
                assert!(reason.contains("terminal failed"), "got {}", reason);
            }
            other => panic!("expected Skip, got {:?}", other),
        }
    }

    #[test]
    fn watchdog_writes_failed_when_no_status_yet() {
        // CH died abnormally before any status was written for the
        // current action (eg listener crash before recv-accept).
        // Watchdog writes failed paired with the action-id.
        let a = ann(&[(MIGRATION_ACTION_ID_KEY, "mig-recv-042")]);
        match decide_watchdog(&a, "code=1") {
            WatchdogDecision::WriteFailed { action_id, detail } => {
                assert_eq!(action_id, "mig-recv-042");
                assert!(detail.contains("destination listener exited abnormally"));
                assert!(detail.contains("code=1"));
            }
            other => panic!("expected WriteFailed, got {:?}", other),
        }
    }

    #[test]
    fn watchdog_writes_failed_when_intermediate_running_for_same_id() {
        // recv accepted by action loop ("running" intermediate written),
        // CH died mid-receive. No terminal status yet → watchdog writes
        // failed. Note: dispatch_migration_receive's success path also
        // writes `running` as success_status; D2 cannot disambiguate
        // intermediate from terminal-success "running", so D2 will
        // also fire post-success-then-CH-death — operationally correct
        // because the dst guest is now dead.
        let a = ann(&[
            (MIGRATION_ACTION_ID_KEY, "mig-recv-042"),
            (MIGRATION_STATUS_KEY, "running"),
            (MIGRATION_STATUS_ID_KEY, "mig-recv-042"),
        ]);
        match decide_watchdog(&a, "signal:9") {
            WatchdogDecision::WriteFailed { action_id, .. } => {
                assert_eq!(action_id, "mig-recv-042");
            }
            other => panic!("expected WriteFailed, got {:?}", other),
        }
    }

    #[test]
    fn watchdog_writes_failed_when_terminal_failed_was_for_different_id() {
        // Status carries a stale terminal from a prior migration
        // attempt (different action-id). The current action has no
        // matching terminal status; watchdog writes failed for the
        // current action-id.
        let a = ann(&[
            (MIGRATION_ACTION_ID_KEY, "mig-recv-002"),
            (MIGRATION_STATUS_KEY, "failed"),
            (MIGRATION_STATUS_ID_KEY, "mig-recv-001"),
            (MIGRATION_STATUS_DETAIL_KEY, "stale"),
        ]);
        match decide_watchdog(&a, "code=137") {
            WatchdogDecision::WriteFailed { action_id, .. } => {
                assert_eq!(action_id, "mig-recv-002");
            }
            other => panic!("expected WriteFailed, got {:?}", other),
        }
    }

    #[test]
    fn watchdog_writes_failed_when_terminal_complete_was_for_same_id() {
        // Source-side success status ("complete") for the same id.
        // For dst-side D2 this case is implausible (dst writes
        // "running" not "complete"), but defensively the watchdog
        // would fire because the skip rule only matches "failed",
        // not "complete". A successful-then-CH-death event surfaces
        // as a NEW failure; the controller's interpretation is that
        // the migration succeeded then the dst guest died.
        let a = ann(&[
            (MIGRATION_ACTION_ID_KEY, "mig-recv-007"),
            (MIGRATION_STATUS_KEY, "complete"),
            (MIGRATION_STATUS_ID_KEY, "mig-recv-007"),
        ]);
        match decide_watchdog(&a, "code=1") {
            WatchdogDecision::WriteFailed { action_id, .. } => {
                assert_eq!(action_id, "mig-recv-007");
            }
            other => panic!("expected WriteFailed, got {:?}", other),
        }
    }

    #[tokio::test]
    async fn migration_send_args_missing_target_url_is_error() {
        let action = migration_send_action(serde_json::json!({}));
        let err = dispatch(&action, Path::new("/does/not/matter"))
            .await
            .unwrap_err();
        assert!(err.contains("parse migration_send args"), "got {}", err);
    }

    #[tokio::test]
    async fn migration_receive_args_missing_listen_url_is_error() {
        let action = migration_receive_action(serde_json::json!({}));
        let err = dispatch(&action, Path::new("/does/not/matter"))
            .await
            .unwrap_err();
        assert!(err.contains("parse migration_receive args"), "got {}", err);
    }

    // ─── Phase 3a D3 (`docs/design/live-migration-phase-3a.md` §7.2) ─────────
    //
    // Args-parsing tests for the new `guest_ip` field. The actual
    // `propagate_guest_ip_annotation` kube write is exercised by the
    // cluster integration test in the manual demo re-run at PR close
    // — unit-testing it would need a kube client mock and POD_*
    // env-var harness disproportionate to a 50-line reuse of the
    // lease.rs pattern.

    // ─── W16 (Phase 3a PR 1 cluster walkthrough finding) ─────────────
    //
    // Cluster walkthrough surfaced that swiftletd-on-dst's receiver-mode
    // never flips the SwiftGuest's GuestRunning condition to True after
    // receive_migration completes successfully. The
    // `report_guest_running_post_receive` helper added in
    // `dispatch_migration_receive`'s success branch closes the gap.
    //
    // The kube-write path follows the same pattern as
    // propagate_guest_ip_annotation (D3): real kube client +
    // POD_NAMESPACE/POD_NAME env reads + best-effort patch. Cluster
    // integration validates the side effect; unit-testing the kube
    // write would need a kube-client mock harness disproportionate to
    // the 30-line helper.
    //
    // The pure-function piece (label extraction) IS unit-testable:

    // ─── W22 (PR #46 follow-up cluster re-walkthrough) ──────────────
    //
    // Cluster re-walkthrough Scenario 8 surfaced a race between
    // swiftletd-on-src's post-CH-exit VmStopped write and
    // swiftletd-on-dst's W16 GuestRunning=True write. Both target the
    // same SwiftGuest condition; last-write-wins. In S1 happy path
    // dst typically wins (lucky timing); in S8 cancel-during-Resuming
    // src wins because cancel-induced apiserver activity reorders
    // things, leaving the SwiftGuest stuck at GuestRunning=False.
    //
    // The fix: when dispatch_migration_send completes cleanly (W1
    // gate verifies CH is gone post-send), set
    // MIGRATION_SEND_COMPLETED=true. main.rs's post-launch path
    // checks the flag via migration_send_completed_clean() and
    // suppresses the VmStopped report; dst's W16 write becomes the
    // authoritative post-migration state.
    //
    // The full kube-write race is exercised by the cluster re-
    // walkthrough; the unit-testable surface is the flag's
    // behavior.

    // Serialization: the static AtomicBool is process-wide, so tests
    // that mutate it must serialize. Using #[serial_test] would add
    // a dependency; for two tests we sequence via a small helper.

    #[test]
    fn migration_send_completed_clean_default_false() {
        // The flag default is false. Don't mutate; just check the
        // initial-state contract (other W22 tests reset the flag
        // after their assertion).
        // Note: if a prior test leaked state, this assertion catches
        // the regression — the flag must not be set until a real
        // dispatch_migration_send success.
        // We can't unconditionally assert false here because test
        // ordering is undefined; instead we test the helper as a
        // pure read of the AtomicBool's current value, against an
        // explicit set/clear in the same test.
        // First: explicit clear, then read should be false.
        MIGRATION_SEND_COMPLETED.store(false, Ordering::SeqCst);
        assert!(!migration_send_completed_clean());
    }

    #[test]
    fn migration_send_completed_clean_after_set_true() {
        MIGRATION_SEND_COMPLETED.store(true, Ordering::SeqCst);
        assert!(migration_send_completed_clean());
        // Reset so other tests see the default.
        MIGRATION_SEND_COMPLETED.store(false, Ordering::SeqCst);
    }

    // ─── W23 (PR #46 follow-up cluster re-walkthrough, post-W22) ────
    //
    // Cluster re-walkthrough Scenarios 1+8 against the W22 image
    // (sha-8dd1a51) showed src pod's migration-status stuck at
    // "running" because main exited before the action loop's
    // terminal-status write landed. W23 adds graceful shutdown
    // signaling: action loop fires Notify after the terminal
    // write returns; main awaits with 10s bounded timeout before
    // exiting on the W22 success path.
    //
    // Unit-testable surface: the Notify primitive's signal-receive
    // and timeout-expiry contracts. The full kube-write-then-signal
    // shutdown flow is exercised by the cluster re-walkthrough.

    #[tokio::test]
    async fn w23_signal_received_within_timeout() {
        // Two threads (or in this case, two tokio tasks) sharing the
        // singleton Notify: one fires notify_one shortly; the other
        // awaits .notified() with a 10s bound. The await must
        // complete via signal, NOT timeout.
        let signal = migration_send_terminal_signal();
        let firer_signal = signal.clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(50)).await;
            firer_signal.notify_one();
        });
        let result = tokio::time::timeout(Duration::from_secs(10), signal.notified()).await;
        assert!(result.is_ok(), "signal must be received within 10s timeout");
    }

    #[tokio::test]
    async fn w23_signal_timeout_path_returns_err() {
        // No firer; ensure timeout fires after a short bound.
        // Use a *fresh* Notify (not the singleton) since the
        // singleton may have a stored permit from prior tests.
        let fresh = Arc::new(Notify::new());
        let result = tokio::time::timeout(Duration::from_millis(100), fresh.notified()).await;
        assert!(
            result.is_err(),
            "timeout-expiry path must return Err (Elapsed)"
        );
    }

    #[tokio::test]
    async fn w23_signal_idempotent_notify_one_before_notified() {
        // Notify semantics: notify_one before notified stores at
        // most one permit; the next notified() wakes immediately.
        // This is the load-bearing property for the W23 contract:
        // if the action loop fires before main awaits, main still
        // wakes promptly.
        let fresh = Arc::new(Notify::new());
        fresh.notify_one();
        // Second notify_one is benign (still at most one permit).
        fresh.notify_one();
        let result = tokio::time::timeout(Duration::from_millis(100), fresh.notified()).await;
        assert!(
            result.is_ok(),
            "notify_one before notified must wake the next notified() call"
        );
    }

    #[test]
    fn extract_guest_name_from_labels_returns_value_when_present() {
        use std::collections::BTreeMap;
        let mut labels = BTreeMap::new();
        labels.insert(
            "swift.kubeswift.io/guest".to_string(),
            "faas-s2".to_string(),
        );
        labels.insert("kubeswift.io/migration".to_string(), "s2-mig".to_string());
        assert_eq!(
            extract_guest_name_from_labels(Some(&labels)),
            Some("faas-s2".to_string())
        );
    }

    #[test]
    fn extract_guest_name_from_labels_returns_none_when_missing() {
        use std::collections::BTreeMap;
        let labels = BTreeMap::new();
        assert_eq!(extract_guest_name_from_labels(Some(&labels)), None);
    }

    #[test]
    fn extract_guest_name_from_labels_returns_none_when_labels_absent() {
        assert_eq!(extract_guest_name_from_labels(None), None);
    }

    #[test]
    fn extract_guest_name_from_labels_returns_none_when_value_empty() {
        use std::collections::BTreeMap;
        let mut labels = BTreeMap::new();
        labels.insert("swift.kubeswift.io/guest".to_string(), "".to_string());
        assert_eq!(extract_guest_name_from_labels(Some(&labels)), None);
    }

    #[test]
    fn migration_receive_args_parses_with_guest_ip() {
        // Phase 3a controller-mediated path: controller forwards the
        // src pod's existing kubeswift.io/guest-ip annotation into
        // migration-action-args.guest_ip.
        let raw = serde_json::json!({
            "listen_url": "tcp:0.0.0.0:6789",
            "guest_ip": "192.168.99.42",
        });
        let parsed: MigrationReceiveArgs = serde_json::from_value(raw).unwrap();
        assert_eq!(parsed.listen_url, "tcp:0.0.0.0:6789");
        assert_eq!(parsed.guest_ip.as_deref(), Some("192.168.99.42"));
    }

    #[test]
    fn migration_receive_args_parses_without_guest_ip() {
        // Phase 2 manual-path / pre-D3 callers don't set guest_ip;
        // the field is `#[serde(default)]` so absence parses as
        // None and the propagation step skips with a benign log.
        let raw = serde_json::json!({"listen_url": "tcp:0.0.0.0:6789"});
        let parsed: MigrationReceiveArgs = serde_json::from_value(raw).unwrap();
        assert_eq!(parsed.guest_ip, None);
    }

    #[test]
    fn migration_receive_args_with_explicit_null_guest_ip_is_none() {
        // JSON null also parses as None (consistent with default).
        let raw = serde_json::json!({
            "listen_url": "tcp:0.0.0.0:6789",
            "guest_ip": null,
        });
        let parsed: MigrationReceiveArgs = serde_json::from_value(raw).unwrap();
        assert_eq!(parsed.guest_ip, None);
    }

    #[test]
    fn d3_security_s1_tag_present_at_args_read_site() {
        // Phase 3b's grep-and-delete sweep keys on the literal
        // string `SECURITY-S1` in the swiftletd source. This test
        // pins that the marker is present at the D3 args-read site
        // so a refactor that drops the comment doesn't silently
        // remove the Phase 3b audit signal.
        let source = include_str!("action.rs");
        let count = source.matches("SECURITY-S1").count();
        assert!(
            count >= 3,
            "expected ≥3 SECURITY-S1 markers (D3 args struct + send + receive read sites), found {}",
            count
        );
        // Confirm D3-specific marker text is intact.
        assert!(
            source.contains("// SECURITY-S1: guest_ip is read"),
            "D3 SECURITY-S1 marker text missing or renamed; Phase 3b grep-and-delete sweep depends on it"
        );
    }
}

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
use std::time::Duration;

use kube::api::{Api, Patch, PatchParams};
use kube::Client;
use serde_json::json;

use crate::kube_client;

// Annotation keys — controller writes (read by swiftletd).
pub const ACTION_KEY: &str = "kubeswift.io/snapshot-action";
pub const ACTION_ID_KEY: &str = "kubeswift.io/snapshot-action-id";
pub const ACTION_ARGS_KEY: &str = "kubeswift.io/snapshot-action-args";

// Annotation keys — swiftletd writes (read by controller).
pub const STATUS_KEY: &str = "kubeswift.io/snapshot-status";
pub const STATUS_ID_KEY: &str = "kubeswift.io/snapshot-status-id";
pub const STATUS_DETAIL_KEY: &str = "kubeswift.io/snapshot-status-detail";
/// Pause window in milliseconds, observed at the action handler. The
/// controller surfaces this in SwiftSnapshot.status.observedPauseWindow.
/// Set only on a successful capture.
pub const STATUS_PAUSE_WINDOW_MS_KEY: &str = "kubeswift.io/snapshot-pause-window-ms";

/// Default per-call timeout for pause/resume/snapshot when the action
/// args don't carry a `timeout_seconds` hint. 600s covers a 200 GiB VM
/// at the Phase 0 ~2.8s/GiB curve with margin; the controller passes
/// a tighter, size-derived value in practice.
pub const DEFAULT_ACTION_TIMEOUT_SECS: u64 = 600;

/// Polling cadence for the action loop. 2s matches the lease poller
/// and is well below any plausible action latency (CH pause is ms;
/// snapshot+memory write is seconds).
pub const POLL_INTERVAL: Duration = Duration::from_secs(2);

/// Action verbs the controller can ask for.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ActionKind {
    /// pause -> snapshot(destination_url) -> resume. Ships in commit 6.
    SnapshotCapture,
    /// Stand-alone resume after a paused VM (used after restore-prepare
    /// completes). Ships in commit 6 alongside capture.
    SnapshotResume,
    /// Bring up a fresh CH process via spawn_ch_restore. Ships in commit 7.
    RestorePrepare,
    /// Anything else — surfaces as a malformed-rejection so the
    /// controller learns immediately rather than the launcher silently
    /// stalling on an unknown verb.
    Unknown(String),
}

impl ActionKind {
    /// Map an `kubeswift.io/snapshot-action` value to a kind.
    pub fn from_verb(verb: &str) -> Self {
        match verb {
            "capture" => ActionKind::SnapshotCapture,
            "resume" => ActionKind::SnapshotResume,
            "prepare" => ActionKind::RestorePrepare,
            other => ActionKind::Unknown(other.to_string()),
        }
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
}

impl StatusKind {
    pub fn as_str(self) -> &'static str {
        match self {
            StatusKind::Running => "running",
            StatusKind::Ready => "ready",
            StatusKind::Failed => "failed",
            StatusKind::Rejected => "rejected",
        }
    }
}

/// Local state the action loop carries across iterations.
#[derive(Default, Debug)]
pub struct ActionState {
    /// action-id of the last action we drove to a terminal status
    /// (Ready, Failed, or Rejected). Used for idempotency: if the
    /// controller's annotations still reference this id, we ignore
    /// rather than re-execute.
    pub last_completed_id: Option<String>,
    /// Action currently being processed. `None` when idle. The loop
    /// is single-threaded, so this slot has at most one occupant.
    pub in_flight: Option<PendingAction>,
}

/// Pure-function action-decision logic. Tested in isolation — no
/// kube-rs dependency, no I/O, no async. The action loop calls this
/// each tick with the freshly-fetched annotations and its current
/// in-memory state.
pub fn decide(
    annotations: &BTreeMap<String, String>,
    last_completed_id: Option<&str>,
    in_flight_id: Option<&str>,
) -> ActionDecision {
    let verb = match annotations.get(ACTION_KEY) {
        Some(v) if !v.is_empty() => v.as_str(),
        _ => return ActionDecision::Idle,
    };
    let id = match annotations.get(ACTION_ID_KEY) {
        Some(i) if !i.is_empty() => i.clone(),
        _ => {
            return ActionDecision::RejectMalformed(format!(
                "{} set without {}",
                ACTION_KEY, ACTION_ID_KEY
            ))
        }
    };
    if last_completed_id == Some(id.as_str()) {
        return ActionDecision::Idempotent { id };
    }
    if let Some(current) = in_flight_id {
        if current == id {
            // Same action still being processed — quiet no-op.
            return ActionDecision::Idempotent { id };
        }
        return ActionDecision::RejectInFlight {
            incoming_id: id,
            current_id: current.to_string(),
        };
    }
    let kind = ActionKind::from_verb(verb);
    let args = annotations
        .get(ACTION_ARGS_KEY)
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
}

impl ActionOutcome {
    pub fn detail(s: impl Into<String>) -> Self {
        Self {
            detail: Some(s.into()),
            pause_window_ms: None,
        }
    }
}

/// Dispatch a pending action. Returns the outcome on success or a
/// human-readable detail string on failure (mirrored into the
/// status-detail annotation).
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

/// Patch the launcher pod's status annotations. Always merge-patches
/// the four keys together so a status rewrite never leaves stale
/// detail or stale pause-window from a prior status.
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

async fn handle_pod_state(
    client: &Client,
    namespace: &str,
    pod_name: &str,
    api_socket: &Path,
    state: &mut ActionState,
    annotations: &BTreeMap<String, String>,
) {
    let last = state.last_completed_id.as_deref();
    let in_flight = state.in_flight.as_ref().map(|p| p.id.as_str());
    match decide(annotations, last, in_flight) {
        ActionDecision::Idle | ActionDecision::Idempotent { .. } => {}
        ActionDecision::RejectMalformed(reason) => {
            log::warn!("action_reject_malformed: {}", reason);
            // No action-id to mirror — we can't write a meaningful
            // status. Surface in logs only; the controller will learn
            // from the absent status and time out.
        }
        ActionDecision::RejectInFlight {
            incoming_id,
            current_id,
        } => {
            log::warn!(
                "action_reject_inflight incoming={} current={}",
                incoming_id,
                current_id
            );
            if let Err(e) = write_status(
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
        ActionDecision::Accept(pending) => {
            log::info!("action_accept kind={:?} id={}", pending.kind, pending.id);
            state.in_flight = Some(pending.clone());
            if let Err(e) = write_status(
                client,
                namespace,
                pod_name,
                &pending.id,
                StatusKind::Running,
                None,
                None,
            )
            .await
            {
                log::error!("action_status_write_failed: {}", e);
            }
            let result = dispatch(&pending, api_socket).await;
            let (status, detail, pause_window_ms) = match result {
                Ok(outcome) => (StatusKind::Ready, outcome.detail, outcome.pause_window_ms),
                Err(d) => (StatusKind::Failed, Some(d), None),
            };
            if let Err(e) = write_status(
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
        assert_eq!(decide(&a, None, None), ActionDecision::Idle);
    }

    #[test]
    fn empty_action_value_is_idle() {
        let a = ann(&[(ACTION_KEY, "")]);
        assert_eq!(decide(&a, None, None), ActionDecision::Idle);
    }

    #[test]
    fn action_without_id_is_malformed() {
        let a = ann(&[(ACTION_KEY, "capture")]);
        match decide(&a, None, None) {
            ActionDecision::RejectMalformed(_) => {}
            other => panic!("expected RejectMalformed, got {:?}", other),
        }
    }

    #[test]
    fn action_with_id_no_state_is_accepted() {
        let a = ann(&[(ACTION_KEY, "capture"), (ACTION_ID_KEY, "snap1-r42")]);
        match decide(&a, None, None) {
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
        match decide(&a, Some("snap1-r42"), None) {
            ActionDecision::Idempotent { id } => assert_eq!(id, "snap1-r42"),
            other => panic!("expected Idempotent, got {:?}", other),
        }
    }

    #[test]
    fn action_with_id_matching_in_flight_is_idempotent() {
        let a = ann(&[(ACTION_KEY, "capture"), (ACTION_ID_KEY, "snap1-r42")]);
        match decide(&a, None, Some("snap1-r42")) {
            ActionDecision::Idempotent { id } => assert_eq!(id, "snap1-r42"),
            other => panic!("expected Idempotent, got {:?}", other),
        }
    }

    #[test]
    fn different_id_while_in_flight_is_rejected() {
        let a = ann(&[(ACTION_KEY, "capture"), (ACTION_ID_KEY, "snap2-r99")]);
        match decide(&a, None, Some("snap1-r42")) {
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
        match decide(&a, None, None) {
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
        match decide(&a, None, None) {
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
        match decide(&a, None, None) {
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
}

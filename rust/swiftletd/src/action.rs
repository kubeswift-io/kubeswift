//! Annotation-driven control surface for snapshot/restore actions.
//!
//! Phase 2 commit 5 (skeleton). The SwiftSnapshot / SwiftRestore
//! controllers drive lifecycle by writing annotations onto the launcher
//! pod; swiftletd watches its own pod, dispatches the requested action
//! to a handler, and writes a status annotation back. The handlers
//! themselves are no-ops in this commit — commits 6 and 7 fill in the
//! actual pause/snapshot/resume and restore-prepare implementations.
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

/// Skeleton dispatcher. Returns `Ok(detail)` for known kinds (no-op)
/// and `Err(detail)` for unknown kinds. Commits 6 and 7 replace this
/// with the real pause/snapshot/resume and restore-prepare paths.
pub async fn dispatch(
    action: &PendingAction,
    _api_socket: &Path,
) -> Result<Option<String>, String> {
    match &action.kind {
        ActionKind::SnapshotCapture => {
            log::info!(
                "dispatch_snapshot_capture id={} args={} (skeleton no-op)",
                action.id,
                action.args
            );
            Ok(Some("skeleton no-op: capture not yet wired".to_string()))
        }
        ActionKind::SnapshotResume => {
            log::info!("dispatch_snapshot_resume id={} (skeleton no-op)", action.id);
            Ok(Some("skeleton no-op: resume not yet wired".to_string()))
        }
        ActionKind::RestorePrepare => {
            log::info!("dispatch_restore_prepare id={} (skeleton no-op)", action.id);
            Ok(Some(
                "skeleton no-op: restore-prepare not yet wired".to_string(),
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

/// Patch the launcher pod's status annotations. Always merge-patches
/// the three keys together so a status rewrite never leaves stale
/// detail from a prior status.
pub async fn write_status(
    client: &Client,
    namespace: &str,
    pod_name: &str,
    action_id: &str,
    status: StatusKind,
    detail: Option<&str>,
) -> Result<(), kube::Error> {
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let mut annotations = BTreeMap::new();
    annotations.insert(STATUS_KEY.to_string(), status.as_str().to_string());
    annotations.insert(STATUS_ID_KEY.to_string(), action_id.to_string());
    // Detail is overwritten on every write so it never lags the status.
    // Empty string is sent rather than omitted so the merge-patch
    // actually clears any prior value.
    annotations.insert(
        STATUS_DETAIL_KEY.to_string(),
        detail.unwrap_or("").to_string(),
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
            )
            .await
            {
                log::error!("action_status_write_failed: {}", e);
            }
            let result = dispatch(&pending, api_socket).await;
            let (status, detail) = match result {
                Ok(d) => (StatusKind::Ready, d),
                Err(d) => (StatusKind::Failed, Some(d)),
            };
            if let Err(e) = write_status(
                client,
                namespace,
                pod_name,
                &pending.id,
                status,
                detail.as_deref(),
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
    async fn dispatch_skeleton_handlers_succeed_for_known_kinds() {
        let api_socket = PathBuf::from("/run/foo/ch.sock");
        for kind in [
            ActionKind::SnapshotCapture,
            ActionKind::SnapshotResume,
            ActionKind::RestorePrepare,
        ] {
            let action = PendingAction {
                kind: kind.clone(),
                id: "id-1".to_string(),
                args: serde_json::Value::Null,
            };
            let res = dispatch(&action, &api_socket).await;
            assert!(res.is_ok(), "kind={:?} err={:?}", kind, res);
        }
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
}

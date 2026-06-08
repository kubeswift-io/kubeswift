//! Cloud Hypervisor REST API methods for VM lifecycle and snapshots.
//!
//! Layered on top of [`crate::api::ApiClient`]. Each method maps 1:1 to a
//! single CH endpoint:
//!
//!   - [`pause`] → PUT /api/v1/vm.pause
//!   - [`resume`] → PUT /api/v1/vm.resume
//!   - [`snapshot`] → PUT /api/v1/vm.snapshot
//!   - [`shutdown`] → PUT /api/v1/vm.shutdown
//!   - [`vm_info`] → GET /api/v1/vm.info
//!   - [`version`] → GET /api/v1/vmm.ping
//!
//! Action endpoints (pause, resume, snapshot, shutdown) return 204 No
//! Content on success and an error body otherwise. Query endpoints
//! return 200 with a JSON body. We use `request_ok` for the action
//! endpoints (auto-fail on non-2xx) and `request` for the query
//! endpoints so we can attach JSON-parse errors to the body bytes.
//!
//! # Pause window
//!
//! `vm.snapshot` blocks the API socket for the duration of the pause
//! window — measured at ~2.8s/GiB on Longhorn-backed disks during the
//! Phase 0 spike. Callers must use [`ApiClient::with_timeout`] to set
//! a generous timeout (the controller derives one from VM RAM size
//! before issuing the action annotation that drives this method).

use serde::Deserialize;

use crate::api::{ApiClient, ApiError};

/// State of a Cloud Hypervisor VM as reported by `vm.info`.
///
/// Only the `state` field is exposed because that is all the Phase 2
/// action handler in swiftletd needs. The full vm.info response also
/// carries the entire VmConfig the VM was started with; we deliberately
/// don't surface it here so callers don't grow accidental dependencies
/// on CH's internal config layout.
#[derive(Debug, Clone, Deserialize)]
pub struct VmInfo {
    /// VM lifecycle state. Common values from CH v51:
    /// "Created", "Running", "Paused", "Shutdown".
    /// Treated as opaque — we match on it but do not enumerate.
    pub state: String,
}

/// Build/version information from `vmm.ping`.
#[derive(Debug, Clone, Deserialize)]
pub struct VmmVersion {
    /// CH's own version string, e.g. "v51.1" or "v51.1.0".
    /// Format is "v<major>.<minor>" with optional ".<patch>".
    pub build_version: String,

    /// Process ID of the VMM, included in CH's response. We surface
    /// it so the controller can correlate with `kubeswift.io/guest-runtime-pid`
    /// during diagnostics, but no logic keys on it.
    #[serde(default)]
    pub pid: Option<i64>,
}

impl VmmVersion {
    /// Parse `build_version` as `(major, minor)`. Returns `None` if the
    /// string doesn't start with `v<digits>.<digits>`. Patch is dropped.
    ///
    /// The Phase 2 hypervisor-version check (per architect risk #3)
    /// compares major.minor exactly, allowing patch-level drift across
    /// routine upgrades. We deliberately do not return `(major, minor,
    /// patch)` — exposing patch would invite callers to write strict
    /// equality checks that block legitimate routine upgrades.
    pub fn major_minor(&self) -> Option<(u32, u32)> {
        let s = self
            .build_version
            .strip_prefix('v')
            .unwrap_or(&self.build_version);
        let mut parts = s.split('.');
        let major: u32 = parts.next()?.parse().ok()?;
        let minor: u32 = parts.next()?.parse().ok()?;
        Some((major, minor))
    }
}

impl ApiClient {
    /// Pause the VM (CPUs stopped, memory frozen). Idempotent — pausing
    /// an already-paused VM is a no-op on the CH side.
    pub fn pause(&self) -> Result<(), ApiError> {
        self.request_ok("PUT", "/api/v1/vm.pause", None).map(drop)
    }

    /// Resume the VM from a paused state. Idempotent in the same sense.
    pub fn resume(&self) -> Result<(), ApiError> {
        self.request_ok("PUT", "/api/v1/vm.resume", None).map(drop)
    }

    /// Capture a full snapshot (memory + disk state) to `destination_url`.
    ///
    /// `destination_url` must be `file://<absolute-path>` pointing at a
    /// pre-existing, writable directory. CH writes `config.json`,
    /// `state.json`, and `memory-ranges` into that directory. The VM
    /// must be in `Paused` state before this call, otherwise CH returns
    /// a 4xx and the snapshot is not written. Callers MUST call
    /// [`pause`] first and either [`resume`] or [`shutdown`] after.
    ///
    /// Times out per [`ApiClient::with_timeout`]; for large VMs the
    /// caller should configure a generous timeout because CH holds the
    /// API socket open for the full pause window.
    pub fn snapshot(&self, destination_url: &str) -> Result<(), ApiError> {
        let body = serde_json::to_vec(&serde_json::json!({
            "destination_url": destination_url,
        }))
        .map_err(|e| ApiError::Malformed(format!("snapshot body serialize: {}", e)))?;
        self.request_ok("PUT", "/api/v1/vm.snapshot", Some(&body))
            .map(drop)
    }

    /// Clean shutdown of the VM. Replaces the prior `connect_socket`
    /// TODO that was originally meant for this purpose.
    pub fn shutdown(&self) -> Result<(), ApiError> {
        self.request_ok("PUT", "/api/v1/vm.shutdown", None)
            .map(drop)
    }

    /// Send the running VM's state to a destination CH instance over the
    /// migration channel.
    ///
    /// This is the source-side primitive for live migration (Phase 2).
    /// `destination_url` is the URL the destination CH is listening on,
    /// in CH's own URL syntax:
    ///
    ///   - `tcp:<host>:<port>` for cross-host TCP transport. Plaintext;
    ///     Phase 2 carries this only on a trusted cluster network with
    ///     the operator-acknowledged `unsafe-plaintext: ack` gate. Phase
    ///     3 wraps this in mTLS via a sidecar.
    ///   - `unix:<path>` for same-host. Used in development only.
    ///
    /// # Blocking semantics
    ///
    /// `send-migration` blocks the API socket for the **entire pre-copy +
    /// stop-and-copy duration**. Realistic cross-node windows on the
    /// spike's 256 MiB beacon guest were ~2.9 s; a 4 GiB application VM
    /// at typical dirty rates is on the order of tens of seconds (Q2
    /// findings in `live-migration-phase-2-spike.md`). Callers MUST set
    /// a generously-sized [`ApiClient::with_timeout`] before issuing
    /// this call, OR dispatch it on a worker that does not gate the
    /// action loop's poll cadence.
    ///
    /// # Lifecycle
    ///
    /// On success the source CH process exits cleanly (Q1c finding) — the
    /// guest is now running on the destination CH. The action handler
    /// observes the source exit through the existing CH-supervision path
    /// and writes the terminal `migration-status: complete` annotation.
    ///
    /// On failure CH stays alive and the guest continues running on this
    /// side. Failure modes covered by the spike:
    ///   - F1: source killed mid-migration (process gone — caller never
    ///     observes the result here, since this method's process is dead).
    ///   - F2: destination killed mid-migration — the in-flight call
    ///     returns `ApiError::Status(500)` or similar with detail
    ///     `connection refused`; the source guest auto-resumes.
    ///   - F3: network drop — same shape as F2 once the destination
    ///     listener gives up (a few seconds).
    ///
    /// See `docs/design/live-migration-phase-2.md` §4.1 for the full
    /// blocking-semantics rationale.
    /// `downtime_ms`, when `Some`, sets CH's `downtime_ms` target (CH >=
    /// v52): CH runs pre-copy iterations until the estimated final
    /// stop-and-copy fits under this vCPU-pause budget, then commits —
    /// classical dirty-rate convergence, replacing v51.1's hardcoded
    /// 5-iteration cap. `None` omits the field so CH keeps its native
    /// behaviour (on v51.x the field is unknown and would be ignored/
    /// rejected, so callers on older CH must pass `None`).
    pub fn send_migration(
        &self,
        destination_url: &str,
        downtime_ms: Option<u64>,
    ) -> Result<(), ApiError> {
        let mut body = serde_json::json!({
            "destination_url": destination_url,
        });
        if let Some(ms) = downtime_ms {
            body["downtime_ms"] = serde_json::json!(ms);
        }
        let body = serde_json::to_vec(&body)
            .map_err(|e| ApiError::Malformed(format!("send_migration body serialize: {}", e)))?;
        self.request_ok("PUT", "/api/v1/vm.send-migration", Some(&body))
            .map(drop)
    }

    /// Open a migration receive listener on this (empty) CH and wait for
    /// the source CH to connect and stream the VM's state.
    ///
    /// This is the destination-side primitive for live migration. The
    /// CH process MUST have been started with `--api-socket` only and
    /// MUST NOT have a VM created (no `vm.create` / `vm.boot` prior to
    /// this call). See [`crate::spawn_ch_receive`] for the matching
    /// spawn primitive.
    ///
    /// `receiver_url` is the URL CH binds the listener on:
    ///
    ///   - `tcp:0.0.0.0:<port>` accepts the source's connection.
    ///   - `unix:<path>` for same-host development tests.
    ///
    /// # Blocking semantics
    ///
    /// Like [`send_migration`], this call blocks the API socket for the
    /// entire migration. CH:
    ///
    ///   1. Opens the TCP listener at `receiver_url`.
    ///   2. Accepts the source's connection.
    ///   3. Receives pre-copy iterations + stop-and-copy delta.
    ///   4. Restores VM state and **automatically transitions the VM to
    ///      `Running`** (Q1c finding — no separate `boot` or `resume`
    ///      call is needed).
    ///   5. Returns 204 No Content over this API call.
    ///
    /// The TCP listener self-terminates after a few seconds of network
    /// silence (F4 finding). The action handler MUST NOT impose an
    /// application-level timeout shorter than the realistic migration
    /// window; CH's TCP layer is the timeout source.
    ///
    /// # Failure modes
    ///
    /// - Source crashes mid-migration: CH self-terminates with an error
    ///   (F1 finding). This method returns `ApiError::Status` with the
    ///   underlying error; the destination is unrecoverable, controller
    ///   must provision a fresh destination for retry.
    /// - Source never connects: the listener gives up after the TCP
    ///   retransmit window (F4 finding); this method returns an error.
    /// - CPU-feature mismatch: CH receives the snapshot, performs the
    ///   compatibility check post-receive, and aborts pre-resume (F12
    ///   finding). This method returns `ApiError::Status` with detail
    ///   `cpu_incompat` or similar.
    ///
    /// See `docs/design/live-migration-phase-2.md` §4.1.
    pub fn receive_migration(&self, receiver_url: &str) -> Result<(), ApiError> {
        let body = serde_json::to_vec(&serde_json::json!({
            "receiver_url": receiver_url,
        }))
        .map_err(|e| ApiError::Malformed(format!("receive_migration body serialize: {}", e)))?;
        self.request_ok("PUT", "/api/v1/vm.receive-migration", Some(&body))
            .map(drop)
    }

    /// Query the VM's lifecycle state.
    pub fn vm_info(&self) -> Result<VmInfo, ApiError> {
        let resp = self.request_ok("GET", "/api/v1/vm.info", None)?;
        serde_json::from_slice::<VmInfo>(&resp.body).map_err(|e| {
            ApiError::Malformed(format!(
                "vm.info parse: {} (body={:?})",
                e,
                String::from_utf8_lossy(&resp.body)
            ))
        })
    }

    /// Query VMM build version and pid.
    pub fn version(&self) -> Result<VmmVersion, ApiError> {
        let resp = self.request_ok("GET", "/api/v1/vmm.ping", None)?;
        serde_json::from_slice::<VmmVersion>(&resp.body).map_err(|e| {
            ApiError::Malformed(format!(
                "vmm.ping parse: {} (body={:?})",
                e,
                String::from_utf8_lossy(&resp.body)
            ))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::os::unix::net::UnixListener;
    use std::path::PathBuf;
    use std::sync::mpsc;
    use std::thread;

    /// Minimal mock UDS server: handles one request, echoes raw bytes
    /// back to the test thread, and replies with the given response.
    struct MockServer {
        path: PathBuf,
        _tmp: tempfile::TempDir,
        handle: Option<thread::JoinHandle<Vec<u8>>>,
    }

    impl MockServer {
        fn spawn(response: Vec<u8>) -> Self {
            let tmp = tempfile::tempdir().unwrap();
            let path = tmp.path().join("ch.sock");
            let listener = UnixListener::bind(&path).unwrap();
            let (ready_tx, ready_rx) = mpsc::channel::<()>();
            let handle = thread::spawn(move || {
                ready_tx.send(()).unwrap();
                let (mut conn, _) = listener.accept().expect("accept");
                let mut buf = vec![0u8; 4096];
                let mut got = Vec::new();
                loop {
                    let n = conn.read(&mut buf).unwrap();
                    if n == 0 {
                        break;
                    }
                    got.extend_from_slice(&buf[..n]);
                    if find_subseq(&got, b"\r\n\r\n").is_some() {
                        if let Some(cl) = parse_content_length(&got) {
                            let header_end = find_subseq(&got, b"\r\n\r\n").unwrap() + 4;
                            if got.len() - header_end >= cl {
                                break;
                            }
                        } else {
                            break;
                        }
                    }
                }
                conn.write_all(&response).unwrap();
                got
            });
            ready_rx.recv().unwrap();
            Self {
                path,
                _tmp: tmp,
                handle: Some(handle),
            }
        }

        fn collect_request(mut self) -> Vec<u8> {
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

    #[test]
    fn pause_sends_put_with_empty_body() {
        let server = MockServer::spawn(no_content());
        let client = ApiClient::new(server.path.clone());
        client.pause().unwrap();
        let req = String::from_utf8(server.collect_request()).unwrap();
        assert!(req.starts_with("PUT /api/v1/vm.pause HTTP/1.1\r\n"));
        assert!(req.contains("Content-Length: 0\r\n"));
    }

    #[test]
    fn resume_sends_put_with_empty_body() {
        let server = MockServer::spawn(no_content());
        let client = ApiClient::new(server.path.clone());
        client.resume().unwrap();
        let req = String::from_utf8(server.collect_request()).unwrap();
        assert!(req.starts_with("PUT /api/v1/vm.resume HTTP/1.1\r\n"));
    }

    #[test]
    fn shutdown_sends_put_with_empty_body() {
        let server = MockServer::spawn(no_content());
        let client = ApiClient::new(server.path.clone());
        client.shutdown().unwrap();
        let req = String::from_utf8(server.collect_request()).unwrap();
        assert!(req.starts_with("PUT /api/v1/vm.shutdown HTTP/1.1\r\n"));
    }

    #[test]
    fn snapshot_sends_destination_url_in_body() {
        let server = MockServer::spawn(no_content());
        let client = ApiClient::new(server.path.clone());
        client
            .snapshot("file:///var/lib/kubeswift/snapshots/default-snap1")
            .unwrap();
        let req = String::from_utf8(server.collect_request()).unwrap();
        assert!(req.starts_with("PUT /api/v1/vm.snapshot HTTP/1.1\r\n"));
        assert!(req.contains("Content-Type: application/json\r\n"));
        // Body is the last segment after the blank line.
        let (_, body) = req.split_once("\r\n\r\n").unwrap();
        let parsed: serde_json::Value = serde_json::from_str(body).unwrap();
        assert_eq!(
            parsed["destination_url"],
            "file:///var/lib/kubeswift/snapshots/default-snap1"
        );
    }

    #[test]
    fn snapshot_propagates_4xx_with_body() {
        let server = MockServer::spawn(
            b"HTTP/1.1 400 Bad Request\r\nContent-Length: 25\r\n\r\nVM is not in Paused state"
                .to_vec(),
        );
        let client = ApiClient::new(server.path.clone());
        let err = client.snapshot("file:///x").unwrap_err();
        match err {
            ApiError::Status(resp) => {
                assert_eq!(resp.status, 400);
                assert_eq!(resp.body, b"VM is not in Paused state");
            }
            other => panic!("expected Status error, got {:?}", other),
        }
    }

    #[test]
    fn send_migration_sends_destination_url_in_body() {
        let server = MockServer::spawn(no_content());
        let client = ApiClient::new(server.path.clone());
        client.send_migration("tcp:10.0.0.5:6789", None).unwrap();
        let req = String::from_utf8(server.collect_request()).unwrap();
        assert!(req.starts_with("PUT /api/v1/vm.send-migration HTTP/1.1\r\n"));
        assert!(req.contains("Content-Type: application/json\r\n"));
        let (_, body) = req.split_once("\r\n\r\n").unwrap();
        let parsed: serde_json::Value = serde_json::from_str(body).unwrap();
        assert_eq!(parsed["destination_url"], "tcp:10.0.0.5:6789");
        // None -> downtime_ms omitted entirely (CH keeps native behaviour;
        // v51.x would reject an unknown field).
        assert!(parsed.get("downtime_ms").is_none());
    }

    #[test]
    fn send_migration_includes_downtime_ms_when_set() {
        let server = MockServer::spawn(no_content());
        let client = ApiClient::new(server.path.clone());
        client
            .send_migration("tcp:10.0.0.5:6789", Some(300))
            .unwrap();
        let req = String::from_utf8(server.collect_request()).unwrap();
        let (_, body) = req.split_once("\r\n\r\n").unwrap();
        let parsed: serde_json::Value = serde_json::from_str(body).unwrap();
        assert_eq!(parsed["destination_url"], "tcp:10.0.0.5:6789");
        assert_eq!(parsed["downtime_ms"], 300);
    }

    #[test]
    fn send_migration_propagates_5xx_with_body() {
        // F2/F3 in the spike: when the destination is killed mid-flight or
        // the network drops, send-migration returns an error with the
        // underlying TCP failure reason. Callers (the action handler) must
        // see the body so they can sanitize it into a category for the
        // status-detail annotation.
        let server = MockServer::spawn(
            b"HTTP/1.1 500 Internal Server Error\r\nContent-Length: 19\r\n\r\nconnection refused\n"
                .to_vec(),
        );
        let client = ApiClient::new(server.path.clone());
        let err = client
            .send_migration("tcp:10.0.0.5:6789", None)
            .unwrap_err();
        match err {
            ApiError::Status(resp) => {
                assert_eq!(resp.status, 500);
                assert!(
                    String::from_utf8_lossy(&resp.body).contains("connection refused"),
                    "body did not carry the underlying reason: {:?}",
                    resp.body
                );
            }
            other => panic!("expected Status error, got {:?}", other),
        }
    }

    #[test]
    fn receive_migration_sends_receiver_url_in_body() {
        let server = MockServer::spawn(no_content());
        let client = ApiClient::new(server.path.clone());
        client.receive_migration("tcp:0.0.0.0:6789").unwrap();
        let req = String::from_utf8(server.collect_request()).unwrap();
        assert!(req.starts_with("PUT /api/v1/vm.receive-migration HTTP/1.1\r\n"));
        assert!(req.contains("Content-Type: application/json\r\n"));
        let (_, body) = req.split_once("\r\n\r\n").unwrap();
        let parsed: serde_json::Value = serde_json::from_str(body).unwrap();
        assert_eq!(parsed["receiver_url"], "tcp:0.0.0.0:6789");
    }

    #[test]
    fn receive_migration_propagates_4xx_with_body() {
        // Q1c finding: receive-migration on a CH that already has a VM
        // created (rather than empty) errors out. Phase 2 swiftletd
        // surfaces this as `migration-status: failed, detail: ...`; the
        // body of the 4xx is what gets sanitized into the detail.
        let server = MockServer::spawn(
            b"HTTP/1.1 400 Bad Request\r\nContent-Length: 33\r\n\r\nvm already created on destination\n"
                .to_vec(),
        );
        let client = ApiClient::new(server.path.clone());
        let err = client.receive_migration("tcp:0.0.0.0:6789").unwrap_err();
        match err {
            ApiError::Status(resp) => {
                assert_eq!(resp.status, 400);
                assert!(
                    String::from_utf8_lossy(&resp.body).contains("vm already created"),
                    "body lost: {:?}",
                    resp.body
                );
            }
            other => panic!("expected Status error, got {:?}", other),
        }
    }

    #[test]
    fn vm_info_parses_state() {
        let body = br#"{"config":{},"state":"Running","memory_actual_size":2147483648}"#;
        let response = format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n", body.len());
        let mut full = response.into_bytes();
        full.extend_from_slice(body);
        let server = MockServer::spawn(full);
        let client = ApiClient::new(server.path.clone());
        let info = client.vm_info().unwrap();
        assert_eq!(info.state, "Running");
    }

    #[test]
    fn vm_info_parses_paused_state() {
        let body = br#"{"config":{"cpus":{"boot_vcpus":2}},"state":"Paused"}"#;
        let response = format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n", body.len());
        let mut full = response.into_bytes();
        full.extend_from_slice(body);
        let server = MockServer::spawn(full);
        let client = ApiClient::new(server.path.clone());
        let info = client.vm_info().unwrap();
        assert_eq!(info.state, "Paused");
    }

    #[test]
    fn vm_info_malformed_json_surfaces_body() {
        let server =
            MockServer::spawn(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nnope".to_vec());
        let client = ApiClient::new(server.path.clone());
        let err = client.vm_info().unwrap_err();
        let msg = format!("{}", err);
        assert!(msg.contains("vm.info parse"));
        assert!(msg.contains("nope"));
    }

    #[test]
    fn version_parses_build_version_and_pid() {
        let body = br#"{"build_version":"v51.1","version":"51.1.0","pid":12345}"#;
        let response = format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n", body.len());
        let mut full = response.into_bytes();
        full.extend_from_slice(body);
        let server = MockServer::spawn(full);
        let client = ApiClient::new(server.path.clone());
        let v = client.version().unwrap();
        assert_eq!(v.build_version, "v51.1");
        assert_eq!(v.pid, Some(12345));
    }

    #[test]
    fn version_pid_optional_for_forward_compat() {
        let body = br#"{"build_version":"v51.2"}"#;
        let response = format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n", body.len());
        let mut full = response.into_bytes();
        full.extend_from_slice(body);
        let server = MockServer::spawn(full);
        let client = ApiClient::new(server.path.clone());
        let v = client.version().unwrap();
        assert_eq!(v.build_version, "v51.2");
        assert_eq!(v.pid, None);
    }

    #[test]
    fn major_minor_parses_v_prefix() {
        let v = VmmVersion {
            build_version: "v51.1".into(),
            pid: None,
        };
        assert_eq!(v.major_minor(), Some((51, 1)));
    }

    #[test]
    fn major_minor_parses_v_prefix_with_patch() {
        let v = VmmVersion {
            build_version: "v51.1.0".into(),
            pid: None,
        };
        assert_eq!(v.major_minor(), Some((51, 1)));
    }

    #[test]
    fn major_minor_parses_no_prefix() {
        let v = VmmVersion {
            build_version: "51.2".into(),
            pid: None,
        };
        assert_eq!(v.major_minor(), Some((51, 2)));
    }

    #[test]
    fn major_minor_rejects_garbage() {
        let v = VmmVersion {
            build_version: "totally-not-a-version".into(),
            pid: None,
        };
        assert_eq!(v.major_minor(), None);
    }

    #[test]
    fn major_minor_rejects_minor_only() {
        let v = VmmVersion {
            build_version: "v51".into(),
            pid: None,
        };
        assert_eq!(v.major_minor(), None);
    }
}

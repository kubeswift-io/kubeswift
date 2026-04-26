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

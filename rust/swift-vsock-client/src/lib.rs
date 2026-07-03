//! Thin synchronous client for the in-guest identity agent over Cloud
//! Hypervisor's hybrid vsock.
//!
//! CH bridges the guest's AF_VSOCK to a host-side Unix socket
//! (`--vsock cid=<N>,socket=<rt>/vsock.sock`). To reach a guest AF_VSOCK
//! listener the host connects that Unix socket and performs CH's hybrid-vsock
//! handshake — writes `CONNECT <port>\n`, reads `OK <port>\n`, then the stream
//! is bridged to the guest's listener on `<port>`. (PR-0 spike pinned this.)
//!
//! Synchronous on purpose — one request/response per connection, no tokio,
//! mirroring `swift-qemu-client`'s QMP choice. swiftletd runs the call on a
//! blocking task so the async action loop keeps ticking.

use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::time::Duration;

/// AF_VSOCK port the guest agent (`cmd/kubeswift-guest-agent`) listens on.
/// Shared constant — must match the agent's `DefaultPort`.
pub const AGENT_PORT: u32 = 1024;

/// Protocol version (must match the agent's `ProtocolVersion`).
pub const PROTOCOL_VERSION: u32 = 1;

#[derive(Debug)]
pub enum VsockError {
    /// Could not connect / read / write the host-side Unix socket. This is the
    /// "agent unreachable" case (no socket, no listener, or timeout).
    Io(std::io::Error),
    /// CH's hybrid-vsock handshake did not return `OK` — the guest has no
    /// listener on the port (agent absent / not yet up).
    Handshake(String),
    /// Response was not valid JSON / could not be (de)serialized.
    Decode(String),
}

impl std::fmt::Display for VsockError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            VsockError::Io(e) => write!(f, "vsock io: {}", e),
            VsockError::Handshake(s) => write!(f, "vsock handshake: {}", s),
            VsockError::Decode(s) => write!(f, "vsock decode: {}", s),
        }
    }
}

impl std::error::Error for VsockError {}

impl From<std::io::Error> for VsockError {
    fn from(e: std::io::Error) -> Self {
        VsockError::Io(e)
    }
}

/// Identity-regeneration request sent to the guest agent. Field names match the
/// agent's `Request` (and map directly from
/// `CloneFromSnapshotSource.regenerate`).
#[derive(Debug, Clone, serde::Serialize, Default)]
pub struct IdentityRequest {
    pub v: u32,
    pub op: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub items: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mac: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub hostname: Option<String>,
    #[serde(rename = "renewLease", skip_serializing_if = "is_false")]
    pub renew_lease: bool,
}

fn is_false(b: &bool) -> bool {
    !*b
}

impl IdentityRequest {
    /// Build a `regenerate-identity` request at the current protocol version.
    pub fn regenerate(
        items: Vec<String>,
        mac: Option<String>,
        hostname: Option<String>,
        renew_lease: bool,
    ) -> Self {
        Self {
            v: PROTOCOL_VERSION,
            op: "regenerate-identity".to_string(),
            items,
            mac,
            hostname,
            renew_lease,
        }
    }
}

/// Identity-regeneration response from the guest agent. Unknown fields are
/// ignored and missing fields default (forward/backward compatible).
#[derive(Debug, Clone, serde::Deserialize, Default)]
pub struct IdentityResponse {
    #[serde(default)]
    pub v: u32,
    #[serde(default)]
    pub ok: bool,
    #[serde(default)]
    pub regenerated: Vec<String>,
    #[serde(rename = "newIP", default)]
    pub new_ip: Option<String>,
    #[serde(default)]
    pub error: Option<String>,
}

/// Connect to CH's host-side vsock Unix socket, do the CONNECT handshake, send
/// one newline-terminated request line, and read one newline-terminated
/// response line. Low-level — most callers want [`regenerate_identity`].
pub fn request_line(
    socket_path: &Path,
    port: u32,
    req_line: &[u8],
    timeout: Duration,
) -> Result<Vec<u8>, VsockError> {
    let stream = UnixStream::connect(socket_path)?;
    stream.set_read_timeout(Some(timeout))?;
    stream.set_write_timeout(Some(timeout))?;
    let mut writer = stream.try_clone()?;
    let mut reader = BufReader::new(stream);

    // CH hybrid-vsock handshake.
    writer.write_all(format!("CONNECT {}\n", port).as_bytes())?;
    writer.flush()?;
    let mut ok = String::new();
    reader.read_line(&mut ok)?;
    if !ok.starts_with("OK") {
        return Err(VsockError::Handshake(format!("expected OK, got {:?}", ok)));
    }

    // Send the request (ensure a trailing newline — the agent reads to '\n').
    writer.write_all(req_line)?;
    if !req_line.ends_with(b"\n") {
        writer.write_all(b"\n")?;
    }
    writer.flush()?;

    // Read the response line (the agent writes one line then closes).
    let mut resp = Vec::new();
    let mut buf = [0u8; 4096];
    loop {
        let n = reader.read(&mut buf)?;
        if n == 0 {
            break;
        }
        resp.extend_from_slice(&buf[..n]);
        if resp.contains(&b'\n') {
            break;
        }
    }
    Ok(resp)
}

/// Send a `regenerate-identity` request to the guest agent and parse its reply.
pub fn regenerate_identity(
    socket_path: &Path,
    req: &IdentityRequest,
    timeout: Duration,
) -> Result<IdentityResponse, VsockError> {
    let line = serde_json::to_vec(req).map_err(|e| VsockError::Decode(e.to_string()))?;
    let raw = request_line(socket_path, AGENT_PORT, &line, timeout)?;
    let trimmed = trim_trailing_newline(&raw);
    serde_json::from_slice(trimmed)
        .map_err(|e| VsockError::Decode(format!("{}: {}", e, String::from_utf8_lossy(trimmed))))
}

fn trim_trailing_newline(b: &[u8]) -> &[u8] {
    let mut end = b.len();
    while end > 0 && (b[end - 1] == b'\n' || b[end - 1] == b'\r') {
        end -= 1;
    }
    &b[..end]
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader, Write};
    use std::os::unix::net::UnixListener;
    use std::thread;

    // Spawns a fake CH hybrid-vsock endpoint: accepts one connection, expects
    // the CONNECT handshake, replies OK, reads the request line, writes `resp`.
    fn fake_agent(path: std::path::PathBuf, resp: &'static str) -> thread::JoinHandle<String> {
        let listener = UnixListener::bind(&path).unwrap();
        thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            let mut w = stream.try_clone().unwrap();
            let mut r = BufReader::new(stream);
            let mut connect = String::new();
            r.read_line(&mut connect).unwrap();
            assert!(connect.starts_with("CONNECT"), "no CONNECT: {:?}", connect);
            w.write_all(b"OK 1\n").unwrap();
            let mut req = String::new();
            r.read_line(&mut req).unwrap();
            w.write_all(resp.as_bytes()).unwrap();
            req
        })
    }

    #[test]
    fn regenerate_round_trip() {
        let dir = std::env::temp_dir().join(format!("vsock-test-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&dir);
        let sock = dir.join("vsock.sock");
        let _ = std::fs::remove_file(&sock);
        let h = fake_agent(
            sock.clone(),
            "{\"v\":1,\"ok\":true,\"regenerated\":[\"machineId\",\"macAddresses\"],\"newIP\":\"192.168.99.12\"}\n",
        );
        let req = IdentityRequest::regenerate(
            vec!["machineId".into(), "macAddresses".into()],
            Some("52:54:00:22:22:22".into()),
            Some("clone-a".into()),
            true,
        );
        let resp = regenerate_identity(&sock, &req, Duration::from_secs(5)).unwrap();
        assert!(resp.ok);
        assert_eq!(resp.new_ip.as_deref(), Some("192.168.99.12"));
        assert_eq!(resp.regenerated, vec!["machineId", "macAddresses"]);

        // the agent received a regenerate-identity request carrying our fields
        let sent = h.join().unwrap();
        assert!(
            sent.contains("\"op\":\"regenerate-identity\""),
            "sent={}",
            sent
        );
        assert!(
            sent.contains("\"mac\":\"52:54:00:22:22:22\""),
            "sent={}",
            sent
        );
        assert!(sent.contains("\"renewLease\":true"), "sent={}", sent);
        let _ = std::fs::remove_file(&sock);
    }

    #[test]
    fn handshake_refused_is_error() {
        let dir = std::env::temp_dir().join(format!("vsock-test-h-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&dir);
        let sock = dir.join("vsock.sock");
        let _ = std::fs::remove_file(&sock);
        let listener = UnixListener::bind(&sock).unwrap();
        let h = thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            let mut w = stream.try_clone().unwrap();
            let mut r = BufReader::new(stream);
            let mut connect = String::new();
            r.read_line(&mut connect).unwrap();
            // refuse: no listener on the guest port
            w.write_all(b"ERROR 19\n").unwrap();
        });
        let req = IdentityRequest::regenerate(vec![], None, None, false);
        let err = regenerate_identity(&sock, &req, Duration::from_secs(5)).unwrap_err();
        assert!(matches!(err, VsockError::Handshake(_)), "got {:?}", err);
        h.join().unwrap();
        let _ = std::fs::remove_file(&sock);
    }
}

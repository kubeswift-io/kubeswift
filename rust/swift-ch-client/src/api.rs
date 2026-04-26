//! HTTP/1.1 client over a Unix domain socket for the Cloud Hypervisor API.
//!
//! Cloud Hypervisor exposes its REST API on a Unix socket — same wire format
//! as TCP HTTP/1.1 but bound to AF_UNIX. We hand-roll a minimal client here
//! rather than pulling in hyper/hyperlocal because:
//!
//!   1. The CH API surface we use is tiny (GET/PUT, no chunked encoding,
//!      no auth, no TLS).
//!   2. Synchronous std::os::unix::net matches the rest of swift-ch-client
//!      and swift-qemu-client. Adding tokio here would force every caller
//!      into an async context for no benefit.
//!   3. The transport layer is small enough (≈150 lines) that a focused
//!      unit-test suite catches the framing edge cases more reliably than
//!      a full hyper integration would.
//!
//! Framing rules we honor on read:
//!
//!   - Status line + headers parsed line-by-line (CRLF-terminated).
//!   - Body is taken from `Content-Length` if present; otherwise read to EOF.
//!   - 204 No Content has no body regardless of headers.
//!
//! On write we always send `Connection: close` so the server closes the
//! socket after one response; this keeps the read loop trivial. CH's API
//! is request/response per call — we don't benefit from keep-alive here.
//!
//! `Content-Length` is computed from the request body; we never emit
//! `Transfer-Encoding: chunked`. For empty-body requests (the common case
//! for vm.pause/vm.resume) we still emit `Content-Length: 0` because some
//! HTTP/1.1 stacks reject PUTs without it.

use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::time::Duration;

/// Minimum response we accept before declaring the server malformed.
/// "HTTP/1.1 200" = 12 chars, status line at least 14 with CRLF.
const MIN_STATUS_LINE: usize = 14;

/// Hard cap on header section size to bound memory on a malformed peer.
/// CH responses have a handful of small headers; 16 KiB is generous.
const MAX_HEADERS_BYTES: usize = 16 * 1024;

/// Hard cap on body size for safety. CH's vm.info response is a few KiB;
/// we set the cap at 1 MiB to leave headroom for future fields without
/// allowing a runaway peer to OOM the launcher container.
const MAX_BODY_BYTES: usize = 1024 * 1024;

/// HTTP-over-Unix-socket client targeting one CH API socket.
#[derive(Debug, Clone)]
pub struct ApiClient {
    socket_path: PathBuf,
    timeout: Duration,
}

/// Result of a single HTTP request/response cycle.
#[derive(Debug, Clone)]
pub struct ApiResponse {
    pub status: u16,
    pub body: Vec<u8>,
}

impl ApiResponse {
    /// True for 2xx status codes (CH uses 200 for queries, 204 for actions).
    pub fn is_success(&self) -> bool {
        (200..300).contains(&self.status)
    }
}

/// Errors from the transport layer. Higher-level API methods (pause,
/// snapshot, etc.) wrap these with action-specific context.
#[derive(Debug)]
pub enum ApiError {
    /// Could not connect to the socket (CH not listening, wrong path).
    Connect {
        path: PathBuf,
        source: std::io::Error,
    },
    /// Could not set socket-level timeouts.
    Configure(std::io::Error),
    /// Could not write the request bytes.
    Write(std::io::Error),
    /// Could not read the response bytes.
    Read(std::io::Error),
    /// Server response did not parse as HTTP/1.1.
    Malformed(String),
    /// Server sent more bytes than our cap allows.
    ResponseTooLarge { limit: usize },
    /// Server returned a non-2xx status. Body is included for diagnostics.
    Status(ApiResponse),
}

impl std::fmt::Display for ApiError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ApiError::Connect { path, source } => {
                write!(f, "connect to {}: {}", path.display(), source)
            }
            ApiError::Configure(e) => write!(f, "configure socket: {}", e),
            ApiError::Write(e) => write!(f, "write request: {}", e),
            ApiError::Read(e) => write!(f, "read response: {}", e),
            ApiError::Malformed(s) => write!(f, "malformed response: {}", s),
            ApiError::ResponseTooLarge { limit } => {
                write!(f, "response exceeds {} bytes", limit)
            }
            ApiError::Status(resp) => write!(
                f,
                "non-2xx status {}: {}",
                resp.status,
                String::from_utf8_lossy(&resp.body)
            ),
        }
    }
}

impl std::error::Error for ApiError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            ApiError::Connect { source, .. } => Some(source),
            ApiError::Configure(e) | ApiError::Write(e) | ApiError::Read(e) => Some(e),
            _ => None,
        }
    }
}

impl ApiClient {
    /// Create a client targeting the given socket path with a default 30s
    /// timeout. CH operations are mostly fast except for snapshot+restore
    /// which can run minutes — callers issuing those should override the
    /// timeout via [`with_timeout`].
    pub fn new(socket_path: impl Into<PathBuf>) -> Self {
        Self {
            socket_path: socket_path.into(),
            timeout: Duration::from_secs(30),
        }
    }

    /// Override the socket read/write timeout. Snapshot capture for large
    /// guests can pause for tens of seconds (≈2.8s/GiB on Longhorn per the
    /// Phase 0 spike) and the controller will pass a generous timeout
    /// derived from VM RAM size.
    pub fn with_timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    /// Issue one HTTP/1.1 request. `body` is sent verbatim — for JSON
    /// requests the caller is responsible for serialization.
    pub fn request(
        &self,
        method: &str,
        path: &str,
        body: Option<&[u8]>,
    ) -> Result<ApiResponse, ApiError> {
        let mut stream = UnixStream::connect(&self.socket_path).map_err(|e| ApiError::Connect {
            path: self.socket_path.clone(),
            source: e,
        })?;
        stream
            .set_read_timeout(Some(self.timeout))
            .map_err(ApiError::Configure)?;
        stream
            .set_write_timeout(Some(self.timeout))
            .map_err(ApiError::Configure)?;

        // Build request bytes. We always send Content-Length (0 for empty
        // body) so HTTP/1.1 stacks that require it on PUT don't object.
        let mut head = format!(
            "{} {} HTTP/1.1\r\n\
             Host: localhost\r\n\
             Connection: close\r\n\
             Content-Length: {}\r\n",
            method,
            path,
            body.map_or(0, |b| b.len()),
        );
        if body.is_some() {
            head.push_str("Content-Type: application/json\r\n");
        }
        head.push_str("\r\n");

        stream.write_all(head.as_bytes()).map_err(ApiError::Write)?;
        if let Some(b) = body {
            stream.write_all(b).map_err(ApiError::Write)?;
        }
        stream.flush().map_err(ApiError::Write)?;

        let resp = read_response(&mut stream)?;
        Ok(resp)
    }

    /// Convenience: `request` plus auto-fail on non-2xx status. Most
    /// callers want this; only callers that need to interpret error bodies
    /// (e.g. version probing where a 404 is meaningful) should use
    /// `request` directly.
    pub fn request_ok(
        &self,
        method: &str,
        path: &str,
        body: Option<&[u8]>,
    ) -> Result<ApiResponse, ApiError> {
        let resp = self.request(method, path, body)?;
        if !resp.is_success() {
            return Err(ApiError::Status(resp));
        }
        Ok(resp)
    }
}

fn read_response<R: Read>(stream: &mut R) -> Result<ApiResponse, ApiError> {
    let mut reader = BufReader::new(stream);

    // Read the status line.
    let mut status_line = String::new();
    reader.read_line(&mut status_line).map_err(ApiError::Read)?;
    if status_line.len() < MIN_STATUS_LINE {
        return Err(ApiError::Malformed(format!(
            "status line too short: {:?}",
            status_line
        )));
    }
    if !status_line.starts_with("HTTP/1.") {
        return Err(ApiError::Malformed(format!(
            "not HTTP/1.x: {:?}",
            status_line.trim_end()
        )));
    }
    // "HTTP/1.1 200 OK\r\n" → take the 2nd token
    let status: u16 = status_line
        .split_whitespace()
        .nth(1)
        .ok_or_else(|| ApiError::Malformed(format!("no status code in {:?}", status_line)))?
        .parse()
        .map_err(|e| ApiError::Malformed(format!("status code parse: {}", e)))?;

    // Read headers, capturing Content-Length if present.
    let mut content_length: Option<usize> = None;
    let mut headers_total = 0usize;
    loop {
        let mut line = String::new();
        let n = reader.read_line(&mut line).map_err(ApiError::Read)?;
        headers_total += n;
        if headers_total > MAX_HEADERS_BYTES {
            return Err(ApiError::ResponseTooLarge {
                limit: MAX_HEADERS_BYTES,
            });
        }
        if n == 0 {
            return Err(ApiError::Malformed("connection closed mid-headers".into()));
        }
        // End of headers: blank line.
        if line == "\r\n" || line == "\n" {
            break;
        }
        if let Some(value) = strip_header(&line, "content-length") {
            content_length = Some(
                value
                    .trim()
                    .parse()
                    .map_err(|e| ApiError::Malformed(format!("content-length parse: {}", e)))?,
            );
        }
    }

    // 204 No Content has no body, regardless of headers.
    if status == 204 {
        return Ok(ApiResponse {
            status,
            body: Vec::new(),
        });
    }

    // Read body. If Content-Length is present, take exactly that many
    // bytes; otherwise read to EOF (we sent Connection: close so the
    // server will close after this response).
    let body = match content_length {
        Some(len) => {
            if len > MAX_BODY_BYTES {
                return Err(ApiError::ResponseTooLarge {
                    limit: MAX_BODY_BYTES,
                });
            }
            let mut buf = vec![0u8; len];
            reader.read_exact(&mut buf).map_err(ApiError::Read)?;
            buf
        }
        None => {
            let mut buf = Vec::new();
            // read_to_end honors the read timeout we set on the underlying
            // stream; if the server hangs the timeout fires.
            reader.read_to_end(&mut buf).map_err(ApiError::Read)?;
            if buf.len() > MAX_BODY_BYTES {
                return Err(ApiError::ResponseTooLarge {
                    limit: MAX_BODY_BYTES,
                });
            }
            buf
        }
    };

    Ok(ApiResponse { status, body })
}

/// Case-insensitive header match. `line` has the form `Name: value\r\n`.
/// Returns the value (with surrounding whitespace not yet trimmed).
fn strip_header<'a>(line: &'a str, name_lower: &str) -> Option<&'a str> {
    let colon = line.find(':')?;
    let (name, rest) = line.split_at(colon);
    if !name.eq_ignore_ascii_case(name_lower) {
        return None;
    }
    Some(rest[1..].trim_end_matches(['\r', '\n']))
}

/// Wait for the API socket to appear, then return an [`ApiClient`] bound
/// to it. Callers spawn CH first, then call this to gate further API use.
pub fn await_api_client(socket_path: &Path, timeout: Duration) -> Result<ApiClient, String> {
    let start = std::time::Instant::now();
    while start.elapsed() < timeout {
        if socket_path.exists() {
            return Ok(ApiClient::new(socket_path));
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    Err(format!(
        "timeout waiting for API socket at {}",
        socket_path.display()
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::net::UnixListener;
    use std::sync::mpsc;
    use std::thread;

    /// Spawn a tiny mock UDS server that handles exactly one request. The
    /// handler closure receives the raw request bytes and returns the raw
    /// response bytes. Returns the socket path (in a tempdir that lives
    /// for the test's lifetime via the returned guard) and a join handle.
    struct MockServer {
        path: PathBuf,
        _tmp: tempfile::TempDir,
        handle: Option<thread::JoinHandle<Vec<u8>>>,
    }

    impl MockServer {
        fn spawn<F>(handler: F) -> Self
        where
            F: FnOnce(&[u8]) -> Vec<u8> + Send + 'static,
        {
            let tmp = tempfile::tempdir().unwrap();
            let path = tmp.path().join("ch.sock");
            let listener = UnixListener::bind(&path).unwrap();
            let (ready_tx, ready_rx) = mpsc::channel::<()>();
            let path_clone = path.clone();
            let handle = thread::spawn(move || {
                ready_tx.send(()).unwrap();
                let (mut conn, _) = listener.accept().expect("accept");
                let mut buf = vec![0u8; 4096];
                let mut got = Vec::new();
                // Read until we see the end of headers; for tests with
                // bodies, also drain the declared content-length.
                loop {
                    let n = conn.read(&mut buf).unwrap();
                    if n == 0 {
                        break;
                    }
                    got.extend_from_slice(&buf[..n]);
                    if let Some(_pos) = find_subseq(&got, b"\r\n\r\n") {
                        // Check if there's a content-length and whether
                        // we've drained it.
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
                let response = handler(&got);
                conn.write_all(&response).unwrap();
                conn.flush().unwrap();
                drop(conn);
                let _ = path_clone;
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
            if let Some(rest) = line.strip_prefix("Content-Length: ") {
                return rest.trim().parse().ok();
            }
            if let Some(rest) = line.strip_prefix("content-length: ") {
                return rest.trim().parse().ok();
            }
        }
        None
    }

    #[test]
    fn get_returns_200_with_json_body() {
        let server = MockServer::spawn(|_req| {
            b"HTTP/1.1 200 OK\r\n\
              Content-Type: application/json\r\n\
              Content-Length: 19\r\n\
              \r\n\
              {\"version\":\"v51.1\"}"
                .to_vec()
        });
        let client = ApiClient::new(server.path.clone());
        let resp = client.request("GET", "/api/v1/vmm.ping", None).unwrap();
        assert_eq!(resp.status, 200);
        assert_eq!(resp.body, b"{\"version\":\"v51.1\"}");

        let req = server.collect_request();
        let req_s = String::from_utf8_lossy(&req);
        assert!(req_s.starts_with("GET /api/v1/vmm.ping HTTP/1.1\r\n"));
        assert!(req_s.contains("Content-Length: 0\r\n"));
        assert!(req_s.contains("Connection: close\r\n"));
    }

    #[test]
    fn put_with_json_body_sends_content_length_and_type() {
        let server = MockServer::spawn(|_req| b"HTTP/1.1 204 No Content\r\n\r\n".to_vec());
        let client = ApiClient::new(server.path.clone());
        let body = br#"{"destination_url":"file:///snap"}"#;
        let resp = client
            .request("PUT", "/api/v1/vm.snapshot", Some(body))
            .unwrap();
        assert_eq!(resp.status, 204);
        assert!(resp.body.is_empty());

        let req = server.collect_request();
        let req_s = String::from_utf8_lossy(&req);
        assert!(req_s.contains("PUT /api/v1/vm.snapshot HTTP/1.1\r\n"));
        assert!(req_s.contains("Content-Length: 34\r\n"));
        assert!(req_s.contains("Content-Type: application/json\r\n"));
        assert!(req_s.ends_with(r#"{"destination_url":"file:///snap"}"#));
    }

    #[test]
    fn no_content_204_has_empty_body() {
        let server = MockServer::spawn(|_req| b"HTTP/1.1 204 No Content\r\n\r\n".to_vec());
        let client = ApiClient::new(server.path.clone());
        let resp = client.request("PUT", "/api/v1/vm.pause", None).unwrap();
        assert_eq!(resp.status, 204);
        assert!(resp.body.is_empty());
    }

    #[test]
    fn non_2xx_returned_as_response_not_error() {
        let server = MockServer::spawn(|_req| {
            b"HTTP/1.1 400 Bad Request\r\n\
              Content-Length: 11\r\n\
              \r\n\
              bad request"
                .to_vec()
        });
        let client = ApiClient::new(server.path.clone());
        // request() should succeed at the transport level even on 4xx.
        let resp = client.request("PUT", "/api/v1/vm.pause", None).unwrap();
        assert_eq!(resp.status, 400);
        assert_eq!(resp.body, b"bad request");
        assert!(!resp.is_success());
    }

    #[test]
    fn request_ok_fails_on_non_2xx() {
        let server = MockServer::spawn(|_req| {
            b"HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n".to_vec()
        });
        let client = ApiClient::new(server.path.clone());
        let err = client.request_ok("PUT", "/api/v1/vm.pause", None);
        assert!(matches!(err, Err(ApiError::Status(_))));
    }

    #[test]
    fn body_without_content_length_reads_to_eof() {
        let server = MockServer::spawn(|_req| {
            // No Content-Length: client must read until EOF.
            b"HTTP/1.1 200 OK\r\n\r\nhello world".to_vec()
        });
        let client = ApiClient::new(server.path.clone());
        let resp = client.request("GET", "/api/v1/vm.info", None).unwrap();
        assert_eq!(resp.status, 200);
        assert_eq!(resp.body, b"hello world");
    }

    #[test]
    fn malformed_status_line_rejected() {
        let server = MockServer::spawn(|_req| b"NOT-HTTP\r\n\r\n".to_vec());
        let client = ApiClient::new(server.path.clone());
        let err = client.request("GET", "/api/v1/vmm.ping", None);
        assert!(matches!(err, Err(ApiError::Malformed(_))));
    }

    #[test]
    fn connect_failure_includes_path() {
        let client = ApiClient::new("/does/not/exist/ch.sock");
        let err = client.request("GET", "/", None).unwrap_err();
        match err {
            ApiError::Connect { path, .. } => {
                assert_eq!(path, PathBuf::from("/does/not/exist/ch.sock"));
            }
            other => panic!("expected Connect error, got {:?}", other),
        }
    }

    #[test]
    fn case_insensitive_content_length_header() {
        let server =
            MockServer::spawn(|_req| b"HTTP/1.1 200 OK\r\ncontent-length: 5\r\n\r\nhello".to_vec());
        let client = ApiClient::new(server.path.clone());
        let resp = client.request("GET", "/x", None).unwrap();
        assert_eq!(resp.body, b"hello");
    }

    #[test]
    fn strip_header_helper() {
        assert_eq!(
            strip_header("Content-Length: 42\r\n", "content-length"),
            Some(" 42")
        );
        assert_eq!(strip_header("Host: localhost\r\n", "content-length"), None);
        assert_eq!(
            strip_header("CONTENT-LENGTH:0\r\n", "content-length"),
            Some("0")
        );
    }
}

//! Mode `brokered`: the guest has no network device. Its workload's HTTP
//! proxy is the guest agent's forwarder on the guest loopback, which carries
//! every connection unchanged over vsock to the host (CID 2) on
//! `KUBESWIFT_EGRESS_VSOCK_PORT`. Cloud Hypervisor's hybrid vsock connects
//! each one to `<runtime dir>/vsock.sock_<port>`, which swiftletd binds
//! (`bind`) and hands to a helper running under the proxy uid as fd 3
//! (`KUBESWIFT_EGRESS_VSOCK_FD`):
//!
//! - the built-in broker (`serve_broker`) parses each HTTP proxy request
//!   (CONNECT or an absolute `http://` URI), allows it by the same rules as
//!   mode proxied, resolves the name itself and connects, so the guest never
//!   picks an address; or
//! - with an egress proxy, the CONNECT relay (`connect_relay_main`) passes the
//!   bytes unchanged to the proxy's `listeners.http` on loopback, and the
//!   proxy decides.
//!
//! The guest is untrusted: everything here parses its bytes defensively and
//! answers with an HTTP status rather than a silent close where it can.
use std::net::{Ipv4Addr, SocketAddr, SocketAddrV4};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::io::FromRawFd;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpStream, UnixListener, UnixStream};

use crate::egress_broker::{Config, SvidRx};

/// The subcommand of the CONNECT relay (egress proxy in mode brokered).
pub const CONNECT_RELAY: &str = "egress-connect-relay";
/// The fd the helper finds the vsock listener on.
pub const LISTEN_FD: i32 = 3;
const FD_ENV: &str = "KUBESWIFT_EGRESS_VSOCK_FD";

const HEAD_LIMIT: usize = 16 * 1024;
const HEAD_TIMEOUT: Duration = Duration::from_secs(10);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

/// The Services the allowlist reaches, from KUBESWIFT_SANDBOX_EGRESS_SERVICES
/// ("<name>.<namespace>"), and the sandbox's namespace (POD_NAMESPACE).
#[derive(Debug, Default)]
pub(crate) struct Services {
    names: Vec<(String, String)>,
    namespace: String,
}

impl Services {
    pub(crate) fn parse(list: &str, namespace: &str) -> Services {
        Services {
            names: list
                .split_whitespace()
                .filter_map(|e| e.split_once('.'))
                .map(|(n, ns)| (n.to_ascii_lowercase(), ns.to_ascii_lowercase()))
                .collect(),
            namespace: namespace.to_ascii_lowercase(),
        }
    }

    fn from_env() -> Services {
        Services::parse(
            &std::env::var("KUBESWIFT_SANDBOX_EGRESS_SERVICES").unwrap_or_default(),
            &std::env::var("POD_NAMESPACE").unwrap_or_default(),
        )
    }

    /// The name to resolve when `host` names an allowed Service: "name"
    /// (the sandbox's namespace), "name.ns", "name.ns.svc" or
    /// "name.ns.svc.<cluster domain>". The guest has no DNS, so it names
    /// Services; anything else is never resolved here, which keeps the
    /// broker from being a DNS channel out of the sandbox.
    pub(crate) fn lookup(&self, host: &str) -> Option<String> {
        let mut it = host.splitn(4, '.');
        let name = it.next()?;
        let ns = it.next().unwrap_or(&self.namespace);
        match it.next() {
            None | Some("svc") => {}
            Some(_) => return None,
        }
        self.names
            .iter()
            .any(|(n, s)| n == name && s == ns)
            .then(|| format!("{}.{}.svc", name, ns))
    }
}

/// Whether this launcher is in mode brokered.
pub fn enabled() -> bool {
    std::env::var("KUBESWIFT_SANDBOX_EGRESS").as_deref() == Ok("brokered")
}

/// The vsock port the guest forwarder dials (`KUBESWIFT_EGRESS_VSOCK_PORT`).
pub fn port_from_env() -> Result<u32, String> {
    std::env::var("KUBESWIFT_EGRESS_VSOCK_PORT")
        .ok()
        .and_then(|p| p.parse().ok())
        .filter(|p| *p != 0)
        .ok_or_else(|| "KUBESWIFT_EGRESS_VSOCK_PORT is not a vsock port".to_string())
}

/// Binds `<runtime_root>/vsock.sock_<port>`, where Cloud Hypervisor connects
/// the guest's vsock connections to host port `port`. Owner-only: Cloud
/// Hypervisor runs as root, and the helper receives the bound fd.
pub fn bind(runtime_root: &Path, port: u32) -> Result<std::os::unix::net::UnixListener, String> {
    let path = runtime_root.join(format!("vsock.sock_{}", port));
    let _ = std::fs::remove_file(&path);
    let l = std::os::unix::net::UnixListener::bind(&path)
        .map_err(|e| format!("bind {}: {}", path.display(), e))?;
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))
        .map_err(|e| format!("chmod {}: {}", path.display(), e))?;
    Ok(l)
}

/// The listener swiftletd handed down, if any.
pub fn inherited_listener() -> Result<Option<UnixListener>, String> {
    let Ok(v) = std::env::var(FD_ENV) else {
        return Ok(None);
    };
    let fd: i32 = v
        .parse()
        .map_err(|_| format!("{} {:?} is not an fd", FD_ENV, v))?;
    // SAFETY: the parent put a listening unix socket on this fd for us
    // (spawn_helper) and nothing else in this process owns it.
    let std_l = unsafe { std::os::unix::net::UnixListener::from_raw_fd(fd) };
    std_l
        .set_nonblocking(true)
        .map_err(|e| format!("{} {}: {}", FD_ENV, fd, e))?;
    UnixListener::from_std(std_l)
        .map(Some)
        .map_err(|e| format!("{} {}: {}", FD_ENV, fd, e))
}

/// The environment a helper needs to find the listener.
pub fn helper_env() -> (&'static str, String) {
    (FD_ENV, LISTEN_FD.to_string())
}

/// One parsed HTTP proxy request.
#[derive(Debug, PartialEq)]
pub(crate) struct Request {
    /// The destination as the guest wrote it (lowercased): a name or an
    /// IPv4 literal.
    pub host: String,
    pub port: u16,
    pub connect: bool,
    /// For a forward request: the head to send upstream (origin-form
    /// request line, Host set to the destination, hop-by-hop and proxy
    /// headers dropped, Connection: close).
    pub upstream_head: Vec<u8>,
    /// Bytes the client sent after the head.
    pub rest: Vec<u8>,
}

/// Why a request is refused, as the status to answer with.
#[derive(Debug, PartialEq)]
pub(crate) struct Refusal {
    pub status: u16,
    pub reason: &'static str,
}

fn refuse(status: u16, reason: &'static str) -> Refusal {
    Refusal { status, reason }
}

/// The end of the request head in buf, if it is all there.
fn head_end(buf: &[u8]) -> Option<usize> {
    buf.windows(4).position(|w| w == b"\r\n\r\n")
}

/// Parses a complete request head (and what follows it).
pub(crate) fn parse_request(buf: &[u8]) -> Result<Request, Refusal> {
    let end = head_end(buf).ok_or(refuse(400, "incomplete-head"))?;
    let head = &buf[..end];
    // Only printable ASCII, tabs and CRLF line ends: no bare CR or LF to
    // smuggle a second head past this parser.
    for (i, &b) in head.iter().enumerate() {
        let ok = match b {
            b'\r' => head.get(i + 1) == Some(&b'\n'),
            b'\n' => i > 0 && head[i - 1] == b'\r',
            b'\t' => true,
            0x20..=0x7e => true,
            _ => false,
        };
        if !ok {
            return Err(refuse(400, "bad-bytes"));
        }
    }
    let head = std::str::from_utf8(head).map_err(|_| refuse(400, "bad-bytes"))?;
    let mut lines = head.split("\r\n");
    let line = lines.next().unwrap_or_default();
    let mut parts = line.split(' ');
    let (Some(method), Some(target), Some(version), None) =
        (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        return Err(refuse(400, "bad-request-line"));
    };
    if !version.starts_with("HTTP/1.") {
        return Err(refuse(400, "bad-version"));
    }
    let rest = buf[end + 4..].to_vec();
    if method == "CONNECT" {
        let (host, port) = authority(target, None)?;
        return Ok(Request {
            host,
            port,
            connect: true,
            upstream_head: Vec::new(),
            rest,
        });
    }
    if !method.bytes().all(|b| b.is_ascii_uppercase()) || method.is_empty() {
        return Err(refuse(400, "bad-method"));
    }
    let lower = target.to_ascii_lowercase();
    let Some(after) = lower.strip_prefix("http://").map(|_| &target[7..]) else {
        // An https:// absolute URI would need TLS here; clients send
        // CONNECT for those.
        return Err(refuse(400, "not-an-http-absolute-uri"));
    };
    let split = after.find(['/', '?']).unwrap_or(after.len());
    let (auth, path) = after.split_at(split);
    let path = if path.is_empty() {
        "/".to_string()
    } else if path.starts_with('?') {
        format!("/{}", path)
    } else {
        path.to_string()
    };
    let (host, port) = authority(auth, Some(80))?;
    let mut out = format!("{} {} {}\r\n", method, path, version);
    for h in lines {
        let name = h
            .split(':')
            .next()
            .unwrap_or_default()
            .trim()
            .to_ascii_lowercase();
        if matches!(
            name.as_str(),
            "host" | "connection" | "proxy-connection" | "keep-alive" | "proxy-authorization"
        ) {
            continue;
        }
        out.push_str(h);
        out.push_str("\r\n");
    }
    if port == 80 {
        out.push_str(&format!("Host: {}\r\n", host));
    } else {
        out.push_str(&format!("Host: {}:{}\r\n", host, port));
    }
    out.push_str("Connection: close\r\n\r\n");
    Ok(Request {
        host,
        port,
        connect: false,
        upstream_head: out.into_bytes(),
        rest,
    })
}

/// host[:port] -> (lowercase host, port). No userinfo, no IPv6 literal.
fn authority(a: &str, default: Option<u16>) -> Result<(String, u16), Refusal> {
    if a.contains('@') {
        return Err(refuse(400, "userinfo"));
    }
    if a.starts_with('[') {
        return Err(refuse(403, "ipv6"));
    }
    let (host, port) = match a.rsplit_once(':') {
        Some((h, p)) => (
            h,
            p.parse::<u16>()
                .ok()
                .filter(|p| *p != 0)
                .ok_or(refuse(400, "bad-port"))?,
        ),
        None => (a, default.ok_or(refuse(400, "no-port"))?),
    };
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    if host.is_empty()
        || !host
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-')
    {
        return Err(refuse(400, "bad-host"));
    }
    Ok((host, port))
}

fn status_line(status: u16) -> &'static str {
    match status {
        400 => "400 Bad Request",
        403 => "403 Forbidden",
        431 => "431 Request Header Fields Too Large",
        502 => "502 Bad Gateway",
        504 => "504 Gateway Timeout",
        _ => "500 Internal Server Error",
    }
}

async fn answer<W: tokio::io::AsyncWrite + Unpin>(w: &mut W, status: u16) {
    let _ = w
        .write_all(
            format!(
                "HTTP/1.1 {}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                status_line(status)
            )
            .as_bytes(),
        )
        .await;
    let _ = w.shutdown().await;
}

/// Reads the request head (bounded in size and time).
async fn read_head(s: &mut UnixStream) -> Result<Vec<u8>, Refusal> {
    let mut buf = Vec::with_capacity(1024);
    let read = async {
        let mut tmp = [0u8; 4096];
        loop {
            if head_end(&buf).is_some() {
                return Ok(());
            }
            if buf.len() >= HEAD_LIMIT {
                return Err(refuse(431, "head-too-large"));
            }
            match s.read(&mut tmp).await {
                Ok(0) | Err(_) => return Err(refuse(400, "closed-before-head")),
                Ok(n) => buf.extend_from_slice(&tmp[..n]),
            }
        }
    };
    match tokio::time::timeout(HEAD_TIMEOUT, read).await {
        Ok(Ok(())) => Ok(buf),
        Ok(Err(r)) => Err(r),
        Err(_) => Err(refuse(400, "head-timeout")),
    }
}

/// The broker's accept loop in mode brokered. Returns only on error.
pub async fn serve_broker(
    listener: UnixListener,
    cfg: Arc<Config>,
    svid: Option<SvidRx>,
    limit: Arc<tokio::sync::Semaphore>,
) -> i32 {
    let services = Arc::new(Services::from_env());
    loop {
        let sock = match listener.accept().await {
            Ok((s, _)) => s,
            Err(e) => {
                log::warn!("egress_broker_accept: {}", e);
                tokio::time::sleep(Duration::from_millis(50)).await;
                continue;
            }
        };
        let Ok(permit) = limit.clone().try_acquire_owned() else {
            log::warn!("egress_broker_deny peer=vsock reason=too-many-connections");
            continue;
        };
        let cfg = cfg.clone();
        let svid = svid.clone();
        let services = services.clone();
        tokio::spawn(async move {
            handle(sock, cfg, svid, services).await;
            drop(permit);
        });
    }
}

async fn handle(
    mut s: UnixStream,
    cfg: Arc<Config>,
    svid: Option<SvidRx>,
    services: Arc<Services>,
) {
    let req = match read_head(&mut s).await.and_then(|b| parse_request(&b)) {
        Ok(r) => r,
        Err(r) => {
            log::info!("egress_broker_deny peer=vsock reason={}", r.reason);
            answer(&mut s, r.status).await;
            return;
        }
    };
    let target = format!("{}:{}", req.host, req.port);
    let deny = |reason: String| {
        log::info!(
            "egress_broker_deny peer=vsock dst={} reason={}",
            target,
            reason
        )
    };
    // The destination: an IPv4 literal by the address rules, a name by the
    // host rules, resolved here.
    let mut matched = None;
    let (dst, why) = if let Ok(ip) = req.host.parse::<Ipv4Addr>() {
        let dst = SocketAddrV4::new(ip, req.port);
        if !cfg.cidr_allows(dst) {
            deny("no-rule".into());
            answer(&mut s, 403).await;
            return;
        }
        (dst, "address".to_string())
    } else {
        let Some(name) = crate::egress_broker::normalize(req.host.as_bytes()) else {
            deny("bad-host".into());
            answer(&mut s, 400).await;
            return;
        };
        let Some(rule) = cfg.host_rule(&name, req.port).cloned() else {
            // A Service the allowlist names: resolved in the cluster, then
            // allowed by its address rule like an IPv4 literal.
            let Some(svc) = services.lookup(&name) else {
                deny(format!("no-rule name={}", name));
                answer(&mut s, 403).await;
                return;
            };
            let dst = match tokio::net::lookup_host((svc.as_str(), req.port)).await {
                Ok(mut a) => a.find_map(|a| match a {
                    SocketAddr::V4(v) => Some(v),
                    SocketAddr::V6(_) => None,
                }),
                Err(_) => None,
            };
            let Some(dst) = dst.filter(|d| cfg.cidr_allows(*d)) else {
                deny(format!("service={} not-allowed", svc));
                answer(&mut s, 403).await;
                return;
            };
            let why = format!("address service={}", svc);
            return connect_and_finish(s, req, dst, why, None, &cfg, svid).await;
        };
        let addr = match tokio::net::lookup_host((name.as_str(), req.port)).await {
            Ok(mut addrs) => addrs.find_map(|a| match a {
                SocketAddr::V4(v) => Some(v),
                SocketAddr::V6(_) => None,
            }),
            Err(e) => {
                deny(format!("name={} resolve-failed={}", name, e).replace(' ', "_"));
                answer(&mut s, 502).await;
                return;
            }
        };
        let Some(dst) = addr else {
            deny(format!("name={} no-ipv4", name));
            answer(&mut s, 502).await;
            return;
        };
        let why = format!("host={} name={}", rule.pattern, name);
        matched = Some((rule, name));
        (dst, why)
    };
    connect_and_finish(s, req, dst, why, matched, &cfg, svid).await;
}

/// Connects to the allowed dst and hands the client over (CONNECT reply or
/// rewritten head first), answering an HTTP error when that fails.
async fn connect_and_finish(
    mut s: UnixStream,
    req: Request,
    dst: SocketAddrV4,
    why: String,
    matched: Option<(crate::egress_broker::HostRule, String)>,
    cfg: &Config,
    svid: Option<SvidRx>,
) {
    let upstream = match tokio::time::timeout(CONNECT_TIMEOUT, TcpStream::connect(dst)).await {
        Ok(Ok(u)) => u,
        Ok(Err(e)) => {
            log::info!(
                "egress_broker_connect_failed peer=vsock dst={} {} error={}",
                dst,
                why,
                e.to_string().replace(' ', "_")
            );
            answer(&mut s, 502).await;
            return;
        }
        Err(_) => {
            log::info!(
                "egress_broker_connect_failed peer=vsock dst={} {} error=timeout",
                dst,
                why
            );
            answer(&mut s, 504).await;
            return;
        }
    };
    let (to_client, to_upstream): (&[u8], Vec<u8>) = if req.connect {
        (b"HTTP/1.1 200 Connection Established\r\n\r\n", req.rest)
    } else {
        let mut b = req.upstream_head;
        b.extend_from_slice(&req.rest);
        (b"", b)
    };
    let host = matched.as_ref().map(|(r, n)| (r, n.as_str()));
    let ok = crate::egress_broker::finish(
        &mut s,
        upstream,
        cfg,
        svid,
        host,
        "vsock",
        dst,
        &why,
        to_client,
        &to_upstream,
    )
    .await;
    if !ok {
        answer(&mut s, 502).await;
    }
}

/// `swiftletd egress-connect-relay`: passes each guest connection unchanged
/// to the egress proxy's HTTP listener on loopback
/// (`KUBESWIFT_EGRESS_PROXY_HTTP_PORT`). Runs until killed.
pub fn connect_relay_main() -> ! {
    let port: u16 = match std::env::var("KUBESWIFT_EGRESS_PROXY_HTTP_PORT")
        .ok()
        .and_then(|p| p.parse().ok())
        .filter(|p| *p != 0)
    {
        Some(p) => p,
        None => {
            log::error!("egress_connect_relay_config_invalid: KUBESWIFT_EGRESS_PROXY_HTTP_PORT is not a port");
            std::process::exit(2);
        }
    };
    let rt = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            log::error!("egress_connect_relay_runtime: {}", e);
            std::process::exit(1);
        }
    };
    let code = rt.block_on(async move {
        let listener = match inherited_listener() {
            Ok(Some(l)) => l,
            Ok(None) => {
                log::error!("egress_connect_relay_config_invalid: no {}", FD_ENV);
                return 2;
            }
            Err(e) => {
                log::error!("egress_connect_relay_config_invalid: {}", e);
                return 2;
            }
        };
        log::info!(
            "egress_connect_relay_listening proxy_port={} uid={}",
            port,
            unsafe { libc::getuid() }
        );
        let limit = Arc::new(tokio::sync::Semaphore::new(4096));
        loop {
            let mut s = match listener.accept().await {
                Ok((s, _)) => s,
                Err(e) => {
                    log::warn!("egress_connect_relay_accept: {}", e);
                    tokio::time::sleep(Duration::from_millis(50)).await;
                    continue;
                }
            };
            let Ok(permit) = limit.clone().try_acquire_owned() else {
                log::warn!("egress_connect_relay_deny reason=too-many-connections");
                continue;
            };
            tokio::spawn(async move {
                match tokio::time::timeout(CONNECT_TIMEOUT, TcpStream::connect(("127.0.0.1", port)))
                    .await
                {
                    Ok(Ok(mut up)) => {
                        let _ = tokio::io::copy_bidirectional(&mut s, &mut up).await;
                    }
                    _ => {
                        log::warn!("egress_connect_relay_connect_failed proxy_port={}", port);
                        answer(&mut s, 502).await;
                    }
                }
                drop(permit);
            });
        }
    });
    std::process::exit(code);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn connect() {
        let r = parse_request(
            b"CONNECT API.Example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\nTLSDATA",
        )
        .unwrap();
        assert_eq!(
            (r.host.as_str(), r.port, r.connect),
            ("api.example.com", 443, true)
        );
        assert_eq!(r.rest, b"TLSDATA");
        let r = parse_request(b"CONNECT 203.0.113.7:22 HTTP/1.0\r\n\r\n").unwrap();
        assert_eq!((r.host.as_str(), r.port), ("203.0.113.7", 22));
    }

    #[test]
    fn forward_rewrites_the_head() {
        let r = parse_request(
            b"GET http://example.com:8080/a?b=1 HTTP/1.1\r\nHost: evil.test\r\nUser-Agent: x\r\nProxy-Connection: keep-alive\r\nProxy-Authorization: Basic Zm9v\r\nConnection: keep-alive\r\nAccept: */*\r\n\r\nBODY",
        )
        .unwrap();
        assert_eq!(
            (r.host.as_str(), r.port, r.connect),
            ("example.com", 8080, false)
        );
        assert_eq!(
            String::from_utf8(r.upstream_head).unwrap(),
            "GET /a?b=1 HTTP/1.1\r\nUser-Agent: x\r\nAccept: */*\r\nHost: example.com:8080\r\nConnection: close\r\n\r\n"
        );
        assert_eq!(r.rest, b"BODY");
        let r = parse_request(b"HEAD http://example.com HTTP/1.1\r\n\r\n").unwrap();
        assert_eq!(r.port, 80);
        assert!(String::from_utf8(r.upstream_head)
            .unwrap()
            .starts_with("HEAD / HTTP/1.1\r\nHost: example.com\r\n"));
        let r = parse_request(b"GET http://example.com?q HTTP/1.1\r\n\r\n").unwrap();
        assert!(String::from_utf8(r.upstream_head)
            .unwrap()
            .starts_with("GET /?q HTTP/1.1"));
    }

    #[test]
    fn refusals() {
        let bad: &[(&[u8], u16, &str)] = &[
            (b"CONNECT example.com HTTP/1.1\r\n\r\n", 400, "no-port"),
            (b"CONNECT example.com:0 HTTP/1.1\r\n\r\n", 400, "bad-port"),
            (b"CONNECT [::1]:443 HTTP/1.1\r\n\r\n", 403, "ipv6"),
            (
                b"CONNECT user@example.com:443 HTTP/1.1\r\n\r\n",
                400,
                "userinfo",
            ),
            (
                b"CONNECT exa_mple.com:443 HTTP/1.1\r\n\r\n",
                400,
                "bad-host",
            ),
            (
                b"GET https://example.com/ HTTP/1.1\r\n\r\n",
                400,
                "not-an-http-absolute-uri",
            ),
            (
                b"GET /relative HTTP/1.1\r\n\r\n",
                400,
                "not-an-http-absolute-uri",
            ),
            (
                b"GET http://example.com/ HTTP/2\r\n\r\n",
                400,
                "bad-version",
            ),
            (
                b"GET http://example.com/ HTTP/1.1 x\r\n\r\n",
                400,
                "bad-request-line",
            ),
            (
                b"get http://example.com/ HTTP/1.1\r\n\r\n",
                400,
                "bad-method",
            ),
            (
                b"GET http://example.com/ HTTP/1.1\r\nA: b\nGET http://evil/ HTTP/1.1\r\n\r\n",
                400,
                "bad-bytes",
            ),
            (
                b"GET http://example.com/ HTTP/1.1\r\nA: \x01\r\n\r\n",
                400,
                "bad-bytes",
            ),
            (
                b"CONNECT example.com:443 HTTP/1.1\r\n",
                400,
                "incomplete-head",
            ),
        ];
        for (req, status, reason) in bad {
            assert_eq!(
                parse_request(req),
                Err(Refusal {
                    status: *status,
                    reason
                }),
                "{}",
                String::from_utf8_lossy(req)
            );
        }
    }

    /// A TCP server that records the first bytes it gets and answers "ok".
    async fn upstream() -> (u16, tokio::sync::mpsc::UnboundedReceiver<Vec<u8>>) {
        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = l.local_addr().unwrap().port();
        let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
        tokio::spawn(async move {
            loop {
                let (mut s, _) = l.accept().await.unwrap();
                let tx = tx.clone();
                tokio::spawn(async move {
                    let mut b = vec![0u8; 4096];
                    let n = s.read(&mut b).await.unwrap_or(0);
                    let _ = tx.send(b[..n].to_vec());
                    let _ = s.write_all(b"ok").await;
                });
            }
        });
        (port, rx)
    }

    async fn ask(cfg: &Arc<Config>, req: &[u8]) -> String {
        let (mut client, server) = UnixStream::pair().unwrap();
        let cfg = cfg.clone();
        tokio::spawn(handle(server, cfg, None, Arc::new(Services::default())));
        client.write_all(req).await.unwrap();
        let mut out = Vec::new();
        let _ = tokio::time::timeout(Duration::from_secs(5), async {
            let mut b = [0u8; 4096];
            while let Ok(n) = client.read(&mut b).await {
                if n == 0 {
                    break;
                }
                out.extend_from_slice(&b[..n]);
                if out.ends_with(b"ok") {
                    break;
                }
            }
        })
        .await;
        String::from_utf8_lossy(&out).into_owned()
    }

    #[tokio::test]
    async fn broker_allows_by_rule_and_refuses_the_rest() {
        let (port, mut seen) = upstream().await;
        let cfg = Arc::new(
            Config::parse(
                "15001",
                &format!("tcp,127.0.0.1/32,{}", port),
                &format!("localhost,{}", port),
            )
            .unwrap(),
        );
        // CONNECT to an allowed address: 200, then the tunnel.
        let r = ask(
            &cfg,
            format!("CONNECT 127.0.0.1:{} HTTP/1.1\r\n\r\nhello", port).as_bytes(),
        )
        .await;
        assert!(r.starts_with("HTTP/1.1 200 "), "{}", r);
        assert!(r.ends_with("ok"), "{}", r);
        assert_eq!(seen.recv().await.unwrap(), b"hello");
        // A name by its host rule, resolved by the broker.
        let r = ask(
            &cfg,
            format!("CONNECT localhost:{} HTTP/1.1\r\n\r\nx", port).as_bytes(),
        )
        .await;
        assert!(r.starts_with("HTTP/1.1 200 "), "{}", r);
        assert_eq!(seen.recv().await.unwrap(), b"x");
        // A forward request reaches upstream rewritten.
        let r = ask(
            &cfg,
            format!(
                "GET http://127.0.0.1:{}/p HTTP/1.1\r\nHost: evil.test\r\n\r\n",
                port
            )
            .as_bytes(),
        )
        .await;
        assert_eq!(r, "ok");
        let head = String::from_utf8(seen.recv().await.unwrap()).unwrap();
        assert_eq!(
            head,
            format!(
                "GET /p HTTP/1.1\r\nHost: 127.0.0.1:{}\r\nConnection: close\r\n\r\n",
                port
            )
        );
        // Another port, another name, a malformed head: refused, nothing sent.
        for (req, status) in [
            (
                format!("CONNECT 127.0.0.1:{} HTTP/1.1\r\n\r\n", port + 1),
                "403",
            ),
            (
                format!("CONNECT example.com:{} HTTP/1.1\r\n\r\n", port),
                "403",
            ),
            ("CONNECT [::1]:443 HTTP/1.1\r\n\r\n".to_string(), "403"),
            (
                "GET https://example.com/ HTTP/1.1\r\n\r\n".to_string(),
                "400",
            ),
        ] {
            let r = ask(&cfg, req.as_bytes()).await;
            assert!(
                r.starts_with(&format!("HTTP/1.1 {} ", status)),
                "{}: {}",
                req,
                r
            );
        }
        assert!(seen.try_recv().is_err());
    }

    #[test]
    fn service_names() {
        let s = Services::parse("app.sbx llm.ai", "sbx");
        assert_eq!(s.lookup("app").as_deref(), Some("app.sbx.svc"));
        assert_eq!(s.lookup("app.sbx").as_deref(), Some("app.sbx.svc"));
        assert_eq!(s.lookup("llm.ai.svc").as_deref(), Some("llm.ai.svc"));
        assert_eq!(
            s.lookup("llm.ai.svc.cluster.local").as_deref(),
            Some("llm.ai.svc")
        );
        assert_eq!(s.lookup("llm"), None); // another namespace's Service needs its namespace
        assert_eq!(s.lookup("app.other"), None);
        assert_eq!(s.lookup("app.sbx.example.com"), None);
        assert_eq!(s.lookup("evil.example.com"), None);
    }

    #[tokio::test]
    async fn bind_is_owner_only() {
        let dir = tempfile::tempdir().unwrap();
        let _l = bind(dir.path(), 1080).unwrap();
        let p = dir.path().join("vsock.sock_1080");
        let mode = std::fs::metadata(&p).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
        // Rebinding replaces a stale socket.
        drop(_l);
        assert!(bind(dir.path(), 1080).is_ok());
    }
}

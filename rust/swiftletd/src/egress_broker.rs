//! Built-in egress broker for a sandbox in network mode `proxied`.
//!
//! network-init redirects every guest TCP connection to the broker's
//! transparent listener (`KUBESWIFT_EGRESS_BROKER_PORT`) and forwards nothing.
//! For each connection the broker reads the original destination
//! (`SO_ORIGINAL_DST`) and allows it when:
//!
//! - it matches a resolved address rule (`KUBESWIFT_SANDBOX_EGRESS_ALLOW`,
//!   from `cidr` and `service` allow rules), decided on the address alone, so
//!   protocols where the server speaks first work; or
//! - the client's first bytes name a host (TLS server name or HTTP `Host`)
//!   that matches a host rule (`KUBESWIFT_SANDBOX_EGRESS_HOSTS`) on that port,
//!   AND the original destination is one of the addresses that name resolves
//!   to, so a server name alone cannot reach another address.
//!
//! Anything else is closed. No TLS termination, no credentials.
//!
//! swiftletd runs the broker as a child process (`swiftletd egress-broker`)
//! under `KUBESWIFT_EGRESS_PROXY_UID`. network-init matches that uid in
//! OUTPUT and applies the restricted rules to it (metadata, the platform deny
//! list, RFC1918 except the address rules), so the kernel, not this code, is
//! what keeps the broker away from nodes and the API server.
use std::net::{Ipv4Addr, SocketAddr, SocketAddrV4};
use std::os::unix::io::AsRawFd;
use std::os::unix::process::CommandExt;
use std::process::Command;
use std::sync::Arc;
use std::time::Duration;

use tokio::net::{TcpListener, TcpStream};

/// The subcommand the child runs.
pub const SUBCOMMAND: &str = "egress-broker";

const SNIFF_LIMIT: usize = 16 * 1024;
const SNIFF_TIMEOUT: Duration = Duration::from_secs(10);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const MAX_CONNECTIONS: usize = 4096;

#[derive(Debug, Clone, PartialEq)]
struct CidrRule {
    net: u32,
    mask: u32,
    /// 0 = every port.
    port: u16,
}

#[derive(Debug, Clone, PartialEq)]
struct HostRule {
    /// Lowercase; "*.example.com" or "api.example.com".
    pattern: String,
    /// 0 = every port.
    port: u16,
}

#[derive(Debug, Clone, Default)]
pub struct Config {
    port: u16,
    cidrs: Vec<CidrRule>,
    hosts: Vec<HostRule>,
}

impl Config {
    /// Reads the broker's configuration from the launcher environment.
    pub fn from_env() -> Result<Config, String> {
        let port = std::env::var("KUBESWIFT_EGRESS_BROKER_PORT")
            .map_err(|_| "KUBESWIFT_EGRESS_BROKER_PORT is not set".to_string())?;
        Config::parse(
            &port,
            &std::env::var("KUBESWIFT_SANDBOX_EGRESS_ALLOW").unwrap_or_default(),
            &std::env::var("KUBESWIFT_SANDBOX_EGRESS_HOSTS").unwrap_or_default(),
        )
    }

    fn parse(port: &str, allow: &str, hosts: &str) -> Result<Config, String> {
        let port: u16 = port
            .parse()
            .ok()
            .filter(|p| *p > 0)
            .ok_or_else(|| format!("bad broker port {:?}", port))?;
        let mut c = Config {
            port,
            ..Default::default()
        };
        // "<proto>,<cidr>,<port>", proto tcp or any (mode proxied has no UDP).
        for r in allow.split_whitespace() {
            let mut f = r.splitn(3, ',');
            let (proto, cidr, rport) = (f.next(), f.next(), f.next());
            let (Some(proto), Some(cidr), Some(rport)) = (proto, cidr, rport) else {
                return Err(format!("malformed allow rule {:?}", r));
            };
            if proto != "tcp" && proto != "any" {
                return Err(format!(
                    "allow rule {:?}: protocol {} in mode proxied",
                    r, proto
                ));
            }
            let (net, mask) =
                parse_cidr(cidr).ok_or_else(|| format!("allow rule {:?}: bad cidr", r))?;
            let port: u16 = rport
                .parse()
                .map_err(|_| format!("allow rule {:?}: bad port", r))?;
            c.cidrs.push(CidrRule { net, mask, port });
        }
        // "<host>,<port>".
        for r in hosts.split_whitespace() {
            let (h, p) = r
                .rsplit_once(',')
                .ok_or_else(|| format!("malformed host rule {:?}", r))?;
            let port: u16 = p
                .parse()
                .map_err(|_| format!("host rule {:?}: bad port", r))?;
            if h.is_empty()
                || !h.bytes().all(|b| {
                    b.is_ascii_lowercase()
                        || b.is_ascii_digit()
                        || b == b'.'
                        || b == b'-'
                        || b == b'*'
                })
            {
                return Err(format!("host rule {:?}: bad host", r));
            }
            c.hosts.push(HostRule {
                pattern: h.to_string(),
                port,
            });
        }
        Ok(c)
    }

    fn cidr_allows(&self, dst: SocketAddrV4) -> bool {
        let ip = u32::from(*dst.ip());
        self.cidrs
            .iter()
            .any(|r| ip & r.mask == r.net && (r.port == 0 || r.port == dst.port()))
    }

    fn host_rule(&self, name: &str, port: u16) -> Option<&HostRule> {
        self.hosts
            .iter()
            .find(|r| host_matches(&r.pattern, name) && (r.port == 0 || r.port == port))
    }
}

fn parse_cidr(s: &str) -> Option<(u32, u32)> {
    let (a, l) = s.split_once('/')?;
    let ip: Ipv4Addr = a.parse().ok()?;
    let len: u32 = l.parse().ok().filter(|l| *l <= 32)?;
    let mask = if len == 0 { 0 } else { u32::MAX << (32 - len) };
    Some((u32::from(ip) & mask, mask))
}

/// Whether host rule `pattern` matches `name` (both lowercase, no trailing
/// dot). "*.example.com" matches one or more labels under example.com.
fn host_matches(pattern: &str, name: &str) -> bool {
    match pattern.strip_prefix("*.") {
        Some(domain) => {
            name.len() > domain.len() + 1
                && name.ends_with(domain)
                && name.as_bytes()[name.len() - domain.len() - 1] == b'.'
        }
        None => pattern == name,
    }
}

/// What the first bytes of a connection say about its destination name.
#[derive(Debug, PartialEq)]
enum Sniff {
    Name(String),
    NeedMore,
    /// Not TLS or HTTP, or no name in it.
    Unknown,
}

fn normalize(name: &[u8]) -> Option<String> {
    let s = std::str::from_utf8(name)
        .ok()?
        .trim()
        .trim_end_matches('.')
        .to_ascii_lowercase();
    if s.is_empty()
        || !s
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-')
    {
        return None;
    }
    Some(s)
}

fn sniff(buf: &[u8]) -> Sniff {
    match buf.first() {
        None => Sniff::NeedMore,
        Some(0x16) => sniff_tls(buf),
        Some(b) if b.is_ascii_uppercase() => sniff_http(buf),
        _ => Sniff::Unknown,
    }
}

/// The server_name of a TLS ClientHello carried in the first record.
fn sniff_tls(buf: &[u8]) -> Sniff {
    if buf.len() < 5 {
        return Sniff::NeedMore;
    }
    if buf[1] != 0x03 {
        return Sniff::Unknown;
    }
    let rec_len = u16::from_be_bytes([buf[3], buf[4]]) as usize;
    if buf.len() < 5 + rec_len {
        return if 5 + rec_len <= SNIFF_LIMIT {
            Sniff::NeedMore
        } else {
            Sniff::Unknown
        };
    }
    parse_client_hello(&buf[5..5 + rec_len]).map_or(Sniff::Unknown, Sniff::Name)
}

fn parse_client_hello(h: &[u8]) -> Option<String> {
    let mut r = Reader { b: h, i: 0 };
    if r.u8()? != 0x01 {
        return None; // not a ClientHello
    }
    let hs_len = r.u24()? as usize;
    let mut r = Reader {
        b: r.take(hs_len)?,
        i: 0,
    };
    r.take(2 + 32)?; // version, random
    let sid = r.u8()? as usize;
    r.take(sid)?;
    let ciphers = r.u16()? as usize;
    r.take(ciphers)?;
    let comp = r.u8()? as usize;
    r.take(comp)?;
    let ext_len = r.u16()? as usize;
    let mut e = Reader {
        b: r.take(ext_len)?,
        i: 0,
    };
    while e.i < e.b.len() {
        let typ = e.u16()?;
        let len = e.u16()? as usize;
        let data = e.take(len)?;
        if typ != 0 {
            continue;
        }
        let mut s = Reader { b: data, i: 0 };
        let list = s.u16()? as usize;
        let mut l = Reader {
            b: s.take(list)?,
            i: 0,
        };
        while l.i < l.b.len() {
            let name_type = l.u8()?;
            let n = l.u16()? as usize;
            let name = l.take(n)?;
            if name_type == 0 {
                return normalize(name);
            }
        }
        return None;
    }
    None
}

struct Reader<'a> {
    b: &'a [u8],
    i: usize,
}

impl<'a> Reader<'a> {
    fn take(&mut self, n: usize) -> Option<&'a [u8]> {
        let end = self.i.checked_add(n)?;
        let s = self.b.get(self.i..end)?;
        self.i = end;
        Some(s)
    }
    fn u8(&mut self) -> Option<u8> {
        self.take(1).map(|s| s[0])
    }
    fn u16(&mut self) -> Option<u16> {
        self.take(2).map(|s| u16::from_be_bytes([s[0], s[1]]))
    }
    fn u24(&mut self) -> Option<u32> {
        self.take(3)
            .map(|s| u32::from_be_bytes([0, s[0], s[1], s[2]]))
    }
}

/// The Host header of an HTTP/1 request, without its port.
fn sniff_http(buf: &[u8]) -> Sniff {
    let method_end = buf.iter().position(|b| !b.is_ascii_uppercase());
    match method_end {
        None => {
            return if buf.len() < 16 {
                Sniff::NeedMore
            } else {
                Sniff::Unknown
            }
        }
        Some(i) if i == 0 || buf[i] != b' ' => return Sniff::Unknown,
        _ => {}
    }
    let end = find(buf, b"\r\n\r\n");
    let head = &buf[..end.unwrap_or(buf.len())];
    for line in head.split(|b| *b == b'\n').skip(1) {
        let line = line.strip_suffix(b"\r").unwrap_or(line);
        if line.len() > 5 && line[..5].eq_ignore_ascii_case(b"host:") {
            let v = &line[5..];
            let v = match v.iter().rposition(|b| *b == b':') {
                Some(c) if v[c + 1..].iter().all(u8::is_ascii_digit) => &v[..c],
                _ => v,
            };
            return normalize(v).map_or(Sniff::Unknown, Sniff::Name);
        }
    }
    if end.is_some() || buf.len() >= SNIFF_LIMIT {
        Sniff::Unknown
    } else {
        Sniff::NeedMore
    }
}

fn find(h: &[u8], n: &[u8]) -> Option<usize> {
    h.windows(n.len()).position(|w| w == n)
}

/// The destination the guest asked for, before the REDIRECT.
fn original_dst(s: &TcpStream) -> std::io::Result<SocketAddrV4> {
    let mut addr: libc::sockaddr_in = unsafe { std::mem::zeroed() };
    let mut len = std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t;
    // SO_ORIGINAL_DST (linux/netfilter_ipv4.h) = 80.
    let r = unsafe {
        libc::getsockopt(
            s.as_raw_fd(),
            libc::SOL_IP,
            80,
            &mut addr as *mut _ as *mut libc::c_void,
            &mut len,
        )
    };
    if r != 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(SocketAddrV4::new(
        Ipv4Addr::from(u32::from_be(addr.sin_addr.s_addr)),
        u16::from_be(addr.sin_port),
    ))
}

/// Peeks at the client's first bytes until they name a host, say they will
/// not, or SNIFF_TIMEOUT passes.
async fn sniff_stream(s: &TcpStream) -> Sniff {
    let mut buf = vec![0u8; SNIFF_LIMIT];
    let deadline = tokio::time::Instant::now() + SNIFF_TIMEOUT;
    let mut last = 0;
    loop {
        let n = match tokio::time::timeout_at(deadline, s.peek(&mut buf)).await {
            Ok(Ok(n)) => n,
            _ => return Sniff::Unknown,
        };
        if n == 0 {
            return Sniff::Unknown; // closed
        }
        match sniff(&buf[..n]) {
            Sniff::NeedMore if n < SNIFF_LIMIT => {
                if n == last {
                    // peek returns what is buffered without waiting for more.
                    if tokio::time::timeout_at(
                        deadline,
                        tokio::time::sleep(Duration::from_millis(10)),
                    )
                    .await
                    .is_err()
                    {
                        return Sniff::Unknown;
                    }
                }
                last = n;
            }
            Sniff::NeedMore => return Sniff::Unknown,
            v => return v,
        }
    }
}

async fn handle(mut client: TcpStream, peer: SocketAddr, cfg: Arc<Config>) {
    let dst = match original_dst(&client) {
        Ok(d) => d,
        Err(e) => {
            log::warn!(
                "egress_broker_deny peer={} reason=no-original-destination error={}",
                peer,
                e
            );
            return;
        }
    };
    if dst.port() == cfg.port
        && client
            .local_addr()
            .map(|a| a.ip() == *dst.ip())
            .unwrap_or(false)
    {
        log::warn!(
            "egress_broker_deny peer={} dst={} reason=broker-itself",
            peer,
            dst
        );
        return;
    }
    let decision = if cfg.cidr_allows(dst) {
        Ok("address".to_string())
    } else if cfg.hosts.is_empty() {
        Err("no-rule".to_string())
    } else {
        match sniff_stream(&client).await {
            Sniff::Name(name) => match cfg.host_rule(&name, dst.port()) {
                None => Err(format!("no-rule name={}", name)),
                Some(rule) => match tokio::net::lookup_host((name.as_str(), dst.port())).await {
                    Ok(addrs) => {
                        let addrs: Vec<SocketAddr> = addrs.collect();
                        if addrs
                            .iter()
                            .any(|a| a.ip() == std::net::IpAddr::V4(*dst.ip()))
                        {
                            Ok(format!("host={} name={}", rule.pattern, name))
                        } else {
                            Err(format!("name={} resolves-elsewhere", name))
                        }
                    }
                    Err(e) => Err(format!("name={} resolve-failed={}", name, e)),
                },
            },
            _ => Err("no-name".to_string()),
        }
    };
    let why = match decision {
        Ok(why) => why,
        Err(reason) => {
            log::info!(
                "egress_broker_deny peer={} dst={} reason={}",
                peer,
                dst,
                reason
            );
            return;
        }
    };
    let mut upstream = match tokio::time::timeout(CONNECT_TIMEOUT, TcpStream::connect(dst)).await {
        Ok(Ok(u)) => u,
        Ok(Err(e)) => {
            log::info!(
                "egress_broker_connect_failed peer={} dst={} {} error={}",
                peer,
                dst,
                why,
                e
            );
            return;
        }
        Err(_) => {
            log::info!(
                "egress_broker_connect_failed peer={} dst={} {} error=timeout",
                peer,
                dst,
                why
            );
            return;
        }
    };
    log::info!("egress_broker_allow peer={} dst={} {}", peer, dst, why);
    let _ = tokio::io::copy_bidirectional(&mut client, &mut upstream).await;
}

/// `swiftletd egress-broker`: serves until killed. Exits non-zero when it
/// cannot start, so the supervisor in the parent restarts it.
pub fn main() -> ! {
    let cfg = match Config::from_env() {
        Ok(c) => Arc::new(c),
        Err(e) => {
            log::error!("egress_broker_config_invalid: {}", e);
            std::process::exit(2);
        }
    };
    let rt = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            log::error!("egress_broker_runtime: {}", e);
            std::process::exit(1);
        }
    };
    let code = rt.block_on(async move {
        let listener = match TcpListener::bind(("0.0.0.0", cfg.port)).await {
            Ok(l) => l,
            Err(e) => {
                log::error!("egress_broker_bind port={} error={}", cfg.port, e);
                return 1;
            }
        };
        log::info!(
            "egress_broker_listening port={} address_rules={} host_rules={} uid={}",
            cfg.port,
            cfg.cidrs.len(),
            cfg.hosts.len(),
            unsafe { libc::getuid() }
        );
        let limit = Arc::new(tokio::sync::Semaphore::new(MAX_CONNECTIONS));
        loop {
            let (sock, peer) = match listener.accept().await {
                Ok(c) => c,
                Err(e) => {
                    log::warn!("egress_broker_accept: {}", e);
                    tokio::time::sleep(Duration::from_millis(50)).await;
                    continue;
                }
            };
            let Ok(permit) = limit.clone().try_acquire_owned() else {
                log::warn!(
                    "egress_broker_deny peer={} reason=too-many-connections",
                    peer
                );
                continue;
            };
            let cfg = cfg.clone();
            tokio::spawn(async move {
                handle(sock, peer, cfg).await;
                drop(permit);
            });
        }
    });
    std::process::exit(code);
}

/// Starts the broker when the launcher is in mode proxied, under the proxy
/// uid, restarting it if it exits, and waits until it listens. An error means
/// the broker never came up: the caller must not boot a guest that would
/// have no egress at all and no explanation.
pub fn spawn_if_proxied() -> Result<(), String> {
    if std::env::var("KUBESWIFT_SANDBOX_EGRESS").as_deref() != Ok("proxied") {
        return Ok(());
    }
    if std::env::var("KUBESWIFT_EGRESS_PROXY").as_deref() == Ok("container") {
        return wait_for_proxy();
    }
    let cfg = Config::from_env()?;
    let uid: u32 = std::env::var("KUBESWIFT_EGRESS_PROXY_UID")
        .ok()
        .and_then(|u| u.parse().ok())
        .filter(|u| *u != 0)
        .ok_or("KUBESWIFT_EGRESS_PROXY_UID is not a non-root uid")?;
    let exe = std::env::current_exe().map_err(|e| format!("current_exe: {}", e))?;
    std::thread::spawn(move || {
        let mut backoff = Duration::from_millis(500);
        loop {
            let started = std::time::Instant::now();
            match Command::new(&exe).arg(SUBCOMMAND).uid(uid).gid(uid).spawn() {
                Ok(mut child) => match child.wait() {
                    Ok(st) => log::warn!("egress_broker_exited status={}", st),
                    Err(e) => log::warn!("egress_broker_wait: {}", e),
                },
                Err(e) => log::error!("egress_broker_spawn: {}", e),
            }
            if started.elapsed() > Duration::from_secs(30) {
                backoff = Duration::from_millis(500);
            }
            std::thread::sleep(backoff);
            backoff = (backoff * 2).min(Duration::from_secs(10));
        }
    });
    let deadline = std::time::Instant::now() + Duration::from_secs(10);
    while std::time::Instant::now() < deadline {
        if listening(cfg.port) {
            return Ok(());
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    Err(format!(
        "egress broker is not listening on port {} after 10s",
        cfg.port
    ))
}

/// A proxy container (spec.network.egress.proxy) replaces the broker. It is a
/// native sidecar started before this container; wait until it listens on
/// its transparent port (and DNS port, when it has one) so the guest never
/// boots into a redirect to nothing.
fn wait_for_proxy() -> Result<(), String> {
    let port: u16 = std::env::var("KUBESWIFT_EGRESS_BROKER_PORT")
        .ok()
        .and_then(|p| p.parse().ok())
        .ok_or("KUBESWIFT_EGRESS_BROKER_PORT is not a port")?;
    let dns: Option<u16> = std::env::var("KUBESWIFT_EGRESS_PROXY_DNS_PORT")
        .ok()
        .and_then(|p| p.parse().ok());
    let deadline = std::time::Instant::now() + Duration::from_secs(60);
    while std::time::Instant::now() < deadline {
        if listening(port) && dns.map_or(true, udp_bound) {
            log::info!("egress_proxy_ready port={} dns={:?}", port, dns);
            return Ok(());
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    Err(match dns {
        Some(d) => format!(
            "egress proxy is not listening on TCP port {} and UDP port {} after 60s",
            port, d
        ),
        None => format!(
            "egress proxy is not listening on TCP port {} after 60s",
            port
        ),
    })
}

/// Whether something listens on TCP `port` in this network namespace.
fn listening(port: u16) -> bool {
    socket_in("/proc/net/tcp", port, Some("0A"))
}

/// Whether a UDP socket is bound to `port` in this network namespace.
fn udp_bound(port: u16) -> bool {
    socket_in("/proc/net/udp", port, None)
}

fn socket_in(table: &str, port: u16, state: Option<&str>) -> bool {
    let want = format!(":{:04X}", port);
    std::fs::read_to_string(table)
        .map(|t| {
            t.lines().skip(1).any(|l| {
                let f: Vec<&str> = l.split_whitespace().collect();
                f.len() > 3 && f[1].ends_with(&want) && state.map_or(true, |s| f[3] == s)
            })
        })
        .unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn client_hello(sni: &str) -> Vec<u8> {
        let name = sni.as_bytes();
        let mut sn = vec![];
        sn.extend_from_slice(&((name.len() + 3) as u16).to_be_bytes());
        sn.push(0);
        sn.extend_from_slice(&(name.len() as u16).to_be_bytes());
        sn.extend_from_slice(name);
        let mut ext = vec![];
        // An unrelated extension first (supported_groups), then server_name.
        ext.extend_from_slice(&[0x00, 0x0a, 0x00, 0x04, 0x00, 0x02, 0x00, 0x1d]);
        ext.extend_from_slice(&0u16.to_be_bytes());
        ext.extend_from_slice(&(sn.len() as u16).to_be_bytes());
        ext.extend_from_slice(&sn);
        let mut body = vec![0x03, 0x03];
        body.extend_from_slice(&[0u8; 32]);
        body.push(0); // session id
        body.extend_from_slice(&[0x00, 0x02, 0x13, 0x01]); // one cipher
        body.extend_from_slice(&[0x01, 0x00]); // null compression
        body.extend_from_slice(&(ext.len() as u16).to_be_bytes());
        body.extend_from_slice(&ext);
        let mut hs = vec![0x01];
        hs.extend_from_slice(&(body.len() as u32).to_be_bytes()[1..]);
        hs.extend_from_slice(&body);
        let mut rec = vec![0x16, 0x03, 0x01];
        rec.extend_from_slice(&(hs.len() as u16).to_be_bytes());
        rec.extend_from_slice(&hs);
        rec
    }

    #[test]
    fn tls_server_name() {
        let ch = client_hello("API.Example.com");
        assert_eq!(sniff(&ch), Sniff::Name("api.example.com".into()));
        assert_eq!(sniff(&ch[..ch.len() - 1]), Sniff::NeedMore);
        assert_eq!(sniff(&ch[..3]), Sniff::NeedMore);
        // A ClientHello without server_name names nothing.
        let mut no_sni = client_hello("a.b");
        no_sni.truncate(no_sni.len() - 12);
        let body_len = (no_sni.len() - 5) as u16;
        no_sni[3..5].copy_from_slice(&body_len.to_be_bytes());
        assert_eq!(sniff(&no_sni), Sniff::Unknown);
    }

    #[test]
    fn http_host() {
        assert_eq!(
            sniff(b"GET / HTTP/1.1\r\nUser-Agent: x\r\nHost: Api.Example.com:8080\r\n\r\n"),
            Sniff::Name("api.example.com".into())
        );
        assert_eq!(
            sniff(b"GET / HTTP/1.1\r\nUser-Agent: x\r\n"),
            Sniff::NeedMore
        );
        assert_eq!(
            sniff(b"GET / HTTP/1.1\r\nUser-Agent: x\r\n\r\n"),
            Sniff::Unknown
        );
        assert_eq!(sniff(b"GE"), Sniff::NeedMore);
        assert_eq!(sniff(b"SSH-2.0-OpenSSH_9.6\r\n"), Sniff::Unknown);
        assert_eq!(sniff(&[0x00, 0x01, 0x02]), Sniff::Unknown);
    }

    #[test]
    fn host_matching() {
        assert!(host_matches("api.example.com", "api.example.com"));
        assert!(!host_matches("api.example.com", "x.api.example.com"));
        assert!(host_matches("*.example.com", "a.example.com"));
        assert!(host_matches("*.example.com", "a.b.example.com"));
        assert!(!host_matches("*.example.com", "example.com"));
        assert!(!host_matches("*.example.com", "badexample.com"));
        assert!(!host_matches("*.example.com", "a.example.com.evil.net"));
    }

    #[test]
    fn config() {
        let c = Config::parse(
            "15001",
            "tcp,10.96.0.12/32,8000 any,203.0.113.0/24,0",
            "api.example.com,443 *.example.org,0",
        )
        .unwrap();
        assert!(c.cidr_allows("10.96.0.12:8000".parse().unwrap()));
        assert!(!c.cidr_allows("10.96.0.12:8001".parse().unwrap()));
        assert!(c.cidr_allows("203.0.113.9:22".parse().unwrap()));
        assert!(!c.cidr_allows("203.0.114.9:22".parse().unwrap()));
        assert!(c.host_rule("api.example.com", 443).is_some());
        assert!(c.host_rule("api.example.com", 80).is_none());
        assert!(c.host_rule("x.example.org", 22).is_some());
        assert!(Config::parse("15001", "udp,10.0.0.0/8,53", "").is_err());
        assert!(Config::parse("15001", "", "Bad.Host,443").is_err());
        assert!(Config::parse("0", "", "").is_err());
    }
}

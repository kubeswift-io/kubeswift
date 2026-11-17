//! PROXY protocol relay for an egress proxy that cannot read
//! `SO_ORIGINAL_DST` (`listeners.proxyProtocol`).
//!
//! network-init redirects every guest TCP connection to the relay
//! (`KUBESWIFT_EGRESS_BROKER_PORT`, the port the built-in broker would use).
//! For each connection the relay reads the original destination, connects to
//! the proxy's transparent listener on 127.0.0.1
//! (`KUBESWIFT_EGRESS_PROXY_PORT`), writes a PROXY protocol header
//! (`KUBESWIFT_EGRESS_PROXY_PROTOCOL`, v1 text or v2 binary) with the guest's
//! address and the original destination, then copies bytes both ways. It
//! decides nothing: the proxy enforces the policy.
//!
//! The relay runs under the proxy uid like the broker. network-init drops
//! the proxy's port on every interface but loopback, so a guest cannot reach
//! it and forge a header; only processes in the pod can.
use std::net::{SocketAddr, SocketAddrV4};
use std::sync::Arc;
use std::time::Duration;

use tokio::io::AsyncWriteExt;
use tokio::net::{TcpListener, TcpStream};

/// The subcommand the child runs.
pub const SUBCOMMAND: &str = "egress-relay";

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const MAX_CONNECTIONS: usize = 4096;

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Version {
    V1,
    V2,
}

impl Version {
    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "v1" => Some(Version::V1),
            "v2" => Some(Version::V2),
            _ => None,
        }
    }
}

/// What the relay needs from the environment.
#[derive(Debug, Clone)]
pub struct Config {
    pub port: u16,
    pub proxy_port: u16,
    pub version: Version,
}

impl Config {
    pub fn from_env() -> Result<Self, String> {
        let port = |k: &str| -> Result<u16, String> {
            std::env::var(k)
                .ok()
                .and_then(|p| p.parse().ok())
                .filter(|p| *p != 0)
                .ok_or(format!("{} is not a port", k))
        };
        let v = std::env::var("KUBESWIFT_EGRESS_PROXY_PROTOCOL").unwrap_or_default();
        let version = Version::parse(&v).ok_or(format!(
            "KUBESWIFT_EGRESS_PROXY_PROTOCOL {:?} is not v1 or v2",
            v
        ))?;
        let c = Config {
            port: port("KUBESWIFT_EGRESS_BROKER_PORT")?,
            proxy_port: port("KUBESWIFT_EGRESS_PROXY_PORT")?,
            version,
        };
        if c.port == c.proxy_port {
            return Err("the relay and the proxy need different ports".into());
        }
        Ok(c)
    }
}

/// Whether this launcher relays to a proxy with a PROXY protocol header.
pub fn enabled() -> bool {
    std::env::var("KUBESWIFT_EGRESS_PROXY_PROTOCOL").is_ok_and(|v| !v.is_empty())
}

/// The PROXY protocol header for a TCP/IPv4 connection from `src` to `dst`.
pub fn header(v: Version, src: SocketAddrV4, dst: SocketAddrV4) -> Vec<u8> {
    match v {
        Version::V1 => format!(
            "PROXY TCP4 {} {} {} {}\r\n",
            src.ip(),
            dst.ip(),
            src.port(),
            dst.port()
        )
        .into_bytes(),
        Version::V2 => {
            let mut h = Vec::with_capacity(28);
            h.extend_from_slice(b"\r\n\r\n\0\r\nQUIT\n");
            h.push(0x21); // version 2, command PROXY
            h.push(0x11); // AF_INET, STREAM
            h.extend_from_slice(&12u16.to_be_bytes());
            h.extend_from_slice(&src.ip().octets());
            h.extend_from_slice(&dst.ip().octets());
            h.extend_from_slice(&src.port().to_be_bytes());
            h.extend_from_slice(&dst.port().to_be_bytes());
            h
        }
    }
}

async fn handle(client: TcpStream, peer: SocketAddr, cfg: Arc<Config>) {
    // No original destination means the connection was not redirected:
    // nothing to tell the proxy, so nothing to relay.
    let dst = match crate::egress_broker::original_dst(&client) {
        Ok(d) => d,
        Err(e) => {
            log::warn!("egress_relay_no_original_dst peer={} error={}", peer, e);
            return;
        }
    };
    let SocketAddr::V4(src) = peer else {
        log::warn!("egress_relay_deny peer={} reason=not-ipv4", peer);
        return;
    };
    relay(client, src, dst, &cfg).await;
}

async fn relay(mut client: TcpStream, src: SocketAddrV4, dst: SocketAddrV4, cfg: &Config) {
    let peer = src;
    let mut up = match tokio::time::timeout(
        CONNECT_TIMEOUT,
        TcpStream::connect(("127.0.0.1", cfg.proxy_port)),
    )
    .await
    {
        Ok(Ok(s)) => s,
        Ok(Err(e)) => {
            log::warn!(
                "egress_relay_connect_failed peer={} dst={} error={}",
                peer,
                dst,
                e
            );
            return;
        }
        Err(_) => {
            log::warn!(
                "egress_relay_connect_failed peer={} dst={} error=timeout",
                peer,
                dst
            );
            return;
        }
    };
    let _ = client.set_nodelay(true);
    let _ = up.set_nodelay(true);
    if let Err(e) = up.write_all(&header(cfg.version, src, dst)).await {
        log::warn!(
            "egress_relay_header_failed peer={} dst={} error={}",
            peer,
            dst,
            e
        );
        return;
    }
    let _ = tokio::io::copy_bidirectional(&mut client, &mut up).await;
}

/// `swiftletd egress-relay`: runs until killed.
pub fn main() -> ! {
    let cfg = match Config::from_env() {
        Ok(c) => Arc::new(c),
        Err(e) => {
            log::error!("egress_relay_config_invalid: {}", e);
            std::process::exit(2);
        }
    };
    let rt = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            log::error!("egress_relay_runtime: {}", e);
            std::process::exit(1);
        }
    };
    let code = rt.block_on(async move {
        let listener = match TcpListener::bind(("0.0.0.0", cfg.port)).await {
            Ok(l) => l,
            Err(e) => {
                log::error!("egress_relay_bind port={} error={}", cfg.port, e);
                return 1;
            }
        };
        log::info!(
            "egress_relay_listening port={} proxy_port={} version={:?} uid={}",
            cfg.port,
            cfg.proxy_port,
            cfg.version,
            unsafe { libc::getuid() }
        );
        let limit = Arc::new(tokio::sync::Semaphore::new(MAX_CONNECTIONS));
        loop {
            let (sock, peer) = match listener.accept().await {
                Ok(c) => c,
                Err(e) => {
                    log::warn!("egress_relay_accept: {}", e);
                    tokio::time::sleep(Duration::from_millis(50)).await;
                    continue;
                }
            };
            let Ok(permit) = limit.clone().try_acquire_owned() else {
                log::warn!(
                    "egress_relay_deny peer={} reason=too-many-connections",
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::Ipv4Addr;

    fn a(ip: [u8; 4], port: u16) -> SocketAddrV4 {
        SocketAddrV4::new(Ipv4Addr::from(ip), port)
    }

    #[test]
    fn v1_header() {
        let h = header(
            Version::V1,
            a([192, 168, 99, 10], 40000),
            a([93, 184, 215, 14], 443),
        );
        assert_eq!(h, b"PROXY TCP4 192.168.99.10 93.184.215.14 40000 443\r\n");
    }

    #[test]
    fn v2_header() {
        let h = header(
            Version::V2,
            a([192, 168, 99, 10], 40000),
            a([93, 184, 215, 14], 443),
        );
        assert_eq!(h.len(), 28);
        assert_eq!(&h[..12], b"\r\n\r\n\0\r\nQUIT\n");
        assert_eq!(&h[12..16], &[0x21, 0x11, 0, 12]);
        assert_eq!(&h[16..20], &[192, 168, 99, 10]);
        assert_eq!(&h[20..24], &[93, 184, 215, 14]);
        assert_eq!(&h[24..26], &40000u16.to_be_bytes());
        assert_eq!(&h[26..28], &443u16.to_be_bytes());
    }

    #[test]
    fn versions() {
        assert_eq!(Version::parse("v1"), Some(Version::V1));
        assert_eq!(Version::parse("v2"), Some(Version::V2));
        assert_eq!(Version::parse("v3"), None);
        assert_eq!(Version::parse(""), None);
    }

    #[tokio::test]
    async fn relays_with_header() {
        // The "proxy": reads the header, then echoes.
        let proxy = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let pport = proxy.local_addr().unwrap().port();
        let srv = tokio::spawn(async move {
            use tokio::io::AsyncReadExt;
            let (mut s, _) = proxy.accept().await.unwrap();
            let mut h = [0u8; 28];
            s.read_exact(&mut h).await.unwrap();
            let mut b = [0u8; 5];
            s.read_exact(&mut b).await.unwrap();
            s.write_all(&b).await.unwrap();
            h
        });
        // No REDIRECT here (no SO_ORIGINAL_DST): call relay() with a
        // destination of our choosing.
        let lst = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let rport = lst.local_addr().unwrap().port();
        let cfg = Config {
            port: rport,
            proxy_port: pport,
            version: Version::V2,
        };
        tokio::spawn(async move {
            let (s, peer) = lst.accept().await.unwrap();
            let SocketAddr::V4(src) = peer else { panic!() };
            relay(s, src, a([93, 184, 215, 14], 443), &cfg).await;
        });
        let mut c = TcpStream::connect(("127.0.0.1", rport)).await.unwrap();
        c.write_all(b"hello").await.unwrap();
        let mut back = [0u8; 5];
        use tokio::io::AsyncReadExt;
        c.read_exact(&mut back).await.unwrap();
        assert_eq!(&back, b"hello");
        let h = srv.await.unwrap();
        assert_eq!(&h[..12], b"\r\n\r\n\0\r\nQUIT\n");
        assert_eq!(&h[20..24], &[93, 184, 215, 14]);
        assert_eq!(&h[26..28], &443u16.to_be_bytes());
    }
}

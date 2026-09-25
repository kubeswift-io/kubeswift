//! Watching a live-migration send's connection from the source launcher.
//!
//! On Cloud Hypervisor v53 `vm.send-migration` returns as soon as CH accepts
//! the migration, and the source API has no failure signal: a failed transfer
//! auto-resumes the source guest, which then reads `Running` exactly as it did
//! while the transfer ran (see the v53 spike). swiftletd learned of a failure
//! only from its action deadline, 600 s after a cancel had already stopped the
//! destination, and its action slot stayed busy that long: the guest could not
//! be migrated again, and a send written meanwhile waited to run (lab
//! validation of v0.15.0, round 2).
//!
//! CH shares the pod's network namespace with swiftletd, so its connection to
//! the migration target shows in `/proc/net/tcp` / `tcp6`. A transfer holds
//! that connection open until it ends; once it is gone for a while and the
//! guest still runs here, the transfer has failed. A successful transfer ends
//! with the source CH exiting, which the caller checks first.

use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::time::{Duration, Instant};

/// How long the connection must stay gone, with the guest still running,
/// before the send is declared failed. A completed transfer's source CH exits
/// within seconds of closing it (3.4 s in the lab), so this leaves it room.
pub const GONE_GRACE: Duration = Duration::from_secs(20);

/// How long to wait for the connection to appear at all before its absence
/// counts. CH dials the target as soon as it accepts the migration.
pub const APPEAR_GRACE: Duration = Duration::from_secs(60);

/// The target of a send, from its `tcp:<host>:<port>` URL. None for anything
/// else (a `unix:` target, or a host name), which is not watched.
pub fn parse_target(url: &str) -> Option<SocketAddr> {
    let rest = url.strip_prefix("tcp:")?;
    let (host, port) = rest.rsplit_once(':')?;
    let host = host.trim_start_matches('[').trim_end_matches(']');
    let ip: IpAddr = host.parse().ok()?;
    Some(SocketAddr::new(ip, port.parse().ok()?))
}

/// Whether a `/proc/net/tcp` or `tcp6` table holds a socket connected, or
/// connecting, to `target` (states ESTABLISHED 01 and SYN_SENT 02).
pub fn table_connects_to(table: &str, target: SocketAddr) -> bool {
    table.lines().skip(1).any(|line| {
        let mut fields = line.split_whitespace();
        let (Some(_sl), Some(_local), Some(remote), Some(state)) =
            (fields.next(), fields.next(), fields.next(), fields.next())
        else {
            return false;
        };
        (state == "01" || state == "02") && parse_proc_addr(remote) == Some(target)
    })
}

/// A `/proc/net/tcp{,6}` address, `<hex ip>:<hex port>`. The IP is written as
/// 32-bit words in host (little-endian) order.
fn parse_proc_addr(s: &str) -> Option<SocketAddr> {
    let (ip_hex, port_hex) = s.split_once(':')?;
    let port = u16::from_str_radix(port_hex, 16).ok()?;
    let mut bytes = Vec::with_capacity(16);
    for chunk in ip_hex.as_bytes().chunks(8) {
        let word = u32::from_str_radix(std::str::from_utf8(chunk).ok()?, 16).ok()?;
        bytes.extend_from_slice(&word.to_le_bytes());
    }
    let ip = match bytes.len() {
        4 => IpAddr::V4(Ipv4Addr::new(bytes[0], bytes[1], bytes[2], bytes[3])),
        16 => {
            let mut b = [0u8; 16];
            b.copy_from_slice(&bytes);
            IpAddr::V6(Ipv6Addr::from(b))
        }
        _ => return None,
    };
    Some(SocketAddr::new(ip, port))
}

/// Whether this network namespace has a connection to `target`, from the
/// kernel's socket tables. None when they cannot be read: the caller then
/// does not watch.
pub fn connected_to(target: SocketAddr) -> Option<bool> {
    let path = match target {
        SocketAddr::V4(_) => "/proc/net/tcp",
        SocketAddr::V6(_) => "/proc/net/tcp6",
    };
    std::fs::read_to_string(path)
        .ok()
        .map(|t| table_connects_to(&t, target))
}

/// Decides, sample by sample, when a send has failed: its connection has been
/// gone for [`GONE_GRACE`] while the guest still runs here (or never showed up
/// within [`APPEAR_GRACE`]).
#[derive(Debug)]
pub struct Watch {
    pub target: SocketAddr,
    started: Instant,
    seen: bool,
    gone_since: Option<Instant>,
}

impl Watch {
    pub fn new(target: SocketAddr, started: Instant) -> Self {
        Self {
            target,
            started,
            seen: false,
            gone_since: None,
        }
    }

    /// One sample. `guest_running`: the source's `vm.info` answered
    /// `Running` (a teardown transient is not). `connected`: None when the
    /// socket tables were unreadable, which never fails the send. Returns
    /// the failure reason once the send has failed.
    pub fn sample(
        &mut self,
        now: Instant,
        guest_running: bool,
        connected: Option<bool>,
    ) -> Option<String> {
        match connected {
            None => return None,
            Some(true) => {
                self.seen = true;
                self.gone_since = None;
                return None;
            }
            Some(false) => {}
        }
        if !guest_running {
            return None;
        }
        let since = *self.gone_since.get_or_insert(now);
        let failed = if self.seen {
            now.duration_since(since) >= GONE_GRACE
        } else {
            now.duration_since(self.started) >= APPEAR_GRACE
                && now.duration_since(since) >= GONE_GRACE
        };
        failed.then(|| {
            format!(
                "the migration connection to {} {}, and the source guest is still running \
                 (the transfer failed; CH v53 auto-resumed the source)",
                self.target,
                if self.seen { "closed" } else { "never opened" }
            )
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Captured shape of /proc/net/tcp: a listener, the CH -> 10.244.1.42:6789
    // migration connection (ESTABLISHED), and a loopback one.
    const TABLE: &str = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0501F40A:B3C4 2A01F40A:1A85 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:C350 0100007F:1A86 01 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 20 4 30 10 -1
";

    #[test]
    fn parses_send_targets() {
        assert_eq!(
            parse_target("tcp:10.244.1.42:6789"),
            Some("10.244.1.42:6789".parse().unwrap())
        );
        assert_eq!(
            parse_target("tcp:127.0.0.1:6790"),
            Some("127.0.0.1:6790".parse().unwrap())
        );
        assert_eq!(
            parse_target("tcp:[fd00::2a]:6789"),
            Some("[fd00::2a]:6789".parse().unwrap())
        );
        assert_eq!(parse_target("unix:/run/mig.sock"), None);
        assert_eq!(parse_target("tcp:dest.example:6789"), None);
    }

    #[test]
    fn finds_the_migration_connection() {
        assert!(table_connects_to(
            TABLE,
            "10.244.1.42:6789".parse().unwrap()
        ));
        assert!(table_connects_to(TABLE, "127.0.0.1:6790".parse().unwrap()));
        assert!(!table_connects_to(
            TABLE,
            "10.244.1.43:6789".parse().unwrap()
        ));
        // A connection in another state (here CLOSE_WAIT, 08) is not one.
        let closing = TABLE.replace("2A01F40A:1A85 01", "2A01F40A:1A85 08");
        assert!(!table_connects_to(
            &closing,
            "10.244.1.42:6789".parse().unwrap()
        ));
        // SYN_SENT (02): still dialing a destination that does not answer.
        let dialing = TABLE.replace("2A01F40A:1A85 01", "2A01F40A:1A85 02");
        assert!(table_connects_to(
            &dialing,
            "10.244.1.42:6789".parse().unwrap()
        ));
    }

    #[test]
    fn finds_an_ipv6_connection() {
        // fd00::2a, port 6789, as the kernel writes it: four 32-bit words,
        // each little-endian.
        let table =
            "  sl  local_address                         remote_address                        st
   0: 0000000000000000FFFF00000100007F:C350 000000FD00000000000000002A000000:1A85 01
";
        assert!(table_connects_to(table, "[fd00::2a]:6789".parse().unwrap()));
        assert!(!table_connects_to(
            table,
            "[fd00::2b]:6789".parse().unwrap()
        ));
    }

    #[test]
    fn a_closed_connection_fails_the_send_after_the_grace() {
        let t0 = Instant::now();
        let mut w = Watch::new("10.244.1.42:6789".parse().unwrap(), t0);
        assert_eq!(w.sample(t0, true, Some(true)), None);
        // Gone, guest still running: not yet.
        assert_eq!(
            w.sample(t0 + Duration::from_secs(1), true, Some(false)),
            None
        );
        assert_eq!(
            w.sample(t0 + Duration::from_secs(20), true, Some(false)),
            None
        );
        let failed = w.sample(t0 + Duration::from_secs(21), true, Some(false));
        assert!(failed.unwrap().contains("closed"));
    }

    #[test]
    fn a_reappearing_connection_restarts_the_grace() {
        let t0 = Instant::now();
        let mut w = Watch::new("10.244.1.42:6789".parse().unwrap(), t0);
        w.sample(t0, true, Some(true));
        w.sample(t0 + Duration::from_secs(1), true, Some(false));
        w.sample(t0 + Duration::from_secs(15), true, Some(true));
        assert_eq!(
            w.sample(t0 + Duration::from_secs(30), true, Some(false)),
            None
        );
    }

    #[test]
    fn never_fails_while_the_guest_is_not_running_or_tables_are_unreadable() {
        let t0 = Instant::now();
        let mut w = Watch::new("10.244.1.42:6789".parse().unwrap(), t0);
        w.sample(t0, true, Some(true));
        // A completed transfer's teardown: the connection closes, vm.info
        // stops answering Running, and the source exits.
        assert_eq!(
            w.sample(t0 + Duration::from_secs(60), false, Some(false)),
            None
        );
        let mut w = Watch::new("10.244.1.42:6789".parse().unwrap(), t0);
        assert_eq!(w.sample(t0 + Duration::from_secs(600), true, None), None);
    }

    #[test]
    fn a_connection_that_never_opens_fails_later() {
        let t0 = Instant::now();
        let mut w = Watch::new("10.244.1.42:6789".parse().unwrap(), t0);
        assert_eq!(
            w.sample(t0 + Duration::from_secs(1), true, Some(false)),
            None
        );
        assert_eq!(
            w.sample(t0 + Duration::from_secs(59), true, Some(false)),
            None
        );
        let failed = w.sample(t0 + Duration::from_secs(61), true, Some(false));
        assert!(failed.unwrap().contains("never opened"));
    }
}

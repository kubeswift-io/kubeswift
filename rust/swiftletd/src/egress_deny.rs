//! Keeps a restricted sandbox's platform egress deny list current.
//!
//! network-init builds the `KUBESWIFT_SBX_DENY` iptables chain at boot from a
//! downward-API file of the pod's `sandbox.kubeswift.io/egress-deny`
//! annotation: every node's InternalIP/ExternalIP, the API server endpoints
//! and the admin-set CIDRs. The controller updates the annotation when that
//! set changes (a node joins), kubelet rewrites the file, and this loop
//! replaces the chain. No API access is needed, so it works on a launcher
//! that cannot reach the API server.
//!
//! The chain is replaced in one `iptables-restore --noflush` transaction, so
//! the guest never sees a half-written list. A malformed list is refused
//! whole and the chain left as it was.

use std::io::Write;
use std::net::Ipv4Addr;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::Duration;

/// Where the controller mounts the list (EgressDenyDir/EgressDenyFile).
pub const DENY_FILE: &str = "/etc/kubeswift/egress-deny/deny";
const CHAIN: &str = "KUBESWIFT_SBX_DENY";
const INTERVAL: Duration = Duration::from_secs(10);

/// Starts the loop when the pod carries the list (restricted sandboxes only).
pub fn spawn_if_present() {
    let path = PathBuf::from(DENY_FILE);
    if !path.exists() {
        return;
    }
    std::thread::spawn(move || run(&path));
}

fn run(path: &Path) {
    // network-init applied the file as it was at boot; apply again only when
    // it differs from that.
    let mut applied = std::fs::read_to_string(path).ok();
    log::info!("egress_deny_watch_started file={}", path.display());
    loop {
        std::thread::sleep(INTERVAL);
        let Ok(now) = std::fs::read_to_string(path) else {
            continue;
        };
        if applied.as_deref() == Some(now.as_str()) {
            continue;
        }
        match restore_input(&now).and_then(|input| iptables_restore(&input)) {
            Ok(n) => {
                log::info!("egress_deny_applied entries={}", n);
                applied = Some(now);
            }
            // Retried on the next tick; the previous chain stays in place.
            Err(e) => log::warn!("egress_deny_apply_failed: {}", e),
        }
    }
}

/// The `iptables-restore --noflush` input that replaces the chain with the
/// list. Declaring the chain flushes it inside the transaction.
fn restore_input(list: &str) -> Result<(String, usize), String> {
    let mut out = format!("*filter\n:{} - [0:0]\n", CHAIN);
    let mut n = 0;
    for entry in list.split_whitespace() {
        let cidr = parse_cidr(entry)?;
        out.push_str(&format!("-A {} -d {} -j DROP\n", CHAIN, cidr));
        n += 1;
    }
    out.push_str("COMMIT\n");
    Ok((out, n))
}

/// An IPv4 address or CIDR, rendered as CIDR. Anything else is an error, so
/// nothing but an address reaches iptables.
fn parse_cidr(entry: &str) -> Result<String, String> {
    let (addr, len) = match entry.split_once('/') {
        Some((a, l)) => (a, l),
        None => (entry, "32"),
    };
    let addr: Ipv4Addr = addr
        .parse()
        .map_err(|_| format!("malformed egress deny entry {:?}", entry))?;
    let len: u8 = len
        .parse()
        .ok()
        .filter(|l| *l <= 32)
        .ok_or_else(|| format!("malformed egress deny entry {:?}", entry))?;
    Ok(format!("{}/{}", addr, len))
}

fn iptables_restore((input, n): &(String, usize)) -> Result<usize, String> {
    let mut child = Command::new("iptables-restore")
        .arg("--noflush")
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("iptables-restore: {}", e))?;
    child
        .stdin
        .take()
        .ok_or("iptables-restore: no stdin")?
        .write_all(input.as_bytes())
        .map_err(|e| format!("iptables-restore: {}", e))?;
    let out = child
        .wait_with_output()
        .map_err(|e| format!("iptables-restore: {}", e))?;
    if !out.status.success() {
        return Err(format!(
            "iptables-restore: {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    Ok(*n)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn renders_one_transaction() {
        let (input, n) = restore_input("10.0.0.5/32 203.0.113.0/24\n198.51.100.7").unwrap();
        assert_eq!(n, 3);
        assert_eq!(
            input,
            "*filter\n:KUBESWIFT_SBX_DENY - [0:0]\n\
             -A KUBESWIFT_SBX_DENY -d 10.0.0.5/32 -j DROP\n\
             -A KUBESWIFT_SBX_DENY -d 203.0.113.0/24 -j DROP\n\
             -A KUBESWIFT_SBX_DENY -d 198.51.100.7/32 -j DROP\n\
             COMMIT\n"
        );
    }

    #[test]
    fn empty_list_empties_the_chain() {
        let (input, n) = restore_input("").unwrap();
        assert_eq!(n, 0);
        assert_eq!(input, "*filter\n:KUBESWIFT_SBX_DENY - [0:0]\nCOMMIT\n");
    }

    #[test]
    fn refuses_anything_but_an_ipv4_cidr() {
        for bad in [
            "10.0.0.1/33",
            "fd00::1",
            "10.0.0.1 -j ACCEPT",
            "-A FORWARD",
            "10.0.0.1/x",
            "host.example",
        ] {
            assert!(restore_input(bad).is_err(), "{:?} accepted", bad);
        }
    }
}

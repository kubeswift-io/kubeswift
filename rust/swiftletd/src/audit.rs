//! The egress audit of a proxied sandbox, for the unprivileged `egress-audit`
//! container (`kubectl logs <pod> -c egress-audit`).
//!
//! The controller gives a proxied launcher an `egress-audit` volume
//! ([`DIR_ENV`]), shared read-only with a container that copies `audit.log`
//! to its stdout, as the console does (console.rs). Every line is one JSON
//! object with `time`, `kind` and `event`:
//!
//! - `kind: dns`: the guest's DNS queries, from the launcher's dnsmasq
//!   (`query`, `reply`, `blocked` for a name outside the allowlist), through
//!   `swiftletd egress-audit-dns`, which the entrypoint pipes dnsmasq's log
//!   into;
//! - `kind: connection`: the built-in broker's decisions (`allow`, `deny`,
//!   `connect-failed`), from its log;
//! - `kind: credentials`: deliveries of egress credentials (names, hosts and
//!   certificate serials; never a value).
//!
//! The file is bounded ([`MAX_BYTES_ENV`], default 16 MiB): past the bound one
//! `truncated` line is written and the rest dropped, so a guest that resolves
//! names without end cannot fill the node's disk. Writers append whole lines
//! (O_APPEND, one write each), so the entrypoint's DNS pipe and this process
//! can share the file. Every line still goes to the launcher's own log too.
use std::fs::OpenOptions;
use std::io::{BufRead, Write};
use std::path::PathBuf;

/// Environment variable naming the audit volume's mount point.
pub const DIR_ENV: &str = "KUBESWIFT_EGRESS_AUDIT_DIR";
/// Environment variable bounding audit.log, in bytes.
pub const MAX_BYTES_ENV: &str = "KUBESWIFT_EGRESS_AUDIT_MAX_BYTES";
const DEFAULT_MAX_BYTES: u64 = 16 << 20;
/// The audit file, in the volume.
pub const FILE: &str = "audit.log";
/// The subcommand the entrypoint pipes dnsmasq's log into.
pub const DNS_SUBCOMMAND: &str = "egress-audit-dns";

fn dir() -> Option<PathBuf> {
    std::env::var_os(DIR_ENV)
        .map(PathBuf::from)
        .filter(|p| !p.as_os_str().is_empty())
}

fn max_bytes() -> u64 {
    std::env::var(MAX_BYTES_ENV)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(DEFAULT_MAX_BYTES)
}

/// Holds the audit container's FIFO open for this process's life (see
/// console::hold_alive). No-op without the volume.
pub fn hold_alive() {
    if let Some(d) = dir() {
        if let Err(e) = crate::console::hold_alive_in(&d) {
            log::warn!("egress_audit_alive error={}", e);
        }
    }
}

/// Appends one event. Fields are merged after time, kind and event.
pub fn record(kind: &str, event: &str, fields: serde_json::Value) {
    let Some(d) = dir() else { return };
    let mut obj = serde_json::Map::new();
    obj.insert(
        "time".into(),
        chrono::Utc::now()
            .to_rfc3339_opts(chrono::SecondsFormat::Millis, true)
            .into(),
    );
    obj.insert("kind".into(), kind.into());
    obj.insert("event".into(), event.into());
    if let serde_json::Value::Object(m) = fields {
        obj.extend(m);
    }
    append(
        &d.join(FILE),
        max_bytes(),
        &serde_json::Value::Object(obj).to_string(),
    );
}

/// Appends a line unless the file is past max, in which case one
/// `truncated` line marks the end.
fn append(path: &std::path::Path, max: u64, line: &str) {
    let len = std::fs::metadata(path).map(|m| m.len()).unwrap_or(0);
    let marker = r#"{"kind":"audit","event":"truncated","reason":"the audit log reached its size bound; later events are only in the launcher log"}"#;
    let text = if len + (line.len() as u64) < max {
        line.to_string()
    } else if len < max + marker.len() as u64 + 1 && !ends_truncated(path) {
        marker.to_string()
    } else {
        return;
    };
    match OpenOptions::new().create(true).append(true).open(path) {
        Ok(mut f) => {
            let _ = f.write_all(format!("{}\n", text).as_bytes());
        }
        Err(e) => log::warn!("egress_audit_write path={} error={}", path.display(), e),
    }
}

fn ends_truncated(path: &std::path::Path) -> bool {
    use std::io::{Read, Seek, SeekFrom};
    let Ok(mut f) = std::fs::File::open(path) else {
        return false;
    };
    let mut tail = [0u8; 64];
    let len = f.metadata().map(|m| m.len()).unwrap_or(0);
    let n = tail.len().min(len as usize);
    if f.seek(SeekFrom::End(-(n as i64))).is_err() || f.read_exact(&mut tail[..n]).is_err() {
        return false;
    }
    String::from_utf8_lossy(&tail[..n]).contains("later events are only in the launcher log")
}

/// A dnsmasq log line as an audit event: (event, fields), or None for lines
/// that are not about a guest query (DHCP, startup).
pub fn parse_dnsmasq(line: &str) -> Option<(&'static str, serde_json::Value)> {
    // "dnsmasq[7]: query[A] example.com from 192.168.99.10"
    let (prefix, msg) = line.split_once(": ")?;
    let prefix = prefix.trim_start();
    if prefix != "dnsmasq" && !prefix.starts_with("dnsmasq[") {
        return None;
    }
    let w: Vec<&str> = msg.split_whitespace().collect();
    match w.as_slice() {
        [q, name, "from", from] if q.starts_with("query[") => {
            let ty = q.trim_start_matches("query[").trim_end_matches(']');
            Some((
                "query",
                serde_json::json!({"type": ty, "name": name, "from": from}),
            ))
        }
        // --address=/#/ answers a name outside the allowlist itself.
        ["config", name, "is", "NXDOMAIN"] | ["config", name, "is", "NODATA"] => {
            Some(("blocked", serde_json::json!({"name": name})))
        }
        [src, name, "is", answer] if *src == "reply" || *src == "cached" => {
            Some(("reply", serde_json::json!({"name": name, "answer": answer})))
        }
        _ => None,
    }
}

/// `swiftletd egress-audit-dns`: copies stdin (dnsmasq's log) to stderr (the
/// launcher log, as before) and records the guest's queries.
pub fn dns_main() -> ! {
    let stdin = std::io::stdin();
    let mut err = std::io::stderr();
    for line in stdin.lock().lines() {
        let Ok(line) = line else { break };
        let _ = writeln!(err, "{}", line);
        if let Some((event, fields)) = parse_dnsmasq(&line) {
            record("dns", event, fields);
        }
    }
    std::process::exit(0)
}

/// A built-in broker log line as an audit event, or None.
pub fn parse_broker(line: &str) -> Option<(&'static str, serde_json::Value)> {
    // "[... INFO  swiftletd::egress_broker] egress_broker_allow peer=a dst=b host=x name=y"
    let i = line.find("egress_broker_")?;
    let rest = &line[i + "egress_broker_".len()..];
    let (verb, tail) = rest.split_once(' ').unwrap_or((rest, ""));
    let event = match verb {
        "allow" => "allow",
        "deny" => "deny",
        "connect_failed" => "connect-failed",
        _ => return None,
    };
    let mut m = serde_json::Map::new();
    for tok in tail.split_whitespace() {
        match tok.split_once('=') {
            Some((k, v)) => {
                m.insert(k.to_string(), v.into());
            }
            // "address": allowed by a cidr or service rule.
            None => {
                m.insert("match".to_string(), tok.into());
            }
        }
    }
    Some((event, serde_json::Value::Object(m)))
}

/// Copies the broker's log to this process's stderr and records its
/// decisions. Runs on its own thread until the pipe closes.
pub fn follow_broker(stderr: std::process::ChildStderr) {
    let mut out = std::io::stderr();
    for line in std::io::BufReader::new(stderr).lines() {
        let Ok(line) = line else { break };
        let _ = writeln!(out, "{}", line);
        if let Some((event, fields)) = parse_broker(&line) {
            record("connection", event, fields);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_dnsmasq_queries() {
        let (e, f) = parse_dnsmasq("dnsmasq[7]: query[A] example.com from 192.168.99.10").unwrap();
        assert_eq!(e, "query");
        assert_eq!(
            f,
            serde_json::json!({"type": "A", "name": "example.com", "from": "192.168.99.10"})
        );
        assert_eq!(
            parse_dnsmasq("dnsmasq[7]: config evil.test is NXDOMAIN")
                .unwrap()
                .0,
            "blocked"
        );
        let (e, f) = parse_dnsmasq("dnsmasq[7]: reply example.com is 93.184.215.14").unwrap();
        assert_eq!((e, f["answer"].as_str()), ("reply", Some("93.184.215.14")));
        assert!(parse_dnsmasq("dnsmasq-dhcp[7]: DHCPACK(br0) 192.168.99.10 aa:bb").is_none());
        assert!(parse_dnsmasq("dnsmasq-dhcp: DHCPACK(br0) 192.168.99.10 aa:bb").is_none());
        assert_eq!(
            parse_dnsmasq("dnsmasq: query[AAAA] example.com from 192.168.99.10")
                .unwrap()
                .0,
            "query"
        );
        assert!(parse_dnsmasq("dnsmasq[7]: started, version 2.89").is_none());
        assert!(parse_dnsmasq("dnsmasq[7]: forwarded example.com to 10.96.0.10").is_none());
    }

    #[test]
    fn parses_broker_decisions() {
        let (e, f) = parse_broker("[2026-11-17T09:00:00Z INFO  swiftletd::egress_broker] egress_broker_allow peer=192.168.99.10:4000 dst=93.184.215.14:443 host=example.com name=example.com").unwrap();
        assert_eq!(e, "allow");
        assert_eq!(f["dst"], "93.184.215.14:443");
        assert_eq!(f["host"], "example.com");
        let (e, f) =
            parse_broker("egress_broker_allow peer=a:1 dst=10.96.0.12:8000 address").unwrap();
        assert_eq!((e, f["match"].as_str()), ("allow", Some("address")));
        let (e, f) = parse_broker(
            "egress_broker_deny peer=a:1 dst=1.2.3.4:443 reason=no-rule name=evil.test",
        )
        .unwrap();
        assert_eq!(
            (e, f["reason"].as_str(), f["name"].as_str()),
            ("deny", Some("no-rule"), Some("evil.test"))
        );
        assert_eq!(
            parse_broker("egress_broker_connect_failed peer=a dst=b address error=timeout")
                .unwrap()
                .0,
            "connect-failed"
        );
        assert!(parse_broker("egress_broker_listening port=15001").is_none());
        assert!(parse_broker("something else").is_none());
    }

    #[test]
    fn bounds_the_file_with_one_marker() {
        let d = tempfile::tempdir().unwrap();
        let p = d.path().join(FILE);
        for i in 0..50 {
            append(&p, 200, &format!(r#"{{"n":{}}}"#, i));
        }
        let text = std::fs::read_to_string(&p).unwrap();
        assert!(text.lines().count() < 50);
        assert_eq!(text.matches("truncated").count(), 1, "{}", text);
        assert!(text.trim_end().ends_with("launcher log\"}"));
        for l in text.lines() {
            serde_json::from_str::<serde_json::Value>(l).unwrap();
        }
    }
}

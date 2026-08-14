//! DHCP lease file polling and pod annotation for guest IP discovery.

use kube::api::{Api, Patch, PatchParams};
use kube::Client;
use serde_json::json;
use std::path::Path;
use std::thread;
use std::time::Duration;

pub const ANNOTATION_GUEST_IP: &str = "kubeswift.io/guest-ip";
pub const ANNOTATION_GUEST_INTERFACES: &str = "kubeswift.io/guest-interfaces";
/// Written when the lease poller gives up without ever seeing a DHCP lease
/// (#527). The controller maps it to a NetworkReady=False condition.
///
/// This exists because the timeout used to be a bare `log::warn!` and nothing
/// else. The guest reached Running with GuestRunning=True and simply never
/// reported an IP — no condition, no event, nothing on the CR. A real case cost
/// hours: an Ubuntu disk-boot guest with no `seedProfileRef` gets no NoCloud
/// seed, so cloud-init never writes netplan, the NIC is never configured, and no
/// DHCP request is ever sent. Everything looked healthy. Design principle 6 says
/// status fields reflect real state; a launcher-log warning is not status.
pub const ANNOTATION_NETWORK_UNREADY: &str = "kubeswift.io/guest-network-unready";
/// Egress reachability: "true"/"false" written by the launcher entrypoint's
/// cluster-DNS-ClusterIP probe (service exposure §4 — egress observability).
/// The controller maps it to status.network.egress + the EgressReady condition.
pub const ANNOTATION_EGRESS: &str = "kubeswift.io/egress-cluster-reachable";

/// read_egress_marker reads EGRESS_CLUSTER_REACHABLE=true|false from the
/// `egress.env` file the launcher entrypoint writes next to the lease file
/// (same per-guest run dir). Returns None when absent/unparseable (the probe
/// did not run, e.g. no network), leaving the controller's egress status unset.
fn read_egress_marker(lease_path: &Path) -> Option<String> {
    let dir = lease_path.parent()?;
    let contents = std::fs::read_to_string(dir.join("egress.env")).ok()?;
    for line in contents.lines() {
        if let Some(v) = line.trim().strip_prefix("EGRESS_CLUSTER_REACHABLE=") {
            let v = v.trim();
            if v == "true" || v == "false" {
                return Some(v.to_string());
            }
        }
    }
    None
}

/// dnsmasq lease file format: timestamp mac ip hostname client_id (space-separated).
/// Returns the first IP found, or None if no valid lease.
fn parse_first_lease(contents: &str) -> Option<String> {
    for line in contents.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let parts: Vec<&str> = line.split_whitespace().collect();
        if parts.len() >= 3 {
            let ip = parts[2];
            if ip.parse::<std::net::IpAddr>().is_ok() {
                return Some(ip.to_string());
            }
        }
    }
    None
}

/// Spawns a background thread that polls the lease file and patches the pod annotation when IP found.
/// Lease-poll attempt caps (each attempt is `INTERVAL` = 2s apart).
///
/// `DEFAULT` (~4 min) covers a fresh boot: cloud images DHCP within ~60-90s.
///
/// `RESTORE` keeps the poller alive for the pod's lifetime. A cloneFromSnapshot
/// guest (and any CH `--restore` receiver) RESUMES the source's captured RAM —
/// including its already-configured `eth0` lease — so it does NOT re-run DHCP on
/// start. dnsmasq writes no lease, so a fresh clone has no IP to discover. The
/// guest only DHCPs on its FIRST REBOOT (which regenerates identity too), and an
/// operator may reboot it many minutes after it reaches Running. With the
/// `DEFAULT` cap the poller would have exited (`lease_poll_timeout`) long before
/// then, so the post-reboot DHCP would land into a dead poller and the IP would
/// never reach `status.network.primaryIP` (demo-06 finding, 2026-06-15). The
/// poller still terminates immediately on the first successful patch, so an
/// unbounded cap only means "wait as long as the pod lives" for a guest that has
/// not yet acquired an IP.
pub const LEASE_POLL_ATTEMPTS_DEFAULT: u32 = 120;
pub const LEASE_POLL_ATTEMPTS_RESTORE: u32 = u32::MAX;

/// Stops after a SUCCESSFUL patch or after `max_attempts` (× 2s interval —
/// pass [`LEASE_POLL_ATTEMPTS_DEFAULT`] for fresh boots,
/// [`LEASE_POLL_ATTEMPTS_RESTORE`] for CH `--restore` clone/receiver guests).
/// `nics` is passed to build the multi-NIC interfaces annotation.
///
/// Retry-on-failure invariant (added 2026-04-29 — Phase 2 walkthrough
/// finding W4): the prior implementation `return`-ed unconditionally
/// after the first patch attempt regardless of result. When the
/// per-namespace RBAC was missing (Phase 2 walkthrough finding W3 —
/// fixed by `internal/controller/swiftguest/rbac.go`), the patch
/// returned 403 Forbidden and the poller exited; even after the
/// RBAC was applied later in the pod's lifetime, the annotation was
/// never written, leaving `status.network.primaryIP` empty forever.
///
/// The fix: only `return` (terminate the poller) on a SUCCESSFUL
/// patch. On any error from the kube client (transient apiserver
/// unavailability, RBAC gap, etc.), continue polling — eventually
/// the operator-fix will land and the next attempt will succeed.
pub fn spawn_lease_poller(
    lease_path: impl AsRef<Path> + Send + 'static,
    namespace: String,
    pod_name: String,
    nics: Option<Vec<crate::intent::NICIntent>>,
    max_attempts: u32,
) {
    let path = lease_path.as_ref().to_path_buf();
    thread::spawn(move || {
        const INTERVAL: Duration = Duration::from_secs(2);

        for attempt in 0..max_attempts {
            if attempt > 0 {
                thread::sleep(INTERVAL);
            }
            let contents = match std::fs::read_to_string(&path) {
                Ok(c) => c,
                Err(_) => continue,
            };
            let ip = match parse_first_lease(&contents) {
                Some(ip) => ip,
                None => continue,
            };
            log::info!("guest_ip_discovered ip={}", ip);

            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build();
            let Ok(rt) = rt else {
                log::error!("failed to create runtime for pod patch");
                return;
            };
            let nics_ref = nics.as_deref();
            let egress = read_egress_marker(&path);
            // patched is true iff patch_pod_annotation returned Ok.
            // We continue polling on transient errors (kube-client
            // unavailable, 403 RBAC gap during initial namespace
            // setup, etc.) — see W4 finding above. Only a successful
            // patch terminates the poller.
            let patched = rt.block_on(async {
                let client = match crate::kube_client::create_client().await {
                    Ok(c) => c,
                    Err(e) => {
                        log::warn!("kube client unavailable ({}), will retry", e);
                        return false;
                    }
                };
                match patch_pod_annotation(
                    &client,
                    &namespace,
                    &pod_name,
                    &ip,
                    nics_ref,
                    egress.as_deref(),
                )
                .await
                {
                    Err(e) => {
                        log::warn!("patch_pod_annotation_failed (will retry): {}", e);
                        false
                    }
                    Ok(()) => {
                        log::info!("pod_annotation_patched {}={}", ANNOTATION_GUEST_IP, ip);
                        true
                    }
                }
            });
            if patched {
                return;
            }
            // else: continue polling; transient error or RBAC gap.
        }
        let waited_secs = (max_attempts as u64).saturating_mul(INTERVAL.as_secs());
        log::warn!("lease_poll_timeout after {}s", waited_secs);
        report_network_unready(&namespace, &pod_name, waited_secs);
    });
}

/// Patches the network-unready annotation so the timeout reaches the CR (#527).
///
/// Retried, for the same reason spawn_lease_poller retries its IP patch (W4): a
/// 403 during namespace RBAC setup or a momentarily unavailable apiserver must
/// not swallow the report. Failing to report a silent failure silently would be
/// the same bug one level up — so a give-up here logs at ERROR.
///
/// Bounded, unlike the IP poller: this runs once at the end of the poll window
/// and there is nothing further to wait for.
fn report_network_unready(namespace: &str, pod_name: &str, waited_secs: u64) {
    const ATTEMPTS: u32 = 5;
    const BACKOFF: Duration = Duration::from_secs(2);

    let Ok(rt) = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    else {
        log::error!("failed to create runtime for network-unready patch");
        return;
    };
    for attempt in 0..ATTEMPTS {
        if attempt > 0 {
            thread::sleep(BACKOFF);
        }
        let ok = rt.block_on(async {
            let client = match crate::kube_client::create_client().await {
                Ok(c) => c,
                Err(e) => {
                    log::warn!("kube client unavailable for network-unready patch ({e})");
                    return false;
                }
            };
            match patch_network_unready(&client, namespace, pod_name, waited_secs).await {
                Ok(()) => true,
                Err(e) => {
                    log::warn!("network_unready_patch_failed (will retry): {e}");
                    false
                }
            }
        });
        if ok {
            log::info!("pod_annotation_patched {ANNOTATION_NETWORK_UNREADY}=DHCPTimeout");
            return;
        }
    }
    log::error!(
        "could not report the DHCP timeout to the pod after {ATTEMPTS} attempts; \
         the guest will show as Running with no IP and no reason"
    );
}

/// Build the interfaces JSON for pod annotation.
/// Includes the primary NIC with its discovered IP, plus any secondary NICs
/// with their MACs (IPs not discoverable via dnsmasq for secondary NICs).
fn build_interfaces_json(ip: &str, nics: Option<&[crate::intent::NICIntent]>) -> String {
    match nics {
        Some(nics) if !nics.is_empty() => {
            let entries: Vec<serde_json::Value> = nics
                .iter()
                .map(|n| {
                    if n.primary {
                        json!({"name": n.name, "mac": n.mac, "ip": ip})
                    } else if n.is_sriov() {
                        // SR-IOV: no MAC from controller, no DHCP IP discovery.
                        json!({"name": n.name})
                    } else {
                        json!({"name": n.name, "mac": n.mac})
                    }
                })
                .collect();
            serde_json::to_string(&entries).unwrap_or_default()
        }
        _ => {
            // Legacy single-NIC mode.
            serde_json::to_string(&json!([{"name": "eth0", "ip": ip}])).unwrap_or_default()
        }
    }
}

async fn patch_pod_annotation(
    client: &Client,
    namespace: &str,
    name: &str,
    ip: &str,
    nics: Option<&[crate::intent::NICIntent]>,
    egress: Option<&str>,
) -> Result<(), kube::Error> {
    let interfaces_json = build_interfaces_json(ip, nics);

    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let mut annotations = std::collections::BTreeMap::new();
    annotations.insert(ANNOTATION_GUEST_IP.to_string(), ip.to_string());
    annotations.insert(ANNOTATION_GUEST_INTERFACES.to_string(), interfaces_json);
    if let Some(e) = egress {
        annotations.insert(ANNOTATION_EGRESS.to_string(), e.to_string());
    }
    let patch = json!({
        "metadata": {
            "annotations": annotations
        }
    });
    let pp = PatchParams::default();
    api.patch(name, &pp, &Patch::Merge(&patch)).await?;
    Ok(())
}

/// Builds the network-unready annotation value.
///
/// JSON rather than a bare string so the controller can surface the wait
/// duration in its condition message without the operator having to know what
/// the poll window is. `serde_json::json!`, not `format!` — the repo convention
/// for annotation payloads.
fn network_unready_value(waited_secs: u64) -> String {
    serde_json::to_string(&json!({
        "reason": "DHCPTimeout",
        "afterSeconds": waited_secs,
    }))
    .unwrap_or_else(|_| r#"{"reason":"DHCPTimeout"}"#.to_string())
}

async fn patch_network_unready(
    client: &Client,
    namespace: &str,
    name: &str,
    waited_secs: u64,
) -> Result<(), kube::Error> {
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let patch = json!({
        "metadata": {
            "annotations": {
                ANNOTATION_NETWORK_UNREADY: network_unready_value(waited_secs),
            }
        }
    });
    api.patch(name, &PatchParams::default(), &Patch::Merge(&patch))
        .await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    // The controller parses this payload to build its NetworkReady message
    // (#527). A change here that the Go side does not expect silently degrades
    // the message back to a bare "no DHCP lease" — which is the dead end this
    // whole change exists to remove.
    #[test]
    fn network_unready_value_carries_reason_and_wait() {
        let v = network_unready_value(240);
        let parsed: serde_json::Value = serde_json::from_str(&v).expect("must be valid JSON");
        assert_eq!(
            parsed["reason"], "DHCPTimeout",
            "controller matches on reason"
        );
        assert_eq!(
            parsed["afterSeconds"], 240,
            "the wait duration is what makes the message actionable"
        );
    }

    #[test]
    fn parse_first_lease_skips_blank_and_comment_lines() {
        // dnsmasq lease file format:
        //   <expiry> <mac> <ip> <hostname> <client_id>
        // We accept blank/comment lines and pick the first valid IP.
        let contents = "\
# header comment
\n
1777501581 2e:dc:8f:5b:97:21 192.168.99.15 mig-walkthrough-guest ff:b5:5e:67:ff:00
";
        assert_eq!(
            parse_first_lease(contents),
            Some("192.168.99.15".to_string())
        );
    }

    #[test]
    fn parse_first_lease_returns_none_on_no_lease() {
        assert_eq!(parse_first_lease(""), None);
        assert_eq!(parse_first_lease("# only comments\n"), None);
    }

    #[test]
    fn parse_first_lease_skips_non_ip_third_column() {
        // If the third column isn't a valid IP literal, skip the
        // line. dnsmasq sometimes writes intermediate "DUID" lines
        // alongside lease lines; those should not be misread as IPs.
        let contents = "1777501581 mac garbage hostname client_id\n";
        assert_eq!(parse_first_lease(contents), None);
    }

    #[test]
    fn build_interfaces_json_legacy_single_nic() {
        // No NIC list → emit the legacy single-NIC entry shape.
        let s = build_interfaces_json("10.0.0.5", None);
        let v: serde_json::Value = serde_json::from_str(&s).unwrap();
        assert_eq!(v[0]["name"], "eth0");
        assert_eq!(v[0]["ip"], "10.0.0.5");
    }

    // The retry-on-failure invariant inside `spawn_lease_poller` (W4
    // finding from the Phase 2 walkthrough) is verified end-to-end on
    // the cluster: re-running the walkthrough after the RBAC bootstrap
    // fix applies should observe the lease annotation appearing on
    // the pod within ~30s of guest boot, not "never". A unit test
    // would require a mock kube client + tokio runtime + thread
    // synchronisation harness — disproportionate scaffolding for the
    // bug surface area. The structural fix (`if patched { return; }`
    // gate) is small enough for a code-review-bound contract.
}

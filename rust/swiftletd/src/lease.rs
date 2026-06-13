//! DHCP lease file polling and pod annotation for guest IP discovery.

use kube::api::{Api, Patch, PatchParams};
use kube::Client;
use serde_json::json;
use std::path::Path;
use std::thread;
use std::time::Duration;

pub const ANNOTATION_GUEST_IP: &str = "kubeswift.io/guest-ip";
pub const ANNOTATION_GUEST_INTERFACES: &str = "kubeswift.io/guest-interfaces";
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
/// Stops after a SUCCESSFUL patch or after max_attempts (default 120 = 4 min at 2s interval).
/// First boot of cloud images can take 60–90s for cloud-init + DHCP.
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
/// Bounded by MAX_ATTEMPTS (~4 min total).
pub fn spawn_lease_poller(
    lease_path: impl AsRef<Path> + Send + 'static,
    namespace: String,
    pod_name: String,
    nics: Option<Vec<crate::intent::NICIntent>>,
) {
    let path = lease_path.as_ref().to_path_buf();
    thread::spawn(move || {
        const INTERVAL: Duration = Duration::from_secs(2);
        const MAX_ATTEMPTS: u32 = 120;

        for attempt in 0..MAX_ATTEMPTS {
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
        log::warn!("lease_poll_timeout");
    });
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

#[cfg(test)]
mod tests {
    use super::*;

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

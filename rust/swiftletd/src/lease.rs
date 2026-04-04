//! DHCP lease file polling and pod annotation for guest IP discovery.

use kube::api::{Api, Patch, PatchParams};
use kube::Client;
use serde_json::json;
use std::path::Path;
use std::thread;
use std::time::Duration;

pub const ANNOTATION_GUEST_IP: &str = "kubeswift.io/guest-ip";
pub const ANNOTATION_GUEST_INTERFACES: &str = "kubeswift.io/guest-interfaces";

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
/// Stops after patching or after max_attempts (default 120 = 4 min at 2s interval).
/// First boot of cloud images can take 60–90s for cloud-init + DHCP.
/// `nics` is passed to build the multi-NIC interfaces annotation.
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
            rt.block_on(async {
                let client = match crate::kube_client::create_client().await {
                    Ok(c) => c,
                    Err(e) => {
                        log::warn!("kube client unavailable ({}), skipping pod annotation", e);
                        return;
                    }
                };
                if let Err(e) =
                    patch_pod_annotation(&client, &namespace, &pod_name, &ip, nics_ref).await
                {
                    log::error!("patch_pod_annotation_failed: {}", e);
                } else {
                    log::info!("pod_annotation_patched {}={}", ANNOTATION_GUEST_IP, ip);
                }
            });
            return;
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
) -> Result<(), kube::Error> {
    let interfaces_json = build_interfaces_json(ip, nics);

    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let mut annotations = std::collections::BTreeMap::new();
    annotations.insert(ANNOTATION_GUEST_IP.to_string(), ip.to_string());
    annotations.insert(ANNOTATION_GUEST_INTERFACES.to_string(), interfaces_json);
    let patch = json!({
        "metadata": {
            "annotations": annotations
        }
    });
    let pp = PatchParams::default();
    api.patch(name, &pp, &Patch::Merge(&patch)).await?;
    Ok(())
}

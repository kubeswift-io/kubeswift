//! DHCP lease file polling and pod annotation for guest IP discovery.

use kube::api::{Api, Patch, PatchParams};
use kube::Client;
use serde_json::json;
use std::path::Path;
use std::thread;
use std::time::Duration;

pub const ANNOTATION_GUEST_IP: &str = "kubeswift.io/guest-ip";

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
/// Stops after patching or after max_attempts (default 60 = 2 min at 2s interval).
pub fn spawn_lease_poller(
    lease_path: impl AsRef<Path> + Send + 'static,
    namespace: String,
    pod_name: String,
) {
    let path = lease_path.as_ref().to_path_buf();
    thread::spawn(move || {
        const INTERVAL: Duration = Duration::from_secs(2);
        const MAX_ATTEMPTS: u32 = 60;

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
            eprintln!("swiftletd: discovered guest IP {} from lease file", ip);

            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build();
            let Ok(rt) = rt else {
                eprintln!("swiftletd: failed to create runtime for pod patch");
                return;
            };
            rt.block_on(async {
                let client = match kube::Client::try_default().await {
                    Ok(c) => c,
                    Err(e) => {
                        eprintln!(
                            "swiftletd: kube client unavailable ({}), skipping pod annotation",
                            e
                        );
                        return;
                    }
                };
                if let Err(e) = patch_pod_annotation(&client, &namespace, &pod_name, &ip).await {
                    eprintln!("swiftletd: failed to patch pod annotation: {}", e);
                } else {
                    eprintln!(
                        "swiftletd: patched pod annotation {}={}",
                        ANNOTATION_GUEST_IP, ip
                    );
                }
            });
            return;
        }
        eprintln!("swiftletd: lease poll timeout, no IP discovered");
    });
}

async fn patch_pod_annotation(
    client: &Client,
    namespace: &str,
    name: &str,
    ip: &str,
) -> Result<(), kube::Error> {
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let mut annotations = std::collections::BTreeMap::new();
    annotations.insert(ANNOTATION_GUEST_IP.to_string(), ip.to_string());
    let patch = json!({
        "metadata": {
            "annotations": annotations
        }
    });
    let pp = PatchParams::default();
    api.patch(name, &pp, &Patch::Merge(&patch)).await?;
    Ok(())
}

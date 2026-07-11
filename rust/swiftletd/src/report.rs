//! Report VM state to the control plane via SwiftGuest status patch.
//!
//! Uses kube-rs to patch SwiftGuest status with GuestRunning condition.
//! Requires in-cluster config (service account) and RBAC for patch swiftguests/status.

use kube::api::{Api, Patch, PatchParams};
use kube::core::discovery::ApiResource;
use kube::core::gvk::GroupVersionKind;
use kube::core::DynamicObject;
use kube::Client;
use serde_json::json;

const CONDITION_GUEST_RUNNING: &str = "GuestRunning";

/// Reports GuestRunning condition to SwiftGuest status.
/// namespace and name from pod (pod name = guest name when running in pod).
pub async fn report_guest_running(
    client: &Client,
    namespace: &str,
    name: &str,
    running: bool,
    reason: Option<&str>,
) -> Result<(), kube::Error> {
    let gvk = GroupVersionKind::gvk("swift.kubeswift.io", "v1alpha1", "SwiftGuest");
    let api_resource = ApiResource::from_gvk_with_plural(&gvk, "swiftguests");
    let api: Api<DynamicObject> = Api::namespaced_with(client.clone(), namespace, &api_resource);

    let now = chrono::Utc::now().to_rfc3339();
    let (status_val, reason_val, message) = if running {
        ("True", "VmRunning", "VM is running")
    } else {
        (
            "False",
            reason.unwrap_or("VmStopped"),
            reason.unwrap_or("VM stopped or failed"),
        )
    };

    let status = json!({
        "status": {
            "conditions": [{
                "type": CONDITION_GUEST_RUNNING,
                "status": status_val,
                "reason": reason_val,
                "message": message,
                "lastTransitionTime": now
            }]
        }
    });

    let pp = PatchParams::default();
    api.patch_status(name, &pp, &Patch::Merge(status)).await?;
    Ok(())
}

/// Reports runtime and console to the launcher pod annotations.
/// The controller maps these annotations to SwiftGuest status.
pub async fn report_guest_runtime(
    client: &Client,
    namespace: &str,
    name: &str,
    pid: u32,
    serial_socket: &str,
    hypervisor: &str,
) -> Result<(), kube::Error> {
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let mut annotations = std::collections::BTreeMap::new();
    annotations.insert(
        "kubeswift.io/guest-runtime-pid".to_string(),
        pid.to_string(),
    );
    annotations.insert(
        "kubeswift.io/guest-serial-socket".to_string(),
        serial_socket.to_string(),
    );
    annotations.insert(
        "kubeswift.io/guest-hypervisor".to_string(),
        hypervisor.to_string(),
    );
    let patch = json!({
        "metadata": {
            "annotations": annotations
        }
    });
    let pp = PatchParams::default();
    api.patch(name, &pp, &Patch::Merge(&patch)).await?;
    Ok(())
}

/// Whether swiftletd should patch a SwiftGuest CR's GuestRunning condition
/// (`report_guest_running`). Default true — the SwiftGuest launch path is
/// unchanged. A SwiftSandbox launcher sets `KUBESWIFT_REPORT_GUEST_CR=false`:
/// there is no SwiftGuest CR named after the pod (the SwiftSandbox controller
/// owns status, derived from the pod annotations), so the patch would 404 on
/// every launch. This only gates the CR patch — `report_guest_runtime` (pod
/// annotations) and the lease poller are unaffected.
pub fn report_guest_cr_enabled(v: Option<&str>) -> bool {
    match v {
        Some(s) => !matches!(
            s.trim().to_ascii_lowercase().as_str(),
            "false" | "off" | "0" | "no"
        ),
        None => true,
    }
}

#[cfg(test)]
mod tests {
    use super::report_guest_cr_enabled;

    #[test]
    fn cr_report_defaults_on() {
        assert!(report_guest_cr_enabled(None));
        assert!(report_guest_cr_enabled(Some("true")));
        assert!(report_guest_cr_enabled(Some("1")));
        // An empty value is not a disable token -> on (only explicit tokens disable).
        assert!(report_guest_cr_enabled(Some("")));
    }

    #[test]
    fn cr_report_off_tokens() {
        for v in ["false", "off", "0", "no", "False", "OFF", " false "] {
            assert!(!report_guest_cr_enabled(Some(v)), "{v} should disable");
        }
    }
}

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

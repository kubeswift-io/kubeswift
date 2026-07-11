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

/// Writes the sandbox workload's exit code to a launcher pod annotation, for the
/// SwiftSandbox controller to map to `status.exitCode` + a terminal phase. The
/// bridge-init emits a trailing `KUBESWIFT-EXIT-CODE=<n>` on the console after the
/// workload exits; swiftletd extracts it from the sandbox console file once CH exits.
pub async fn report_sandbox_exit(
    client: &Client,
    namespace: &str,
    name: &str,
    exit_code: i32,
) -> Result<(), kube::Error> {
    let api: Api<k8s_openapi::api::core::v1::Pod> = Api::namespaced(client.clone(), namespace);
    let patch = json!({
        "metadata": {
            "annotations": {
                "kubeswift.io/sandbox-exit-code": exit_code.to_string(),
            }
        }
    });
    let pp = PatchParams::default();
    api.patch(name, &pp, &Patch::Merge(&patch)).await?;
    Ok(())
}

/// Parses the LAST `KUBESWIFT-EXIT-CODE=<n>` line from the sandbox console text. The
/// bridge-init emits it once, after the workload exits, so it is the workload's exit
/// code. Taking the LAST match is robust against a workload that printed a look-alike
/// line before exiting (all of its output precedes the bridge's line).
pub fn parse_sandbox_exit_code(console: &str) -> Option<i32> {
    console
        .lines()
        .rev()
        .find_map(|l| l.trim().strip_prefix("KUBESWIFT-EXIT-CODE="))
        .and_then(|v| v.trim().parse::<i32>().ok())
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
    use super::{parse_sandbox_exit_code, report_guest_cr_enabled};

    #[test]
    fn sandbox_exit_code_parsing() {
        assert_eq!(
            parse_sandbox_exit_code("hello\nKUBESWIFT-EXIT-CODE=0\n"),
            Some(0)
        );
        assert_eq!(
            parse_sandbox_exit_code("out\r\nKUBESWIFT-EXIT-CODE=7\r\n"),
            Some(7)
        );
        // Last match wins (workload printed a look-alike before the bridge's real line).
        assert_eq!(
            parse_sandbox_exit_code("KUBESWIFT-EXIT-CODE=99\nwork\nKUBESWIFT-EXIT-CODE=3\n"),
            Some(3)
        );
        assert_eq!(parse_sandbox_exit_code("no marker here\n"), None);
        assert_eq!(
            parse_sandbox_exit_code("KUBESWIFT-EXIT-CODE=notanint\n"),
            None
        );
    }

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

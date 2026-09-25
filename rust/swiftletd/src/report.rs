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

/// How many times to retry the GuestRunning write when the SwiftGuest changed
/// between our read and our patch (409 Conflict).
const GUEST_RUNNING_CONFLICT_RETRIES: usize = 5;

/// Reports GuestRunning condition to SwiftGuest status.
/// namespace and name from pod (pod name = guest name when running in pod).
///
/// `status.conditions` is an atomic list in the CRD schema, so a merge patch
/// replaces all of it: patching just `[GuestRunning]` wiped every other
/// condition the controller maintains (GPUAllocated, StorageReady, ...) and a
/// running GPU guest then read as Pending. Read the conditions, upsert
/// GuestRunning, and write the whole list back with the resourceVersion that
/// was read, so a concurrent controller write is retried rather than lost.
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

    let (status_val, reason_val, message) = if running {
        ("True", "VmRunning", "VM is running")
    } else {
        (
            "False",
            reason.unwrap_or("VmStopped"),
            reason.unwrap_or("VM stopped or failed"),
        )
    };

    let pp = PatchParams::default();
    let mut attempt = 0;
    loop {
        let obj = api.get_status(name).await?;
        let mut conditions = obj
            .data
            .get("status")
            .and_then(|s| s.get("conditions"))
            .and_then(|c| c.as_array())
            .cloned()
            .unwrap_or_default();
        upsert_condition(
            &mut conditions,
            CONDITION_GUEST_RUNNING,
            status_val,
            reason_val,
            message,
            &chrono::Utc::now().to_rfc3339(),
        );
        let patch = json!({
            "metadata": { "resourceVersion": obj.metadata.resource_version },
            "status": { "conditions": conditions }
        });
        match api.patch_status(name, &pp, &Patch::Merge(patch)).await {
            Ok(_) => return Ok(()),
            Err(kube::Error::Api(ae))
                if ae.code == 409 && attempt < GUEST_RUNNING_CONFLICT_RETRIES =>
            {
                attempt += 1;
            }
            Err(e) => return Err(e),
        }
    }
}

/// Set condition `ctype` in `conditions` (a status.conditions list), leaving
/// every other condition as it is. `lastTransitionTime` moves to `now` only
/// when the status changes (or the condition is new), as the field means.
pub fn upsert_condition(
    conditions: &mut Vec<serde_json::Value>,
    ctype: &str,
    status: &str,
    reason: &str,
    message: &str,
    now: &str,
) {
    let existing = conditions
        .iter()
        .position(|c| c.get("type").and_then(|t| t.as_str()) == Some(ctype));
    let transition = match existing {
        Some(i) if conditions[i].get("status").and_then(|s| s.as_str()) == Some(status) => {
            conditions[i]
                .get("lastTransitionTime")
                .and_then(|t| t.as_str())
                .unwrap_or(now)
                .to_string()
        }
        _ => now.to_string(),
    };
    let cond = json!({
        "type": ctype,
        "status": status,
        "reason": reason,
        "message": message,
        "lastTransitionTime": transition
    });
    match existing {
        Some(i) => conditions[i] = cond,
        None => conditions.push(cond),
    }
}

/// The SwiftGuest a launcher reports its GuestRunning condition to:
/// `KUBESWIFT_GUEST_NAME` when set, else the pod name. The two differ for a
/// live migration's destination pod, `<guest>-mig-<uid>`, which becomes the
/// guest's launcher; reporting by pod name patched a SwiftGuest that does
/// not exist, so a migrated guest that later stopped or failed never said
/// so. Launcher pods built before the controller set the variable keep the
/// pod name, which is the guest's for every pod but a migration's.
pub fn guest_name(env_guest: Option<String>, pod_name: Option<String>) -> Option<String> {
    env_guest.filter(|g| !g.is_empty()).or(pod_name)
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
/// How much of the end of a sandbox console log to read for the exit-code
/// marker. The bridge-init prints the marker just before powering off, so it is
/// in the last few lines; bounding the read keeps a workload that logged
/// gigabytes from making swiftletd load all of it into memory.
pub const SANDBOX_CONSOLE_TAIL_BYTES: u64 = 64 * 1024;

/// Read at most the last `max` bytes of a console log as text. Lossy on
/// purpose: a workload can print arbitrary bytes, and a strict UTF-8 read
/// (read_to_string) failed on the first non-UTF-8 one — which dropped the exit
/// code and let a failed workload be reported as exit 0.
pub fn read_console_tail(path: &str, max: u64) -> std::io::Result<String> {
    use std::io::{Read, Seek, SeekFrom};
    let mut f = std::fs::File::open(path)?;
    let len = f.metadata()?.len();
    if len > max {
        f.seek(SeekFrom::Start(len - max))?;
    }
    let mut buf = Vec::with_capacity(len.min(max) as usize);
    f.read_to_end(&mut buf)?;
    Ok(String::from_utf8_lossy(&buf).into_owned())
}

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
    use super::{
        guest_name, parse_sandbox_exit_code, read_console_tail, report_guest_cr_enabled,
        upsert_condition, SANDBOX_CONSOLE_TAIL_BYTES,
    };
    use serde_json::json;

    // A migration's destination pod is not named like its guest; the
    // controller names the guest in KUBESWIFT_GUEST_NAME.
    #[test]
    fn guest_name_prefers_the_guest_over_the_pod() {
        let s = |v: &str| Some(v.to_string());
        assert_eq!(guest_name(s("vm-a"), s("vm-a-mig-1db32d")), s("vm-a"));
        // Pods built before the variable existed, or an empty value.
        assert_eq!(guest_name(None, s("vm-a")), s("vm-a"));
        assert_eq!(guest_name(s(""), s("vm-a")), s("vm-a"));
        assert_eq!(guest_name(None, None), None);
    }

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

    // A workload that printed non-UTF-8 bytes must still have its exit code
    // recovered (read_to_string used to fail and the sandbox read as exit 0).
    #[test]
    fn console_tail_recovers_exit_code_despite_binary_output() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("serial.sock.log");
        let mut data = b"starting\n".to_vec();
        data.extend_from_slice(&[0xff, 0xfe, 0x00, 0xc3, 0x28, b'\n']); // invalid UTF-8
        data.extend_from_slice(b"KUBESWIFT-EXIT-CODE=3\nreboot: Power down\n");
        std::fs::write(&path, &data).unwrap();
        let text = read_console_tail(path.to_str().unwrap(), SANDBOX_CONSOLE_TAIL_BYTES).unwrap();
        assert_eq!(parse_sandbox_exit_code(&text), Some(3));
    }

    // Only the tail is read, so a huge log stays bounded, and the real marker
    // (printed last, after the workload exits) wins over one the workload
    // printed itself.
    #[test]
    fn console_tail_is_bounded_and_takes_the_last_marker() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("serial.sock.log");
        let mut data = b"KUBESWIFT-EXIT-CODE=0\n".to_vec(); // spoof from the workload
        data.extend(std::iter::repeat_n(b'x', 256 * 1024));
        data.extend_from_slice(b"\nKUBESWIFT-EXIT-CODE=1\n");
        std::fs::write(&path, &data).unwrap();
        let text = read_console_tail(path.to_str().unwrap(), SANDBOX_CONSOLE_TAIL_BYTES).unwrap();
        assert!(text.len() as u64 <= SANDBOX_CONSOLE_TAIL_BYTES);
        assert_eq!(parse_sandbox_exit_code(&text), Some(1));
    }

    // Writing GuestRunning must keep every other condition: the list is
    // atomic, and a patch of [GuestRunning] alone wiped GPUAllocated and the
    // rest.
    #[test]
    fn upsert_keeps_other_conditions_and_transition_time() {
        let mut conds = vec![
            json!({"type": "GPUAllocated", "status": "True", "reason": "Allocated",
                   "message": "1 GPU", "lastTransitionTime": "2026-01-01T00:00:00Z"}),
            json!({"type": "GuestRunning", "status": "True", "reason": "VmRunning",
                   "message": "VM is running", "lastTransitionTime": "2026-01-02T00:00:00Z"}),
        ];
        upsert_condition(
            &mut conds,
            "GuestRunning",
            "True",
            "VmRunning",
            "VM is running",
            "NOW",
        );
        assert_eq!(conds.len(), 2);
        assert_eq!(conds[0]["type"], "GPUAllocated");
        assert_eq!(
            conds[1]["lastTransitionTime"], "2026-01-02T00:00:00Z",
            "an unchanged status keeps its transition time"
        );

        upsert_condition(
            &mut conds,
            "GuestRunning",
            "False",
            "VmStopped",
            "stopped",
            "NOW",
        );
        assert_eq!(conds.len(), 2);
        assert_eq!(conds[1]["status"], "False");
        assert_eq!(conds[1]["lastTransitionTime"], "NOW");
        assert_eq!(conds[0]["reason"], "Allocated");
    }

    #[test]
    fn upsert_appends_a_new_condition() {
        let mut conds = vec![json!({"type": "StorageReady", "status": "True"})];
        upsert_condition(&mut conds, "GuestRunning", "True", "VmRunning", "up", "NOW");
        assert_eq!(conds.len(), 2);
        assert_eq!(conds[1]["type"], "GuestRunning");
        assert_eq!(conds[1]["lastTransitionTime"], "NOW");
    }
}

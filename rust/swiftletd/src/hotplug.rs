//! Hot-plugged volumes (SwiftVolumeAttachment): converge Cloud Hypervisor
//! onto the desired set the controller writes on this launcher's pod.
//!
//! The contract is internal/hotplug/protocol.go. The controller writes
//! `kubeswift.io/hotplug-volumes`, a JSON array of volumes; this module plugs
//! in what is in it and unplugs what is not, and reports each volume's state
//! in `kubeswift.io/hotplug-volumes-status`, with the hash of the set the
//! report reflects in `kubeswift.io/hotplug-volumes-observed`. The controller
//! frees a volume's node attachment only once that hash matches and the
//! volume is not reported, so a volume is reported for as long as Cloud
//! Hypervisor may hold it.
//!
//! The device comes from kubelet, never from the set: kubelet publishes a CSI
//! block volume, per PV and consuming pod, under
//! `<kubelet>/plugins/kubernetes.io/csi/volumeDevices/<pv>/dev/<pod UID>`,
//! which this pod mounts read-only at [`DEVICES_DIR`]. The set only names the
//! PV and the attachment pod. The device is then made at a stable
//! `/dev/kubeswift-hotplug-<name>` and handed to Cloud Hypervisor.
//!
//! What Cloud Hypervisor has is read from `vm.info`, not remembered, so a
//! restarted swiftletd neither adds a disk twice nor forgets one. Disks this
//! module adds have ids `hp-<name>-<attempt>`. An add that fails inside the
//! device manager leaves its id in the device tree for good (Cloud
//! Hypervisor v54: it can be neither re-added nor removed), so each attempt
//! takes the next number, and the free PCI slot is checked first.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Condvar, Mutex, OnceLock};
use std::time::{Duration, Instant};

use k8s_openapi::api::core::v1::Pod;
use kube::api::{Api, Patch, PatchParams};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub const VOLUMES_ANNOTATION: &str = "kubeswift.io/hotplug-volumes";
pub const FORCE_DETACH_ANNOTATION: &str = "kubeswift.io/hotplug-force-detach";
pub const STATUS_ANNOTATION: &str = "kubeswift.io/hotplug-volumes-status";
pub const OBSERVED_ANNOTATION: &str = "kubeswift.io/hotplug-volumes-observed";

/// Advertised in `kubeswift.io/launcher-features` when this launcher can
/// hot-plug.
pub const FEATURE: &str = "hotplug-volumes";

/// Where the launcher pod mounts kubelet's CSI volumeDevices directory.
pub const DEVICES_DIR: &str = "/var/run/kubeswift/volume-devices";

/// Device nodes for hot-plugged volumes: `<prefix><name>`.
const NODE_PREFIX: &str = "/dev/kubeswift-hotplug-";

/// Cloud Hypervisor device ids of hot-plugged disks: `hp-<name>-<attempt>`.
const ID_PREFIX: &str = "hp-";

/// Device numbers on PCI segment 0 (00 is the host bridge).
const PCI_SLOTS: usize = 32;

/// Cadence while something is still moving, and otherwise.
const RETRY: Duration = Duration::from_secs(2);
const RESYNC: Duration = Duration::from_secs(30);

/// A remove-device not acknowledged by the guest within this is sent again.
const REMOVE_RESEND: Duration = Duration::from_secs(30);

const AGENT_TIMEOUT: Duration = Duration::from_secs(5);

static ENABLED: AtomicBool = AtomicBool::new(false);
/// The latest hot-plug annotations read from the pod, and the worker's wake-up.
type Latest = (Mutex<Option<BTreeMap<String, String>>>, Condvar);
static LATEST: OnceLock<Latest> = OnceLock::new();

/// Launcher features this module adds.
pub fn features() -> Vec<&'static str> {
    if ENABLED.load(Ordering::SeqCst) {
        vec![FEATURE]
    } else {
        Vec::new()
    }
}

/// Hands the worker the hot-plug annotations of a pod snapshot (from the
/// action loop's pod feed). Wakes it only when they changed.
pub fn observe(annotations: &BTreeMap<String, String>) {
    let Some((lock, cv)) = LATEST.get() else {
        return;
    };
    let subset: BTreeMap<String, String> = annotations
        .iter()
        .filter(|(k, _)| *k == VOLUMES_ANNOTATION || *k == FORCE_DETACH_ANNOTATION)
        .map(|(k, v)| (k.clone(), v.clone()))
        .collect();
    let mut latest = lock.lock().unwrap_or_else(|e| e.into_inner());
    if latest.as_ref() != Some(&subset) {
        *latest = Some(subset);
        cv.notify_one();
    }
}

/// Starts the worker when this pod mounts kubelet's volumeDevices directory
/// (the controller adds it with the feature on). Must run before the launch
/// reports its features.
pub fn spawn(namespace: String, pod_name: String, api_socket: PathBuf, vsock: Option<PathBuf>) {
    if !Path::new(DEVICES_DIR).is_dir() {
        return;
    }
    ENABLED.store(true, Ordering::SeqCst);
    let _ = LATEST.set((Mutex::new(None), Condvar::new()));
    log::info!("hotplug_enabled devices_dir={}", DEVICES_DIR);
    std::thread::spawn(move || {
        let rt = match tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
        {
            Ok(r) => r,
            Err(e) => {
                log::error!("hotplug_runtime_failed: {}", e);
                return;
            }
        };
        let mut w = Worker {
            ch: swift_ch_client::ApiClient::new(api_socket),
            vsock,
            state: State::default(),
        };
        let (lock, cv) = LATEST.get().expect("set above");
        let mut wait = RETRY;
        loop {
            let ann = {
                let guard = lock.lock().unwrap_or_else(|e| e.into_inner());
                let (guard, _) = cv
                    .wait_timeout(guard, wait)
                    .unwrap_or_else(|e| e.into_inner());
                guard.clone()
            };
            // Nothing read from the pod yet: the action loop has not started.
            let Some(ann) = ann else {
                wait = RETRY;
                continue;
            };
            let pass = w.converge(&ann);
            if let Some(report) = pass.report {
                if w.state.last_report.as_ref() != Some(&report) {
                    match rt.block_on(write_report(&namespace, &pod_name, &report)) {
                        Ok(()) => w.state.last_report = Some(report),
                        Err(e) => log::warn!("hotplug_report_failed (will retry): {}", e),
                    }
                }
            }
            wait = if pass.pending { RETRY } else { RESYNC };
        }
    });
}

/// One volume of the desired set (internal/hotplug.Volume).
#[derive(Debug, Clone, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct Volume {
    pub name: String,
    pub serial: String,
    pub pv_name: String,
    #[serde(rename = "attachmentPodUID")]
    pub attachment_pod_uid: String,
    #[serde(default)]
    pub read_only: bool,
}

/// One volume of the report (internal/hotplug.Status).
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Status {
    pub name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub serial: String,
    pub state: &'static str,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub reason: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub message: String,
    #[serde(skip_serializing_if = "is_zero")]
    pub attempt: u32,
}

fn is_zero(n: &u32) -> bool {
    *n == 0
}

impl Status {
    fn new(name: &str, serial: &str, state: &'static str) -> Self {
        Status {
            name: name.to_string(),
            serial: serial.to_string(),
            state,
            reason: String::new(),
            message: String::new(),
            attempt: 0,
        }
    }
    fn because(mut self, reason: &str, message: impl Into<String>) -> Self {
        self.reason = reason.to_string();
        self.message = message.into();
        self
    }
}

pub const ATTACHING: &str = "Attaching";
pub const ATTACHED: &str = "Attached";
pub const DETACHING: &str = "Detaching";
pub const IN_USE: &str = "InUse";
pub const FAILED: &str = "Failed";

/// The report to write: the statuses (JSON) and the observed set hash.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Report {
    pub status: String,
    pub observed: String,
}

#[derive(Default)]
struct State {
    /// Failed adds, by volume, with what they were for. Not retried until the
    /// volume names another PV or attachment pod.
    failed: HashMap<String, ((String, String), Status)>,
    /// Removes sent and not yet acknowledged, by volume.
    removing: HashMap<String, Instant>,
    last_report: Option<Report>,
}

struct Pass {
    report: Option<Report>,
    pending: bool,
}

struct Worker {
    ch: swift_ch_client::ApiClient,
    vsock: Option<PathBuf>,
    state: State,
}

/// A hot-plugged disk Cloud Hypervisor has.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Plugged {
    pub id: String,
    pub serial: String,
}

impl Worker {
    fn converge(&mut self, ann: &BTreeMap<String, String>) -> Pass {
        let set = ann.get(VOLUMES_ANNOTATION).cloned().unwrap_or_default();
        let desired: Vec<Volume> = if set.trim().is_empty() {
            Vec::new()
        } else {
            match serde_json::from_str(&set) {
                Ok(v) => v,
                Err(e) => {
                    // Report nothing for a set that does not parse: the
                    // controller keeps every volume held until it does.
                    log::error!("hotplug_set_invalid err={}", e);
                    return Pass {
                        report: None,
                        pending: false,
                    };
                }
            }
        };
        let force: HashSet<&str> = ann
            .get(FORCE_DETACH_ANNOTATION)
            .map(|s| s.split(',').filter(|n| !n.is_empty()).collect())
            .unwrap_or_default();

        let info = match self.ch.vm_info_value() {
            Ok(i) if i["state"] == "Running" => i,
            // Not booted yet, paused for a snapshot, or a migration receiver
            // with no VM: nothing can be plugged now.
            Ok(_) | Err(_) => {
                return Pass {
                    report: None,
                    pending: true,
                }
            }
        };
        let plugged = plugged_disks(&info);
        let mut statuses = Vec::new();
        let mut pending = false;

        let wanted: HashSet<&str> = desired.iter().map(|v| v.name.as_str()).collect();
        for v in &desired {
            if let Some(p) = plugged.get(&v.name) {
                self.state.failed.remove(&v.name);
                let mut st = Status::new(&v.name, &v.serial, ATTACHED);
                st.attempt = parse_id(&p.id).map(|(_, a)| a).unwrap_or(0);
                statuses.push(st);
                continue;
            }
            let key = (v.pv_name.clone(), v.attachment_pod_uid.clone());
            if let Some((k, st)) = self.state.failed.get(&v.name) {
                if *k == key {
                    statuses.push(st.clone());
                    continue;
                }
            }
            match self.attach(v, &info) {
                Ok(attempt) => {
                    log::info!(
                        "hotplug_attached name={} serial={} attempt={}",
                        v.name,
                        v.serial,
                        attempt
                    );
                    let mut st = Status::new(&v.name, &v.serial, ATTACHED);
                    st.attempt = attempt;
                    statuses.push(st);
                }
                Err(Fail::Retry(msg)) => {
                    pending = true;
                    statuses
                        .push(Status::new(&v.name, &v.serial, ATTACHING).because("Waiting", msg));
                }
                Err(Fail::Final(reason, msg)) => {
                    log::warn!(
                        "hotplug_attach_failed name={} reason={} {}",
                        v.name,
                        reason,
                        msg
                    );
                    let st = Status::new(&v.name, &v.serial, FAILED).because(reason, msg);
                    self.state.failed.insert(v.name.clone(), (key, st.clone()));
                    statuses.push(st);
                }
            }
        }
        self.state.failed.retain(|n, _| wanted.contains(n.as_str()));

        for (name, disk) in &plugged {
            if wanted.contains(name.as_str()) {
                continue;
            }
            pending = true;
            statuses.push(self.detach(name, disk, force.contains(name.as_str())));
        }
        self.state.removing.retain(|n, _| plugged.contains_key(n));

        // Device nodes of unplugged volumes: reported until Cloud Hypervisor
        // has closed them, then removed.
        for (name, node) in hotplug_nodes() {
            if plugged.contains_key(&name) || wanted.contains(name.as_str()) {
                continue;
            }
            if node_open(&node) {
                pending = true;
                statuses.push(Status::new(&name, "", DETACHING).because(
                    "Releasing",
                    "waiting for Cloud Hypervisor to close the disk",
                ));
            } else if let Err(e) = std::fs::remove_file(&node) {
                log::warn!(
                    "hotplug_node_remove_failed node={} err={}",
                    node.display(),
                    e
                );
            } else {
                log::info!("hotplug_detached name={}", name);
            }
        }

        statuses.sort_by(|a, b| a.name.cmp(&b.name));
        let status = serde_json::to_string(&statuses).unwrap_or_else(|_| "[]".to_string());
        Pass {
            report: Some(Report {
                status,
                observed: set_hash(&set),
            }),
            pending,
        }
    }

    fn attach(&mut self, v: &Volume, info: &serde_json::Value) -> Result<u32, Fail> {
        if !valid_name(&v.name) || !valid_name(&v.pv_name) || !valid_uid(&v.attachment_pod_uid) {
            return Err(Fail::Final(
                "InvalidVolume",
                format!("invalid entry {:?}", v),
            ));
        }
        if v.serial.is_empty() || v.serial.len() > 20 {
            return Err(Fail::Final(
                "InvalidVolume",
                format!("serial {:?} must be 1 to 20 bytes", v.serial),
            ));
        }
        let src = Path::new(DEVICES_DIR)
            .join(&v.pv_name)
            .join("dev")
            .join(&v.attachment_pod_uid);
        let rdev = published_device(&src)?;
        let node = PathBuf::from(format!("{}{}", NODE_PREFIX, v.name));
        make_node(&node, rdev).map_err(|e| Fail::Final("DeviceNodeFailed", e))?;
        if free_slots(info) == 0 {
            return Err(Fail::Final(
                "NoPCISlot",
                "the guest has no free PCI slot for another disk".to_string(),
            ));
        }
        let attempt = next_attempt(info, &v.name);
        let disk = swift_ch_client::HotplugDisk {
            path: node.display().to_string(),
            id: format!("{}{}-{}", ID_PREFIX, v.name, attempt),
            serial: v.serial.clone(),
            readonly: v.read_only,
        };
        self.ch
            .add_disk(&disk)
            .map_err(|e| Fail::Final("AddDiskFailed", e.to_string()))?;
        Ok(attempt)
    }

    fn detach(&mut self, name: &str, disk: &Plugged, force: bool) -> Status {
        if let Some(sent) = self.state.removing.get(name) {
            if sent.elapsed() < REMOVE_RESEND {
                return Status::new(name, &disk.serial, DETACHING)
                    .because("Ejecting", "waiting for the guest to release the device");
            }
        }
        if !force {
            let Some(vsock) = &self.vsock else {
                return Status::new(name, &disk.serial, IN_USE).because(
                    "AgentUnavailable",
                    "the guest has no agent channel to ask whether it uses the disk; set kubeswift.io/force-detach=true to detach anyway",
                );
            };
            match swift_vsock_client::block_in_use(vsock, &disk.serial, AGENT_TIMEOUT) {
                Ok(r) if r.ok && r.in_use => {
                    return Status::new(name, &disk.serial, IN_USE)
                        .because("InUse", r.detail.unwrap_or_default());
                }
                Ok(r) if r.ok => {}
                Ok(r) => {
                    return Status::new(name, &disk.serial, IN_USE).because(
                        "AgentError",
                        format!(
                            "the guest agent could not check the disk: {}; set kubeswift.io/force-detach=true to detach anyway",
                            r.error.unwrap_or_default()
                        ),
                    );
                }
                Err(e) => {
                    return Status::new(name, &disk.serial, IN_USE).because(
                        "AgentUnreachable",
                        format!(
                            "the guest agent did not answer ({}); set kubeswift.io/force-detach=true to detach anyway",
                            e
                        ),
                    );
                }
            }
        }
        match self.ch.remove_device(&disk.id) {
            Ok(()) => {
                log::info!(
                    "hotplug_remove_sent name={} id={} force={}",
                    name,
                    disk.id,
                    force
                );
                self.state.removing.insert(name.to_string(), Instant::now());
                Status::new(name, &disk.serial, DETACHING)
                    .because("Ejecting", "waiting for the guest to release the device")
            }
            Err(e) => {
                Status::new(name, &disk.serial, DETACHING).because("RemoveFailed", e.to_string())
            }
        }
    }
}

enum Fail {
    /// Try again on the next pass.
    Retry(String),
    /// Report Failed until the volume names another PV or attachment pod.
    Final(&'static str, String),
}

/// The device kubelet published at `src`, checked: a block device, a whole
/// disk, and not backing a mount this launcher can see.
fn published_device(src: &Path) -> Result<u64, Fail> {
    let md = match std::fs::metadata(src) {
        Ok(m) => m,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Err(Fail::Retry(format!(
                "kubelet has not published {} yet",
                src.display()
            )));
        }
        Err(e) => return Err(Fail::Retry(format!("stat {}: {}", src.display(), e))),
    };
    if !md.file_type().is_block_device() {
        return Err(Fail::Final(
            "DeviceRefused",
            format!("{} is not a block device", src.display()),
        ));
    }
    let rdev = md.rdev();
    let (major, minor) = (libc::major(rdev), libc::minor(rdev));
    if Path::new(&format!("/sys/dev/block/{}:{}/partition", major, minor)).exists() {
        return Err(Fail::Final(
            "DeviceRefused",
            format!("{}:{} is a partition, not a whole disk", major, minor),
        ));
    }
    if let Ok(mi) = std::fs::read_to_string("/proc/self/mountinfo") {
        if backs_mount(&mi, major, minor) {
            return Err(Fail::Final(
                "DeviceRefused",
                format!("{}:{} backs a mount in the launcher", major, minor),
            ));
        }
    }
    Ok(rdev)
}

/// Whether `major:minor` is the device of a mount in a mountinfo listing.
pub fn backs_mount(mountinfo: &str, major: u32, minor: u32) -> bool {
    let want = format!("{}:{}", major, minor);
    mountinfo
        .lines()
        .any(|l| l.split_whitespace().nth(2) == Some(want.as_str()))
}

/// Makes (or keeps) a block device node for `rdev` at `node`.
fn make_node(node: &Path, rdev: u64) -> Result<(), String> {
    if let Ok(md) = std::fs::symlink_metadata(node) {
        if md.file_type().is_block_device() && md.rdev() == rdev {
            return Ok(());
        }
        std::fs::remove_file(node)
            .map_err(|e| format!("remove stale {}: {}", node.display(), e))?;
    }
    let c =
        std::ffi::CString::new(node.as_os_str().as_encoded_bytes()).map_err(|e| e.to_string())?;
    // SAFETY: c is a valid NUL-terminated path; mknod has no other inputs.
    let rc = unsafe { libc::mknod(c.as_ptr(), libc::S_IFBLK | 0o600, rdev as libc::dev_t) };
    if rc != 0 {
        return Err(format!(
            "mknod {}: {}",
            node.display(),
            std::io::Error::last_os_error()
        ));
    }
    Ok(())
}

/// The hot-plugged disks in a `vm.info` response, by volume name.
pub fn plugged_disks(info: &serde_json::Value) -> HashMap<String, Plugged> {
    let mut out = HashMap::new();
    let Some(disks) = info["config"]["disks"].as_array() else {
        return out;
    };
    for d in disks {
        let Some(id) = d["id"].as_str() else { continue };
        if let Some((name, _)) = parse_id(id) {
            out.insert(
                name.to_string(),
                Plugged {
                    id: id.to_string(),
                    serial: d["serial"].as_str().unwrap_or_default().to_string(),
                },
            );
        }
    }
    out
}

/// `hp-<name>-<attempt>` → (name, attempt).
pub fn parse_id(id: &str) -> Option<(&str, u32)> {
    let rest = id.strip_prefix(ID_PREFIX)?;
    let (name, attempt) = rest.rsplit_once('-')?;
    Some((name, attempt.parse().ok()?))
}

/// The next attempt number for a volume: one past every id it ever had in
/// the device tree, burned ones included.
pub fn next_attempt(info: &serde_json::Value, name: &str) -> u32 {
    let mut max = 0;
    let mut see = |id: &str| {
        if let Some((n, a)) = parse_id(id) {
            if n == name {
                max = max.max(a);
            }
        }
    };
    if let Some(tree) = info["device_tree"].as_object() {
        tree.keys().for_each(|k| see(k));
    }
    if let Some(disks) = info["config"]["disks"].as_array() {
        disks
            .iter()
            .filter_map(|d| d["id"].as_str())
            .for_each(&mut see);
    }
    max + 1
}

/// Free device numbers on PCI segment 0, from the device tree's `pci_bdf`s.
pub fn free_slots(info: &serde_json::Value) -> usize {
    let mut used: HashSet<u32> = HashSet::from([0]);
    if let Some(tree) = info["device_tree"].as_object() {
        for node in tree.values() {
            if let Some(bdf) = node["pci_bdf"].as_str() {
                // "0000:00:1f.0": segment, bus, device.function
                let parts: Vec<&str> = bdf.split(':').collect();
                if parts.len() == 3 && parts[0].trim_start_matches('0').is_empty() {
                    if let Some(dev) = parts[2]
                        .split('.')
                        .next()
                        .and_then(|d| u32::from_str_radix(d, 16).ok())
                    {
                        used.insert(dev);
                    }
                }
            }
        }
    }
    PCI_SLOTS.saturating_sub(used.len())
}

/// The hot-plug device nodes in /dev, by volume name.
fn hotplug_nodes() -> Vec<(String, PathBuf)> {
    let Ok(entries) = std::fs::read_dir("/dev") else {
        return Vec::new();
    };
    let prefix = NODE_PREFIX.trim_start_matches("/dev/");
    entries
        .flatten()
        .filter_map(|e| {
            let n = e.file_name().into_string().ok()?;
            let name = n.strip_prefix(prefix)?.to_string();
            Some((name, e.path()))
        })
        .collect()
}

/// Whether any process in this pod holds `node` open.
fn node_open(node: &Path) -> bool {
    let Ok(procs) = std::fs::read_dir("/proc") else {
        return false;
    };
    for p in procs.flatten() {
        if !p
            .file_name()
            .to_string_lossy()
            .bytes()
            .all(|b| b.is_ascii_digit())
        {
            continue;
        }
        let Ok(fds) = std::fs::read_dir(p.path().join("fd")) else {
            continue;
        };
        for fd in fds.flatten() {
            if std::fs::read_link(fd.path()).is_ok_and(|t| t == node) {
                return true;
            }
        }
    }
    false
}

/// A Kubernetes object name: lowercase alphanumerics, '-' and '.', no path.
pub fn valid_name(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 253
        && s != "."
        && s != ".."
        && s.bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-' || b == b'.')
}

/// A pod UID: hex digits and dashes.
pub fn valid_uid(s: &str) -> bool {
    !s.is_empty() && s.len() <= 64 && s.bytes().all(|b| b.is_ascii_hexdigit() || b == b'-')
}

/// internal/hotplug.SetHash: the first 16 hex digits of the SHA-256 of the
/// exact annotation value ("" when absent).
pub fn set_hash(set: &str) -> String {
    let sum = Sha256::digest(set.as_bytes());
    sum[..8].iter().map(|b| format!("{:02x}", b)).collect()
}

async fn write_report(namespace: &str, pod_name: &str, report: &Report) -> Result<(), String> {
    let client = crate::kube_client::create_client()
        .await
        .map_err(|e| e.to_string())?;
    let api: Api<Pod> = Api::namespaced(client, namespace);
    // One write for both: the controller trusts the report only with the
    // hash of the set it reflects.
    let patch = serde_json::json!({"metadata": {"annotations": {
        STATUS_ANNOTATION: report.status,
        OBSERVED_ANNOTATION: report.observed,
    }}});
    api.patch(pod_name, &PatchParams::default(), &Patch::Merge(&patch))
        .await
        .map(drop)
        .map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn set_hash_matches_go() {
        // internal/hotplug.SetHash("[]") and SetHash("").
        assert_eq!(set_hash("[]"), "4f53cda18c2baa0c");
        assert_eq!(set_hash(""), "e3b0c44298fc1c14");
    }

    #[test]
    fn parses_ids() {
        assert_eq!(parse_id("hp-db-data-3"), Some(("db-data", 3)));
        assert_eq!(parse_id("_disk0"), None);
        assert_eq!(parse_id("hp-x"), None);
    }

    #[test]
    fn plugged_and_attempts() {
        let info = json!({
            "config": {"disks": [
                {"id": "_disk0", "path": "/dev/root"},
                {"id": "hp-data-2", "serial": "ksabc"}
            ]},
            "device_tree": {
                "hp-data-1": {"pci_bdf": null},
                "hp-data-2": {"pci_bdf": null},
                "hp-logs-7": {"pci_bdf": null}
            }
        });
        let p = plugged_disks(&info);
        assert_eq!(p.len(), 1);
        assert_eq!(
            p["data"],
            Plugged {
                id: "hp-data-2".into(),
                serial: "ksabc".into()
            }
        );
        assert_eq!(next_attempt(&info, "data"), 3);
        assert_eq!(next_attempt(&info, "logs"), 8);
        assert_eq!(next_attempt(&info, "new"), 1);
    }

    #[test]
    fn counts_free_slots() {
        let mut tree = serde_json::Map::new();
        for d in 1..31u32 {
            tree.insert(
                format!("dev{}", d),
                json!({"pci_bdf": format!("0000:00:{:02x}.0", d)}),
            );
        }
        tree.insert("disk-no-bdf".into(), json!({"pci_bdf": null}));
        let info = json!({"device_tree": tree});
        assert_eq!(free_slots(&info), 1); // 00 + 01..1e used, 1f free
        let mut full = info.clone();
        full["device_tree"]["last"] = json!({"pci_bdf": "0000:00:1f.0"});
        assert_eq!(free_slots(&full), 0);
    }

    #[test]
    fn validates_names() {
        assert!(valid_name("pvc-1234.abc"));
        assert!(!valid_name("../etc"));
        assert!(!valid_name("a/b"));
        assert!(!valid_name(".."));
        assert!(valid_uid("0f8e1b7a-3c2d-4e5f-9a8b-7c6d5e4f3a2b"));
        assert!(!valid_uid("../x"));
    }

    #[test]
    fn mount_check() {
        let mi = "22 1 9:2 / / rw - ext4 /dev/md2 rw\n30 22 8:16 / /data rw - ext4 /dev/sdb rw\n";
        assert!(backs_mount(mi, 8, 16));
        assert!(!backs_mount(mi, 8, 32));
    }

    #[test]
    fn decodes_the_controllers_set() {
        let set = r#"[{"name":"data","serial":"ks0123","pvName":"pvc-1","attachmentPodUID":"ab-cd","readOnly":true}]"#;
        let v: Vec<Volume> = serde_json::from_str(set).unwrap();
        assert_eq!(v[0].pv_name, "pvc-1");
        assert_eq!(v[0].attachment_pod_uid, "ab-cd");
        assert!(v[0].read_only);
    }

    #[test]
    fn status_json_matches_go() {
        let st = Status::new("data", "ks1", IN_USE).because("InUse", "mounted at /srv");
        assert_eq!(
            serde_json::to_string(&st).unwrap(),
            r#"{"name":"data","serial":"ks1","state":"InUse","reason":"InUse","message":"mounted at /srv"}"#
        );
    }
}

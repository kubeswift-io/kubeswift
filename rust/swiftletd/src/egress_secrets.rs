//! Egress credentials (spec.network.egress.credentials) for a sandbox whose
//! egress proxy terminates the guest's TLS to swap a placeholder for a real
//! value.
//!
//! The controller gives every such launcher a memory-backed directory shared
//! only with the proxy (`KUBESWIFT_EGRESS_SECRETS`, JSON: [`Spec`]) and names
//! its files in the proxy's egress-intent file. This module fills it:
//!
//! - an intercept CA (ECDSA P-256, 24 h, name-constrained to the credential
//!   hosts) generated here; its key stays in this process and is never
//!   written, so the proxy gets leaf certificates (one per host) instead;
//! - each credential's value (`cred-<name>`) and rendered header value
//!   (`header-<name>`).
//!
//! The directory is replaced as a whole, kubelet style: the files are
//! symlinks to `..data/<name>`, and `..data` is a symlink renamed onto a fresh
//! directory, so a proxy watching the directory reloads everything together.
//! Files are 0440 root:<proxy gid>.
//!
//! `swiftletd egress-secrets-init` (an init container, before the proxy
//! starts) writes throwaway material with empty values so the proxy can load
//! its configuration; a request then fails at the proxy (an empty value
//! answers 401). The real material is delivered before the workload starts
//! (cold) or at checkout (warm), and swiftletd waits for the preset's
//! readiness probe to list every new leaf's serial number before the
//! workload gets its placeholder. The guest gets the CA certificate and a
//! bundle (the image's own CA bundle plus the CA) as files; the values never
//! enter the guest.
use std::collections::BTreeMap;
use std::io::{Read, Write};
use std::net::TcpStream;
use std::os::unix::fs::{symlink, PermissionsExt};
use std::path::Path;
#[cfg(test)]
use std::path::PathBuf;
use std::time::{Duration, Instant};

use rcgen::{
    BasicConstraints, CertificateParams, DistinguishedName, DnType, ExtendedKeyUsagePurpose,
    GeneralSubtree, IsCa, Issuer, KeyPair, KeyUsagePurpose, NameConstraints, SerialNumber,
    PKCS_ECDSA_P256_SHA256,
};
use serde::Deserialize;

/// The init container's subcommand.
pub const SUBCOMMAND: &str = "egress-secrets-init";
/// The env var holding the [`Spec`].
pub const ENV: &str = "KUBESWIFT_EGRESS_SECRETS";

const VALIDITY: Duration = Duration::from_secs(24 * 3600);
const READY_TIMEOUT: Duration = Duration::from_secs(30);
const DATA: &str = "..data";

#[derive(Debug, Clone, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct Spec {
    pub dir: String,
    pub gid: u32,
    pub hosts: Vec<String>,
    pub credentials: Vec<SpecCredential>,
    #[serde(default)]
    pub readiness: Option<Readiness>,
    /// Guest path of the CA certificate.
    pub ca_cert: String,
    /// Guest path of the CA bundle (image bundle + CA).
    pub ca_bundle: String,
}

#[derive(Debug, Clone, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct SpecCredential {
    pub name: String,
    pub value_template: String,
}

#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct Readiness {
    pub port: u16,
    pub path: String,
}

/// The spec from the environment; None when this launcher has no secrets
/// volume.
pub fn spec_from_env() -> Option<Result<Spec, String>> {
    let raw = std::env::var(ENV).ok()?;
    Some(serde_json::from_str(&raw).map_err(|e| format!("{}: {}", ENV, e)))
}

/// TLS material for one delivery. The CA key is dropped with this value.
pub struct Material {
    ca_pem: String,
    /// (cert PEM, key PEM, serial as lowercase hex), host order.
    leaves: Vec<(String, String, String)>,
}

impl Material {
    pub fn serials(&self) -> Vec<String> {
        self.leaves.iter().map(|l| l.2.clone()).collect()
    }
    pub fn ca_pem(&self) -> &str {
        &self.ca_pem
    }
}

fn random_serial() -> Result<(SerialNumber, String), String> {
    let mut b = [0u8; 16];
    std::fs::File::open("/dev/urandom")
        .and_then(|mut f| f.read_exact(&mut b))
        .map_err(|e| format!("random serial: {}", e))?;
    // Positive, and no leading zero nibble, so the hex a proxy prints is
    // exactly these bytes.
    b[0] = 0x10 | (b[0] & 0x6f);
    let hex = b.iter().map(|x| format!("{:02x}", x)).collect();
    Ok((SerialNumber::from_slice(&b), hex))
}

/// Issues a fresh CA and one leaf certificate per host.
pub fn issue(hosts: &[String]) -> Result<Material, String> {
    let now = time::OffsetDateTime::now_utc();
    let ca_key =
        KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256).map_err(|e| format!("CA key: {}", e))?;
    let mut ca = CertificateParams::default();
    let mut dn = DistinguishedName::new();
    dn.push(DnType::CommonName, "KubeSwift sandbox egress intercept CA");
    ca.distinguished_name = dn;
    ca.is_ca = IsCa::Ca(BasicConstraints::Constrained(0));
    ca.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    ca.name_constraints = Some(NameConstraints {
        permitted_subtrees: hosts
            .iter()
            .map(|h| GeneralSubtree::DnsName(h.clone()))
            .collect(),
        excluded_subtrees: vec![],
    });
    ca.not_before = now - time::Duration::minutes(5);
    ca.not_after = now + VALIDITY;
    ca.serial_number = Some(random_serial()?.0);
    let ca_cert = ca
        .self_signed(&ca_key)
        .map_err(|e| format!("CA certificate: {}", e))?;
    let ca_pem = ca_cert.pem();
    let issuer = Issuer::new(ca, ca_key);
    let mut leaves = Vec::with_capacity(hosts.len());
    for h in hosts {
        let key = KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256)
            .map_err(|e| format!("leaf key: {}", e))?;
        let mut p =
            CertificateParams::new(vec![h.clone()]).map_err(|e| format!("leaf {}: {}", h, e))?;
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, h.as_str());
        p.distinguished_name = dn;
        p.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        p.not_before = now - time::Duration::minutes(5);
        p.not_after = now + VALIDITY;
        let (serial, hex) = random_serial()?;
        p.serial_number = Some(serial);
        let cert = p
            .signed_by(&key, &issuer)
            .map_err(|e| format!("leaf {}: {}", h, e))?;
        leaves.push((cert.pem(), key.serialize_pem(), hex));
    }
    Ok(Material { ca_pem, leaves })
}

/// The header value: the template with the value for "{}". A value with a
/// line break would split the header, so it is refused.
pub fn render(template: &str, value: &str) -> Result<String, String> {
    if value.contains(['\r', '\n']) {
        return Err("the value contains a line break".to_string());
    }
    Ok(template.replacen("{}", value, 1))
}

/// The files of one delivery, by name. Values are empty for a slot that no
/// sandbox has claimed.
fn files(
    spec: &Spec,
    m: &Material,
    values: &BTreeMap<String, String>,
) -> Result<Vec<(String, Vec<u8>)>, String> {
    let mut out = vec![("ca.crt".to_string(), m.ca_pem.clone().into_bytes())];
    for (i, (cert, key, _)) in m.leaves.iter().enumerate() {
        out.push((format!("cert-{}.crt", i), cert.clone().into_bytes()));
        out.push((format!("cert-{}.key", i), key.clone().into_bytes()));
    }
    for c in &spec.credentials {
        let v = values.get(&c.name).map(String::as_str).unwrap_or("");
        let header = if v.is_empty() {
            String::new()
        } else {
            render(&c.value_template, v)
                .map_err(|e| format!("egress credential {}: {}", c.name, e))?
        };
        out.push((format!("cred-{}", c.name), v.as_bytes().to_vec()));
        out.push((format!("header-{}", c.name), header.into_bytes()));
    }
    Ok(out)
}

fn chown(p: &Path, gid: u32) -> Result<(), String> {
    std::os::unix::fs::chown(p, None, Some(gid))
        .map_err(|e| format!("chown {}: {}", p.display(), e))
}

/// Replaces the directory's content as a whole (see the module doc).
pub fn write(spec: &Spec, m: &Material, values: &BTreeMap<String, String>) -> Result<(), String> {
    let dir = Path::new(&spec.dir);
    let files = files(spec, m, values)?;
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    let gen_name = format!("..{}", nanos);
    let gen = dir.join(&gen_name);
    let io = |what: &str, p: &Path, e: std::io::Error| format!("{} {}: {}", what, p.display(), e);
    std::fs::create_dir(&gen).map_err(|e| io("mkdir", &gen, e))?;
    for (name, data) in &files {
        let p = gen.join(name);
        let mut f = std::fs::File::create(&p).map_err(|e| io("create", &p, e))?;
        f.write_all(data).map_err(|e| io("write", &p, e))?;
        f.sync_all().map_err(|e| io("sync", &p, e))?;
        chown(&p, spec.gid)?;
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(0o440))
            .map_err(|e| io("chmod", &p, e))?;
    }
    chown(&gen, spec.gid)?;
    std::fs::set_permissions(&gen, std::fs::Permissions::from_mode(0o750))
        .map_err(|e| io("chmod", &gen, e))?;
    let tmp = dir.join("..data_tmp");
    let _ = std::fs::remove_file(&tmp);
    symlink(&gen_name, &tmp).map_err(|e| io("symlink", &tmp, e))?;
    std::fs::rename(&tmp, dir.join(DATA)).map_err(|e| io("rename", &tmp, e))?;
    for (name, _) in &files {
        let link = dir.join(name);
        if std::fs::symlink_metadata(&link).is_err() {
            symlink(format!("{}/{}", DATA, name), &link).map_err(|e| io("symlink", &link, e))?;
        }
    }
    // Earlier generations: the proxy has the new one (or will on its next
    // read through ..data).
    if let Ok(rd) = std::fs::read_dir(dir) {
        for e in rd.flatten() {
            let n = e.file_name().to_string_lossy().to_string();
            if n.starts_with("..")
                && n != gen_name
                && n != DATA
                && n.trim_start_matches('.')
                    .chars()
                    .all(|c| c.is_ascii_digit())
            {
                let _ = std::fs::remove_dir_all(e.path());
            }
        }
    }
    let _ = chown(dir, spec.gid);
    let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o750));
    Ok(())
}

/// `swiftletd egress-secrets-init`: throwaway material, empty values.
pub fn init_main() -> ! {
    let code = match spec_from_env() {
        None => {
            log::error!("egress_secrets_init: {} unset", ENV);
            1
        }
        Some(Err(e)) => {
            log::error!("egress_secrets_init: {}", e);
            1
        }
        Some(Ok(spec)) => match issue(&spec.hosts).and_then(|m| write(&spec, &m, &BTreeMap::new()))
        {
            Ok(()) => {
                log::info!(
                    "egress_secrets_init hosts={} credentials={}",
                    spec.hosts.len(),
                    spec.credentials.len()
                );
                0
            }
            Err(e) => {
                log::error!("egress_secrets_init: {}", e);
                1
            }
        },
    };
    std::process::exit(code)
}

/// One readiness probe: Ok(true) once the response is 200 and lists every
/// serial; Err with the status line otherwise. HTTP/1.1: Envoy refuses
/// HTTP/1.0 (426).
fn probe_once(r: &Readiness, serials: &[String]) -> Result<bool, String> {
    let mut s =
        TcpStream::connect_timeout(&([127, 0, 0, 1], r.port).into(), Duration::from_secs(2))
            .map_err(|e| format!("connect 127.0.0.1:{}: {}", r.port, e))?;
    s.set_read_timeout(Some(Duration::from_secs(2))).ok();
    write!(
        s,
        "GET {} HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n",
        r.path
    )
    .map_err(|e| e.to_string())?;
    let mut raw = Vec::new();
    s.take(4 << 20)
        .read_to_end(&mut raw)
        .map_err(|e| e.to_string())?;
    let text = String::from_utf8_lossy(&raw);
    let (head, body) = text.split_once("\r\n\r\n").unwrap_or((&text, ""));
    let status = head.lines().next().unwrap_or("");
    if !(status.starts_with("HTTP/1.1 200") || status.starts_with("HTTP/1.0 200")) {
        return Err(format!("answered {:?}", status));
    }
    let chunked = head
        .lines()
        .any(|l| l.to_ascii_lowercase().replace(' ', "") == "transfer-encoding:chunked");
    let body = if chunked {
        dechunk(body)
    } else {
        body.to_string()
    };
    Ok(serials.iter().all(|s| body.contains(s.as_str())))
}

/// The payload of a chunked HTTP/1.1 body (a serial may span two chunks).
fn dechunk(mut b: &str) -> String {
    let mut out = String::new();
    while let Some((size, rest)) = b.split_once("\r\n") {
        let n = usize::from_str_radix(size.split(';').next().unwrap_or("").trim(), 16).unwrap_or(0);
        if n == 0 || rest.len() < n {
            break;
        }
        out.push_str(&rest[..n]);
        b = rest[n..].trim_start_matches("\r\n");
    }
    out
}

/// Waits until the proxy's readiness probe lists every serial.
pub fn wait_ready(r: &Readiness, serials: &[String], timeout: Duration) -> Result<(), String> {
    let start = Instant::now();
    let mut last;
    loop {
        match probe_once(r, serials) {
            Ok(true) => {
                log::info!(
                    "egress_credentials_loaded ms={}",
                    start.elapsed().as_millis()
                );
                return Ok(());
            }
            Ok(false) => last = "the proxy does not serve the new certificates yet".to_string(),
            Err(e) => last = e,
        }
        if start.elapsed() >= timeout {
            return Err(format!(
                "the egress proxy did not load the credentials within {}s (readiness GET 127.0.0.1:{}{}: {})",
                timeout.as_secs(),
                r.port,
                r.path,
                last
            ));
        }
        std::thread::sleep(Duration::from_millis(100));
    }
}

/// Why a delivery failed, for the reason the sandbox reports.
#[derive(Debug, PartialEq)]
pub enum Failure {
    /// A Secret could not be read (SecretUnavailable).
    Secret(String),
    /// The material could not be written or the proxy did not load it
    /// (EgressProxyNotReady).
    Proxy(String),
}

/// Reads the values, issues and writes fresh material, and waits for the
/// proxy. Returns the material (for the guest's CA files).
pub async fn deliver(
    spec: &Spec,
    refs: &[crate::intent::EgressCredentialRef],
) -> Result<Material, Failure> {
    let ns = std::env::var("POD_NAMESPACE")
        .map_err(|_| Failure::Secret("POD_NAMESPACE unset".to_string()))?;
    let client = crate::kube_client::create_client()
        .await
        .map_err(|e| Failure::Secret(format!("egress credentials: kube client: {}", e)))?;
    let values = crate::secrets::resolve_credentials(&client, &ns, refs)
        .await
        .map_err(Failure::Secret)?;
    let spec = spec.clone();
    tokio::task::spawn_blocking(move || {
        let m = issue(&spec.hosts).map_err(Failure::Proxy)?;
        write(&spec, &m, &values).map_err(Failure::Proxy)?;
        log::info!(
            "egress_credentials_written count={} hosts={}",
            values.len(),
            spec.hosts.len()
        );
        let r = spec.readiness.as_ref().ok_or_else(|| {
            Failure::Proxy("the egress proxy preset has no readiness probe".to_string())
        })?;
        wait_ready(r, &m.serials(), READY_TIMEOUT).map_err(Failure::Proxy)?;
        Ok(m)
    })
    .await
    .map_err(|e| Failure::Proxy(format!("egress credentials: {}", e)))?
}

/// Bundles an image may read, most common first.
const IMAGE_BUNDLES: &[&str] = &[
    "/etc/ssl/certs/ca-certificates.crt",
    "/etc/pki/tls/certs/ca-bundle.crt",
    "/etc/ssl/cert.pem",
    "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
    "/etc/ssl/ca-bundle.pem",
];

/// Where a warm checkout stages its CA files and bundle copies in the slot's
/// staging share. Artifact names are DNS labels without "--" next to a
/// prefix like this one's, and the action refuses a clash anyway.
pub const STAGE_PREFIX: &str = "ks--egress";

/// A warm checkout's trust material, through the staging share instead of
/// files in the exec request (the guest agent reads at most 64 KiB per
/// request, and an image's bundle alone is larger). Returns the mounts: the
/// directory holding spec.ca_cert and spec.ca_bundle, and, for each image
/// bundle, a copy of its directory (files, and symlinks as symlinks) with the
/// CA appended to the bundle, bound read-only over the original. The guest
/// then sees what a cold sandbox sees in its writable layer. A bundle whose
/// directory has a subdirectory is skipped (the copy would hide it).
pub fn stage_warm(
    spec: &Spec,
    ca_pem: &str,
    rootfs: &dyn crate::rootfs_read::RootfsReader,
    stage: &Path,
) -> Result<Vec<swift_vsock_client::ExecMount>, String> {
    if let Ok(rd) = std::fs::read_dir(stage) {
        for e in rd.flatten() {
            if e.file_name().to_string_lossy().starts_with(STAGE_PREFIX) {
                let _ = std::fs::remove_dir_all(e.path());
            }
        }
    }
    let files = guest_files(spec, ca_pem, rootfs);
    let parent = |p: &str| -> String {
        Path::new(p)
            .parent()
            .and_then(Path::to_str)
            .unwrap_or("/")
            .to_string()
    };
    let own_dir = parent(&spec.ca_cert);
    let mut dirs: BTreeMap<String, Vec<&crate::intent::GuestFile>> = BTreeMap::new();
    for f in &files {
        dirs.entry(parent(&f.path)).or_default().push(f);
    }
    let mut mounts = Vec::new();
    for (i, (dir, fs)) in dirs.iter().enumerate() {
        let name = format!("{}-{}", STAGE_PREFIX, i);
        let dest = stage.join(&name);
        if *dir == own_dir {
            std::fs::create_dir_all(&dest).map_err(|e| format!("{}: {}", dest.display(), e))?;
        } else if let Err(e) = rootfs.copy_dir(dir, &dest) {
            log::warn!("egress_ca_bundle_skipped dir={} reason={}", dir, e);
            let _ = std::fs::remove_dir_all(&dest);
            continue;
        }
        for f in fs {
            let to = dest.join(Path::new(&f.path).file_name().unwrap_or_default());
            let _ = std::fs::remove_file(&to);
            std::fs::write(&to, &f.data).map_err(|e| format!("{}: {}", to.display(), e))?;
            std::fs::set_permissions(&to, std::fs::Permissions::from_mode(0o444))
                .map_err(|e| e.to_string())?;
        }
        std::fs::set_permissions(&dest, std::fs::Permissions::from_mode(0o755))
            .map_err(|e| e.to_string())?;
        mounts.push(swift_vsock_client::ExecMount {
            name,
            path: dir.clone(),
        });
    }
    Ok(mounts)
}

/// The guest files that make the workload trust the intercept CA: the CA
/// certificate, the bundle at spec.ca_bundle, and each CA bundle the image
/// has (resolved through its symlinks) with the CA appended. They land in the
/// guest's writable layer, so the image's own files are untouched.
pub fn guest_files(
    spec: &Spec,
    ca_pem: &str,
    rootfs: &dyn crate::rootfs_read::RootfsReader,
) -> Vec<crate::intent::GuestFile> {
    let mut out = vec![crate::intent::GuestFile {
        path: spec.ca_cert.clone(),
        mode: 0o444,
        data: ca_pem.as_bytes().to_vec(),
    }];
    let mut seen = std::collections::BTreeSet::new();
    let mut first: Option<Vec<u8>> = None;
    for b in IMAGE_BUNDLES {
        let Some(real) = rootfs.resolve(b) else {
            continue;
        };
        if !seen.insert(real.clone()) {
            continue;
        }
        let Some(mut data) = rootfs.read(&real) else {
            continue;
        };
        if !data.is_empty() && !data.ends_with(b"\n") {
            data.push(b'\n');
        }
        if first.is_none() {
            first = Some(data.clone());
        }
        data.extend_from_slice(ca_pem.as_bytes());
        log::info!("egress_ca_appended path={}", real);
        out.push(crate::intent::GuestFile {
            path: real,
            mode: 0o444,
            data,
        });
    }
    let mut bundle = first.unwrap_or_default();
    bundle.extend_from_slice(ca_pem.as_bytes());
    out.push(crate::intent::GuestFile {
        path: spec.ca_bundle.clone(),
        mode: 0o444,
        data: bundle,
    });
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(dir: &Path) -> Spec {
        Spec {
            dir: dir.display().to_string(),
            gid: nix_gid(),
            hosts: vec!["api.example.com".into(), "models.example.org".into()],
            credentials: vec![
                SpecCredential {
                    name: "API_KEY".into(),
                    value_template: "Bearer {}".into(),
                },
                SpecCredential {
                    name: "ORG".into(),
                    value_template: "{}".into(),
                },
            ],
            readiness: None,
            ca_cert: "/etc/kubeswift/egress-ca.crt".into(),
            ca_bundle: "/etc/kubeswift/egress-ca-bundle.crt".into(),
        }
    }

    // The test runs unprivileged: chown to our own group is allowed.
    fn nix_gid() -> u32 {
        unsafe { libc::getgid() }
    }

    #[test]
    fn issues_a_name_constrained_ca_and_a_leaf_per_host() {
        let m = issue(&["api.example.com".into(), "b.example.org".into()]).unwrap();
        assert_eq!(m.leaves.len(), 2);
        assert!(m.ca_pem.starts_with("-----BEGIN CERTIFICATE-----"));
        for (cert, key, serial) in &m.leaves {
            assert!(cert.contains("BEGIN CERTIFICATE") && key.contains("PRIVATE KEY"));
            assert_eq!(serial.len(), 32);
            assert!(serial.starts_with(|c: char| c != '0'));
        }
        assert_ne!(m.leaves[0].2, m.leaves[1].2);
    }

    #[test]
    fn renders_and_refuses_line_breaks() {
        assert_eq!(render("Bearer {}", "s3cret").unwrap(), "Bearer s3cret");
        assert_eq!(render("{}", "v").unwrap(), "v");
        assert!(render("{}", "a\r\nX-Evil: 1").is_err());
    }

    #[test]
    fn swaps_the_whole_directory() {
        let d = tempfile::tempdir().unwrap();
        let s = spec(d.path());
        let m1 = issue(&s.hosts).unwrap();
        write(&s, &m1, &BTreeMap::new()).unwrap();
        let read = |n: &str| std::fs::read_to_string(d.path().join(n)).unwrap();
        assert_eq!(read("cred-API_KEY"), "");
        assert_eq!(read("header-API_KEY"), "");
        assert_eq!(read("ca.crt"), m1.ca_pem);
        let m2 = issue(&s.hosts).unwrap();
        let mut v = BTreeMap::new();
        v.insert("API_KEY".to_string(), "s3cret".to_string());
        v.insert("ORG".to_string(), "org-1".to_string());
        write(&s, &m2, &v).unwrap();
        assert_eq!(read("cred-API_KEY"), "s3cret");
        assert_eq!(read("header-API_KEY"), "Bearer s3cret");
        assert_eq!(read("header-ORG"), "org-1");
        assert_eq!(read("cert-1.crt"), m2.leaves[1].0);
        // One generation left, files are symlinks into ..data, mode 0440.
        let gens: Vec<_> = std::fs::read_dir(d.path())
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().to_string())
            .filter(|n| n.starts_with("..") && n != DATA)
            .collect();
        assert_eq!(gens.len(), 1, "{:?}", gens);
        assert_eq!(
            std::fs::read_link(d.path().join("ca.crt")).unwrap(),
            PathBuf::from("..data/ca.crt")
        );
        let mode = std::fs::metadata(d.path().join("cred-API_KEY"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(mode, 0o440);
        // The CA key is never written.
        for e in std::fs::read_dir(d.path().join(DATA)).unwrap().flatten() {
            let body = std::fs::read_to_string(e.path()).unwrap();
            if e.file_name().to_string_lossy().starts_with("cert-")
                && e.file_name().to_string_lossy().ends_with(".key")
            {
                continue;
            }
            assert!(
                !body.contains("PRIVATE KEY"),
                "{:?} holds a key",
                e.file_name()
            );
        }
    }

    #[test]
    fn readiness_waits_for_every_serial() {
        // Like Envoy's admin: 426 for HTTP/1.0, chunked bodies for 1.1.
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        std::thread::spawn(move || {
            for (n, s) in l.incoming().enumerate() {
                let mut s = s.unwrap();
                let mut buf = [0u8; 512];
                let got = s.read(&mut buf).unwrap_or(0);
                if !String::from_utf8_lossy(&buf[..got]).contains("HTTP/1.1") {
                    let _ = write!(
                        s,
                        "HTTP/1.1 426 Upgrade Required\r\nContent-Length: 0\r\n\r\n"
                    );
                    continue;
                }
                // The second serial split across two chunks.
                let chunks: &[&str] = if n < 2 {
                    &["\"serial_number\": \"aa\""]
                } else {
                    &["\"serial_number\": \"aa\" \"serial_number\": \"b", "b\""]
                };
                let mut body = String::new();
                for c in chunks {
                    body.push_str(&format!("{:x}\r\n{}\r\n", c.len(), c));
                }
                let _ = write!(
                    s,
                    "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n{}0\r\n\r\n",
                    body
                );
            }
        });
        let r = Readiness {
            port,
            path: "/certs".into(),
        };
        wait_ready(&r, &["aa".into(), "bb".into()], Duration::from_secs(5)).unwrap();
        let err = wait_ready(&r, &["cc".into()], Duration::from_millis(300)).unwrap_err();
        assert!(err.contains("did not load"), "{}", err);
        assert_eq!(dechunk("3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n"), "abcde");
    }

    struct Fake(BTreeMap<String, (Option<String>, Vec<u8>)>);
    impl crate::rootfs_read::RootfsReader for Fake {
        fn lstat(&self, p: &str) -> crate::rootfs_read::Entry {
            match self.0.get(p) {
                None => crate::rootfs_read::Entry::Missing,
                Some((Some(t), _)) => crate::rootfs_read::Entry::Symlink(t.clone()),
                Some((None, _)) => crate::rootfs_read::Entry::File,
            }
        }
        fn read(&self, p: &str) -> Option<Vec<u8>> {
            self.0.get(p).map(|e| e.1.clone())
        }
        fn copy_dir(&self, dir: &str, dest: &Path) -> Result<(), String> {
            std::fs::create_dir(dest).map_err(|e| e.to_string())?;
            for (p, (link, data)) in &self.0 {
                if Path::new(p).parent().and_then(Path::to_str) != Some(dir) {
                    continue;
                }
                let to = dest.join(Path::new(p).file_name().unwrap());
                match link {
                    Some(t) => std::os::unix::fs::symlink(t, to).unwrap(),
                    None if !data.is_empty() => std::fs::write(to, data).unwrap(),
                    None => {}
                }
            }
            Ok(())
        }
    }

    #[test]
    fn appends_the_ca_to_the_image_bundles() {
        let mut m = BTreeMap::new();
        m.insert("/etc".to_string(), (None, vec![]));
        m.insert("/etc/ssl".to_string(), (None, vec![]));
        m.insert("/etc/ssl/certs".to_string(), (None, vec![]));
        m.insert(
            "/etc/ssl/certs/ca-certificates.crt".to_string(),
            (None, b"ROOTS".to_vec()),
        );
        m.insert(
            "/etc/ssl/cert.pem".to_string(),
            (Some("certs/ca-certificates.crt".into()), vec![]),
        );
        let s = spec(Path::new("/x"));
        let files = guest_files(&s, "CA\n", &Fake(m));
        let by: BTreeMap<_, _> = files
            .iter()
            .map(|f| (f.path.clone(), String::from_utf8(f.data.clone()).unwrap()))
            .collect();
        assert_eq!(by["/etc/kubeswift/egress-ca.crt"], "CA\n");
        assert_eq!(by["/etc/ssl/certs/ca-certificates.crt"], "ROOTS\nCA\n");
        assert_eq!(by["/etc/kubeswift/egress-ca-bundle.crt"], "ROOTS\nCA\n");
        // The symlink resolves to the same file: written once.
        assert_eq!(files.len(), 3, "{:?}", by.keys());
    }

    #[test]
    fn stages_the_bundle_directories_for_a_warm_checkout() {
        let mut m = BTreeMap::new();
        m.insert("/etc".to_string(), (None, vec![]));
        m.insert("/etc/ssl".to_string(), (None, vec![]));
        m.insert("/etc/ssl/certs".to_string(), (None, vec![]));
        m.insert(
            "/etc/ssl/certs/ca-certificates.crt".to_string(),
            (None, b"ROOTS".to_vec()),
        );
        m.insert(
            "/etc/ssl/certs/abcd.0".to_string(),
            (Some("ca-certificates.crt".into()), vec![]),
        );
        let mut s = spec(Path::new("/x"));
        s.ca_cert = "/etc/kubeswift/egress/ca.crt".into();
        s.ca_bundle = "/etc/kubeswift/egress/ca-bundle.crt".into();
        let stage = tempfile::tempdir().unwrap();
        // A previous checkout's leftovers go.
        std::fs::create_dir(stage.path().join(format!("{}-9", STAGE_PREFIX))).unwrap();
        let mounts = stage_warm(&s, "CA\n", &Fake(m), stage.path()).unwrap();
        let by: BTreeMap<_, _> = mounts
            .iter()
            .map(|m| (m.path.clone(), stage.path().join(&m.name)))
            .collect();
        assert_eq!(by.len(), 2, "{:?}", by);
        let own = &by["/etc/kubeswift/egress"];
        assert_eq!(std::fs::read_to_string(own.join("ca.crt")).unwrap(), "CA\n");
        assert_eq!(
            std::fs::read_to_string(own.join("ca-bundle.crt")).unwrap(),
            "ROOTS\nCA\n"
        );
        let certs = &by["/etc/ssl/certs"];
        assert_eq!(
            std::fs::read_to_string(certs.join("ca-certificates.crt")).unwrap(),
            "ROOTS\nCA\n"
        );
        assert_eq!(
            std::fs::read_link(certs.join("abcd.0")).unwrap(),
            PathBuf::from("ca-certificates.crt")
        );
        assert!(!stage.path().join(format!("{}-9", STAGE_PREFIX)).exists());
    }
}

mod intent;
mod kube_client;
mod launch;
mod lease;
mod report;

use std::env;
use std::path::Path;
use std::process::Command;
use std::sync::Arc;

/// Creates NoCloud seed ISO from directory. CH expects disk image, not directory.
/// Passes meta-data, user-data, network-config explicitly for correct root-level layout.
/// -volid cidata: cloud-init identifies NoCloud datasource by this volume label.
/// -rock: Rock Ridge extensions, preserves full lowercase filenames (meta-data not META_DAT.).
/// -joliet: Joliet extensions, additional filename compatibility.
fn create_seed_iso(seed_dir: &Path, output_iso: &Path) -> Result<(), String> {
    let mut files = Vec::new();
    for name in ["meta-data", "user-data", "network-config"] {
        let p = seed_dir.join(name);
        if p.exists() {
            files.push(name.to_string());
        }
    }
    if files.is_empty() {
        return Err("no seed files (meta-data, user-data, network-config) found".to_string());
    }
    let mut args = vec![
        "-output",
        output_iso.to_str().ok_or("invalid output path")?,
        "-volid",
        "cidata",
        "-joliet",
        "-rock",
        "-input-charset",
        "utf-8",
    ];
    args.extend(files.iter().map(String::as_str));
    let status = Command::new("genisoimage")
        .args(&args)
        .current_dir(seed_dir)
        .status()
        .map_err(|e| format!("genisoimage exec failed: {}", e))?;
    if !status.success() {
        return Err(format!("genisoimage exited with {:?}", status.code()));
    }
    Ok(())
}

const VERSION: &str = env!("KUBESWIFT_VERSION");
const GIT_COMMIT: &str = env!("KUBESWIFT_GIT_COMMIT");

fn main() {
    if env::args().any(|a| a == "--version" || a == "-V") {
        eprintln!("swiftletd {} (git {})", VERSION, GIT_COMMIT);
        std::process::exit(0);
    }

    let intent_path =
        env::var("KUBESWIFT_INTENT_PATH").unwrap_or_else(|_| intent::INTENT_PATH.to_string());

    match intent::load_intent(&intent_path) {
        Ok(intent) => {
            eprintln!("swiftletd: {} (git {})", VERSION, GIT_COMMIT);
            eprintln!("swiftletd: loaded intent for guest {}", intent.guest_id);
            eprintln!("  disk: {}", intent.disk_path());
            eprintln!(
                "  seed: {}",
                if intent.has_seed() {
                    intent.seed_path()
                } else {
                    "(none)"
                }
            );

            let base_run_dir = swift_runtime::base_run_dir();
            let runtime_dir =
                match swift_runtime::create_runtime_dir(&intent.guest_id, &base_run_dir) {
                    Ok(rt) => rt,
                    Err(e) => {
                        eprintln!("swiftletd: failed to create runtime dir: {}", e);
                        std::process::exit(1);
                    }
                };
            eprintln!("swiftletd: runtime dir at {}", runtime_dir.root().display());

            if intent.has_seed() {
                let configmap_path = Path::new(intent.seed_path());
                let nocloud_output = runtime_dir.seed_dir();
                if let Err(e) = swift_seed::build_nocloud_dir(configmap_path, &nocloud_output) {
                    eprintln!("swiftletd: failed to build NoCloud dir: {}", e);
                    std::process::exit(1);
                }
                eprintln!(
                    "swiftletd: built NoCloud dir at {}",
                    nocloud_output.display()
                );
                // CH expects disk image (ISO), not directory. Create seed.iso for cloud-init.
                let seed_iso = runtime_dir.root().join("seed.iso");
                if let Err(e) = create_seed_iso(&nocloud_output, &seed_iso) {
                    eprintln!("swiftletd: failed to create seed ISO: {}", e);
                    std::process::exit(1);
                }
                eprintln!("swiftletd: created seed ISO at {}", seed_iso.display());
            }

            let rt = Arc::new(
                tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("tokio runtime"),
            );

            let (namespace, name) = (env::var("POD_NAMESPACE").ok(), env::var("POD_NAME").ok());

            let report_running = |running: bool, reason: Option<&str>| {
                let (Some(ns), Some(n)) = (&namespace, &name) else {
                    return;
                };
                rt.block_on(async {
                    let client = match kube_client::create_client().await {
                        Ok(c) => c,
                        Err(e) => {
                            eprintln!(
                                "swiftletd: kube client unavailable ({}), skipping report",
                                e
                            );
                            return;
                        }
                    };
                    if let Err(e) =
                        report::report_guest_running(&client, ns, n, running, reason).await
                    {
                        eprintln!("swiftletd: failed to report status: {}", e);
                    }
                });
            };

            if intent.lifecycle == "stop" {
                eprintln!("swiftletd: lifecycle=stop, skipping launch");
                report_running(false, Some("VmStopped"));
                return;
            }

            if intent.has_network() {
                if let (Some(ref ns), Some(ref n)) = (&namespace, &name) {
                    let lease_path = runtime_dir.root().join("dnsmasq.leases");
                    lease::spawn_lease_poller(lease_path, ns.clone(), n.clone());
                }
            }

            let on_socket_ready = namespace.as_ref().zip(name.as_ref()).map(|_| {
                let ns = namespace.clone().unwrap();
                let name = name.clone().unwrap();
                let rt_clone = Arc::clone(&rt);
                move || {
                    rt_clone.block_on(async {
                        let client = match kube_client::create_client().await {
                            Ok(c) => c,
                            Err(e) => {
                                eprintln!(
                                    "swiftletd: kube client unavailable ({}), skipping report",
                                    e
                                );
                                return;
                            }
                        };
                        if let Err(e) =
                            report::report_guest_running(&client, &ns, &name, true, None).await
                        {
                            eprintln!("swiftletd: failed to report running: {}", e);
                        }
                    });
                }
            });

            match launch::run(&intent, &runtime_dir, on_socket_ready) {
                Ok(exit_status) => {
                    if exit_status.success() {
                        eprintln!("swiftletd: VM stopped gracefully");
                        report_running(false, Some("VmStopped"));
                    } else {
                        eprintln!("swiftletd: VM exited with code {:?}", exit_status.code());
                        report_running(false, Some("VmFailed"));
                        std::process::exit(1);
                    }
                }
                Err(e) => {
                    eprintln!("swiftletd: launch error: {}", e);
                    report_running(false, Some("VmFailed"));
                    std::process::exit(1);
                }
            }
        }
        Err(e) => {
            eprintln!("swiftletd: error: {}", e);
            std::process::exit(1);
        }
    }
}

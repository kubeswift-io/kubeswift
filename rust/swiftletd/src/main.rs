mod action;
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
    env_logger::Builder::from_env(env_logger::Env::default().default_filter_or("info"))
        .format_timestamp_millis()
        .init();

    if env::args().any(|a| a == "--version" || a == "-V") {
        log::info!("swiftletd {} (git {})", VERSION, GIT_COMMIT);
        std::process::exit(0);
    }

    let intent_path =
        env::var("KUBESWIFT_INTENT_PATH").unwrap_or_else(|_| intent::INTENT_PATH.to_string());

    // Live-migration role (Phase 2). KUBESWIFT_MIGRATION_ROLE=receiver
    // signals destination-receive mode; absent / any other value means
    // normal startup (source-side or unrelated to migration). See
    // `docs/design/live-migration-phase-2.md` §4.3.2 for why this is an
    // env var rather than a field in the intent JSON: keeps the intent
    // JSON shape unchanged and isolates the receiver branch from the
    // Phase 1 pod-builder logic.
    let migration_role = env::var("KUBESWIFT_MIGRATION_ROLE")
        .ok()
        .filter(|v| !v.is_empty());

    match intent::load_intent(&intent_path) {
        Ok(mut intent) => {
            if let Some(ref role) = migration_role {
                intent.migration = Some(intent::MigrationIntent { role: role.clone() });
                log::info!("migration_role role={}", role);
            }
            if intent.has_kernel() {
                let kb = intent.kernel_boot.as_ref().unwrap();
                log::info!(
                    "intent_loaded guest_id={} kernel={} initramfs={}",
                    intent.guest_id,
                    kb.kernel_path,
                    kb.initramfs_path
                );
            } else {
                let seed_val = if intent.has_seed() {
                    intent.seed_path()
                } else {
                    "(none)"
                };
                log::info!(
                    "intent_loaded guest_id={} disk={} seed={}",
                    intent.guest_id,
                    intent.disk_path(),
                    seed_val
                );
            }

            let base_run_dir = swift_runtime::base_run_dir();
            let runtime_dir =
                match swift_runtime::create_runtime_dir(&intent.guest_id, &base_run_dir) {
                    Ok(rt) => rt,
                    Err(e) => {
                        log::error!("failed to create runtime dir: {}", e);
                        std::process::exit(1);
                    }
                };
            log::info!("runtime_dir path={}", runtime_dir.root().display());

            // Seed.iso construction.
            //
            // Fresh boot: cloud-init reads the seed.iso at first wake to
            // configure the VM. Required path.
            //
            // Restore-receive (Phase 2): cloud-init has already run, but
            // CH on `--restore` re-opens every disk listed in the
            // snapshot's config.json — and the snapshot's config.json
            // still references the seed.iso disk path. CH bails with
            // "no such file or directory" if the file is missing.
            //
            // The build is deterministic and idempotent, so we run it
            // for both fresh boot and restore-receive whenever the
            // intent carries a seed. For in-place restore, the
            // runtime_dir name matches the source pod's, so the
            // rebuilt seed.iso lands at the same path config.json
            // expects. Clone restores have a different runtime_dir
            // and need a config.json disk-path patch in addition
            // (Phase 2 follow-up).
            if intent.is_restore() {
                log::info!(
                    "restore_receive_mode snapshot_path={}",
                    intent.restore_snapshot_path()
                );
            }
            if intent.is_migration_receiver() {
                // Phase 2 receiver mode: launch.rs will spawn CH with
                // --api-socket only; the action loop dispatches
                // vm.receive-migration over the API socket. Disk paths
                // listed in the source's migrated config.json must
                // exist on this dst pod at the same paths — the
                // hand-rolled launcher pod YAML mounts the same PVCs
                // and the seed.iso below is rebuilt for path-equality
                // (CH re-opens every disk listed in config.json on
                // receive completion). See
                // `docs/design/live-migration-phase-2.md` §4.3.2.
                log::info!("migration_receiver_mode");
            }
            if intent.has_seed() && !intent.has_kernel() {
                let configmap_path = Path::new(intent.seed_path());
                let nocloud_output = runtime_dir.seed_dir();
                if let Err(e) = swift_seed::build_nocloud_dir(configmap_path, &nocloud_output) {
                    log::error!("failed to build NoCloud dir: {}", e);
                    std::process::exit(1);
                }
                log::info!("nocloud_built path={}", nocloud_output.display());
                // CH expects disk image (ISO), not directory. Create seed.iso for cloud-init.
                let seed_iso = runtime_dir.root().join("seed.iso");
                if let Err(e) = create_seed_iso(&nocloud_output, &seed_iso) {
                    log::error!("failed to create seed ISO: {}", e);
                    std::process::exit(1);
                }
                log::info!("seed_iso_created path={}", seed_iso.display());
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
                            log::warn!("kube client unavailable ({}), skipping report", e);
                            return;
                        }
                    };
                    if let Err(e) =
                        report::report_guest_running(&client, ns, n, running, reason).await
                    {
                        log::error!("report_failed: {}", e);
                    }
                });
            };

            if intent.lifecycle == "stop" {
                log::info!("lifecycle=stop, skipping launch");
                report_running(false, Some("VmStopped"));
                return;
            }

            // Lease poller: skipped in receiver mode. The migrated
            // guest's DHCP state arrives over the migration wire as
            // part of the virtio-net device state; the local
            // dnsmasq on the dst node will not re-issue a lease
            // mid-migration (and even if it does, mutating the
            // launcher pod's status annotation while migration is
            // in flight would race the action handler's status
            // writes). A future PR-D may add an
            // on-receive-completion lease report driven by the
            // action loop. See `docs/design/live-migration-phase-2.md`
            // §4.3.2 + §3.4 (poll-info-API).
            if intent.has_network() && !intent.is_migration_receiver() {
                if let (Some(ref ns), Some(ref n)) = (&namespace, &name) {
                    let lease_path = runtime_dir.root().join("dnsmasq.leases");
                    let nics_for_poller = intent.nics.clone();
                    lease::spawn_lease_poller(lease_path, ns.clone(), n.clone(), nics_for_poller);
                }
            }

            // Snapshot/restore action handler. Phase 2 commit 5: skeleton
            // only — handlers are no-ops; commits 6 and 7 wire in the
            // pause/snapshot/resume and restore-prepare flows. Spawning
            // it here means it's running before launch::run blocks, so
            // the controller can drive it as soon as the launcher pod is
            // up (it does not require the VM to have already booted).
            if let (Some(ref ns), Some(ref n)) = (&namespace, &name) {
                let api_socket = runtime_dir.root().join("ch.sock");
                action::spawn_action_loop(ns.clone(), n.clone(), api_socket);
            }

            // on_socket_ready: skipped in receiver mode. CH on a
            // receiver pod has no VM at socket-ready time (it's an
            // empty VMM awaiting receive-migration); reporting
            // "guest running" here would be a lie. The action loop's
            // dispatch_migration_receive path verifies vm_info
            // state=Running post-receive (W1 gate from PR-B); a
            // future follow-up may add an
            // on-migration-receive-success runtime report. For PR-C
            // operators verify destination success via the
            // migration-status: ready annotation OR via vm_info
            // directly.
            let on_socket_ready = if intent.is_migration_receiver() {
                None
            } else {
                namespace.as_ref().zip(name.as_ref()).map(|_| {
                    let ns = namespace.clone().unwrap();
                    let name = name.clone().unwrap();
                    let rt_clone = Arc::clone(&rt);
                    move |pid: u32, serial_socket_path: String, hypervisor: String| {
                        log::info!(
                            "socket_ready pid={} serial={} hypervisor={}",
                            pid,
                            serial_socket_path,
                            hypervisor
                        );
                        rt_clone.block_on(async {
                            let client = match kube_client::create_client().await {
                                Ok(c) => c,
                                Err(e) => {
                                    log::warn!("kube client unavailable ({}), skipping report", e);
                                    return;
                                }
                            };
                            if let Err(e) =
                                report::report_guest_running(&client, &ns, &name, true, None).await
                            {
                                log::error!("report_running_failed: {}", e);
                            } else {
                                log::info!("guest_running_reported");
                            }
                            if let Err(e) = report::report_guest_runtime(
                                &client,
                                &ns,
                                &name,
                                pid,
                                serial_socket_path.as_str(),
                                hypervisor.as_str(),
                            )
                            .await
                            {
                                log::error!("report_runtime_failed: {}", e);
                            } else {
                                log::info!("guest_runtime_reported");
                            }
                        });
                    }
                })
            };

            // Phase 3a D2 (`docs/design/live-migration-phase-3a.md` §7.2):
            // on abnormal exit while we were a migration receiver, write
            // `migration-status: failed` paired with the last-observed
            // action-id so the controller doesn't stall on a never-
            // arriving terminal status. The watchdog's `decide_watchdog`
            // is the canonical write-once guard for the D1+D2 race.
            let watchdog_dst = intent.is_migration_receiver();
            let result = launch::run(&intent, &runtime_dir, on_socket_ready);
            let abnormal_exit_detail: Option<String> = match &result {
                Ok((exit_status, _, _)) if !exit_status.success() => {
                    Some(format!("vm_exited_nonzero code={:?}", exit_status.code()))
                }
                Ok(_) => None,
                Err(e) => Some(format!("launch_error: {}", e)),
            };
            if watchdog_dst {
                if let (Some(detail), Some(ref ns), Some(ref n)) =
                    (&abnormal_exit_detail, &namespace, &name)
                {
                    rt.block_on(action::write_migration_failed_on_abnormal_exit(
                        ns, n, detail,
                    ));
                }
            }
            match result {
                Ok((exit_status, _pid, _serial_socket_path)) => {
                    if exit_status.success() {
                        log::info!("vm_stopped_gracefully");
                        report_running(false, Some("VmStopped"));
                    } else {
                        log::warn!("vm_exited_nonzero code={:?}", exit_status.code());
                        report_running(false, Some("VmFailed"));
                        std::process::exit(1);
                    }
                }
                Err(e) => {
                    log::error!("launch_error: {}", e);
                    report_running(false, Some("VmFailed"));
                    std::process::exit(1);
                }
            }
        }
        Err(e) => {
            log::error!("intent_load_error: {}", e);
            std::process::exit(1);
        }
    }
}

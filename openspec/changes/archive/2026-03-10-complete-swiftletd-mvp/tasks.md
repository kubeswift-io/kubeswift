## 1. swift-runtime directory setup

**Prerequisite:** implement-swiftletd-mvp (intent, paths), complete-swiftguest-controller (runtime intent format).

- [x] 1.1 Add rust/swift-runtime create_runtime_dir(guest_id, base_path) -> Path
- [x] 1.2 Create subdirs: seed/, socket path for CH
- [x] 1.3 Add base path from env (KUBESWIFT_RUN_DIR, default /var/lib/kubeswift/run)
- [x] 1.4 Add unit test for directory layout

## 2. swift-ch-client spawn and socket

- [x] 2.1 Add rust/swift-ch-client spawn_ch(config) -> Child (spawn cloud-hypervisor process)
- [x] 2.2 Build CH args from intent: --api-socket, --disk, --memory, --cpus
- [x] 2.3 Add connect(socket_path) -> Client (poll until socket exists, then connect)
- [x] 2.4 Ensure no TCP binding; Unix socket only
- [x] 2.5 Add shutdown_vm(client) or equivalent (optional for MVP)
- [x] 2.6 Document Cloud Hypervisor version compatibility

## 3. NoCloud output in runtime dir

- [x] 3.1 Update swiftletd main to pass runtime dir seed path to swift-seed (not hardcoded)
- [x] 3.2 Call build_nocloud_dir with output_path = runtime_dir/seed
- [x] 3.3 Handle seed media for CH: ISO or directory (document choice)

## 4. swiftletd main flow integration

- [x] 4.1 Add swiftletd dependency on swift-runtime, swift-ch-client
- [x] 4.2 Implement flow: read intent -> create runtime dir -> build NoCloud (if seed) -> spawn CH
- [x] 4.3 If lifecycle=stop: skip launch, report Stopped
- [x] 4.4 If lifecycle=start: spawn CH, wait on process

## 5. Process monitoring

- [x] 5.1 Add CH process wait (std::process::Child::wait or tokio equivalent)
- [x] 5.2 On exit 0: set state Stopped
- [x] 5.3 On exit non-zero: set state Failed
- [x] 5.4 Handle CH crash: reap process, set Failed

## 6. Status reporting

- [x] 6.1 Add rust/swiftletd report module with kube-rs (or similar) Kubernetes client
- [x] 6.2 Use in-cluster config from service account
- [x] 6.3 Implement patch SwiftGuest status (GuestRunning condition)
- [x] 6.4 Obtain SwiftGuest name/namespace from pod ownerReference or env
- [x] 6.5 On VM running: patch GuestRunning=True
- [x] 6.6 On VM stopped/failed: patch GuestRunning=False, set condition reason
- [x] 6.7 Add config/rbac for swiftletd (patch swiftguests/status)

## 7. Container and docs

- [x] 7.1 Add Dockerfile/Containerfile for swiftletd (multi-stage Rust build)
- [x] 7.2 Ensure Cloud Hypervisor binary in image
- [x] 7.3 Update pod spec to use swiftletd image (replace pause placeholder)
- [x] 7.4 Add docs/swiftletd-mvp.md describing flow, mount paths, CH requirements

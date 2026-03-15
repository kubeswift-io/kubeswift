## 1. Prerequisite: Cloud Hypervisor serial socket support

- [x] 1.1 Add `serial_socket_path: Option<String>` to `VmConfig` in rust/swift-ch-client/src/config.rs
- [x] 1.2 When `serial_socket_path` is `Some(path)`, append `--serial socket=<path>` and `--console off` to CH args in `to_args()`
- [x] 1.3 In rust/swiftletd launch flow, pass `serial_socket_path: Some(runtime_dir.root().join("serial.sock"))` when building VmConfig

## 2. Lifecycle commands

- [ ] 2.1 Add cobra to cmd/swiftctl; create root command with global flags `-n`, `--namespace`, `--kubeconfig`, `--context`
- [ ] 2.2 Add internal/cli/kubeconfig.go: load rest.Config from flags/env
- [ ] 2.3 Add internal/cli/guest.go: resolve SwiftGuest by name, get pod (status.podRef or label selector)
- [ ] 2.4 Implement `swiftctl start <guest>`: patch runPolicy=Running, delete pod if exists
- [ ] 2.5 Implement `swiftctl stop <guest>`: patch runPolicy=Stopped, delete pod
- [ ] 2.6 Implement `swiftctl restart <guest>`: delete pod (require runPolicy=Running; fail with clear message otherwise)

## 3. Console command

- [x] 3.1 Implement `swiftctl console <guest>`: resolve pod, exec `socat -,crnl UNIX-CONNECT:/var/lib/kubeswift/run/<namespace>-<name>/serial.sock` in launcher container with TTY
- [x] 3.2 Use exec.Stream with Stdin/Stdout/Tty for interactive access; handle SIGINT for clean exit
- [x] 3.3 Fail with clear errors when guest not found, pod not found, or guest phase not Running

## 4. Release integration

- [ ] 4.1 Ensure `make build-go` produces swiftctl (cmd/swiftctl under ./cmd/...); add `swiftctl --version` using internal/version
- [ ] 4.2 Add release-stable workflow step: build swiftctl for linux/amd64 (and optionally darwin/arm64), attach binaries to GitHub Release
- [ ] 4.3 Update docs/releases.md: add swiftctl section (how to obtain, version stamping)

## 5. Documentation

- [ ] 5.1 Add docs/swiftctl.md: command reference, SwiftGuest/pod operations per command, console transport (exec + tail)
- [ ] 5.2 Add swiftctl to docs index or install docs if applicable

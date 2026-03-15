## 1. API and CRD

- [x] 1.1 Add `GuestNetworkStatus` struct (PrimaryIP, Interface, Ready) and `Network *GuestNetworkStatus` to SwiftGuestStatus in api/swift/v1alpha1/swiftguest_types.go
- [x] 1.2 Run `make generate` to regenerate CRD manifests

## 2. Runtime Intent

- [x] 2.1 Add `Network bool` to RuntimeIntent in internal/runtimeintent/types.go
- [x] 2.2 In internal/runtimeintent/build.go, set Network: rg.HasSeed() when building RuntimeIntent
- [x] 2.3 Add HasNetwork() to ResolvedGuest interface if needed; ResolvedGuest already has HasSeed()

## 3. swift-ch-client

- [x] 3.1 Add `tap_name: Option<String>` to VmConfig in rust/swift-ch-client/src/config.rs
- [x] 3.2 In to_args(), when tap_name is Some, append `--net tap=<name>` to CH args

## 4. swiftletd Intent and Launch

- [x] 4.1 Add `network: Option<bool>` to RuntimeIntent in rust/swiftletd/src/intent.rs (default true when seed present)
- [x] 4.2 When intent.has_network(), pass tap_name: Some("tap0") to VmConfig in rust/swiftletd/src/launch.rs

## 5. Network Setup Scripts and Image

- [x] 5.1 Create images/swiftletd/scripts/network-init.sh: create br0, add eth0 to br0, create tap0, add tap0 to br0
- [x] 5.2 Create images/swiftletd/scripts/launcher-entrypoint.sh: when network enabled, create runtime dir, start dnsmasq, exec swiftletd; else exec swiftletd
- [x] 5.3 Update images/swiftletd/Containerfile: add iproute2, bridge-utils, dnsmasq; use launcher-entrypoint.sh as entrypoint

## 6. swiftletd IP Discovery and Pod Annotation

- [x] 6.1 Implement DHCP lease file polling in swiftletd: read /var/lib/kubeswift/run/<guest-id>/dnsmasq.leases, extract VM IP by MAC
- [x] 6.2 When IP discovered, patch pod annotation kubeswift.io/guest-ip via kube-rs
- [x] 6.3 Add RBAC for launcher ServiceAccount to patch pods (config/rbac/)

## 7. Controller: Init Container and Pod

- [x] 7.1 Add network-init init container to BuildPod when rg.HasSeed() in internal/controller/swiftguest/pod.go
- [x] 7.2 Init container uses same launcher image, runs network-init.sh; add securityContext with NET_ADMIN capability
- [x] 7.3 Init container only creates bridge and tap; launcher starts dnsmasq and creates runtime dir for lease file

## 8. Controller: Status from Pod Annotation

- [x] 8.1 Add PodAnnotationGuestIP constant in internal/controller/swiftguest/constants.go
- [x] 8.2 In MapPodToStatus (internal/controller/swiftguest/status.go), read pod.Annotations[PodAnnotationGuestIP] and set status.Network.PrimaryIP, Ready

## 9. Seed: Default Network-Config

- [x] 9.1 When rg.HasSeed() and networkData empty, pass default network-config to seed.BuildConfigMap in controller reconcile
- [x] 9.2 Default: `version: 2` with `ethernets: { eth0: { dhcp4: true } }` (or equivalent cloud-init netplan)

## 10. Samples and Documentation

- [x] 10.1 Create config/samples/swiftseedprofile-ssh.yaml with ssh_authorized_keys in userData
- [x] 10.2 Create docs/guest-networking-ssh.md: pod network model, SSH keys, IP discovery, operator workflow (create guest, kubectl get -o jsonpath, ssh)
- [x] 10.3 Update config/samples README or docs/first-boot.md with SSH workflow reference

## 11. Tests

- [x] 11.1 Add test: BuildPod includes network-init init container when HasSeed
- [x] 11.2 Add test: RuntimeIntent.Network true when HasSeed
- [x] 11.3 Document smoke test extension for SSH in docs/smoke-verification.md (or add follow-up task)
